// Command uuparser adalah service yang menjaga agar setiap peraturan baru pada
// sumber JDIH terdaftar otomatis terunduh, di-OCR, diperbaiki, lalu diurai
// menjadi baris siap ditinjau di database Postgres.
//
// Tidak ada flag CLI: konfigurasi hanya lewat .env. Aksi admin (reset/hapus
// tanda duplikat/parse ulang) dilakukan lewat SQL langsung — lihat
// CATATAN-MIGRASI.md.
//
// Pola main() -> run() int dipakai supaya SEMUA defer tetap dijalankan
// sebelum proses keluar (os.Exit di tengah main melewati defer).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/fuadarradhi/uuparser/internal/config"
	"github.com/fuadarradhi/uuparser/internal/localllm"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/pipeline"
	"github.com/fuadarradhi/uuparser/internal/prompts"
	"github.com/fuadarradhi/uuparser/internal/store"
)

func main() { os.Exit(run()) }

// warmup memuat kedua model di muka dan melaporkannya ke konsol.
func warmup(cfg config.Config, vision *localllm.Client, text *localllm.TextClient) error {
	logx.Banner("memuat model OCR  : %s", cfg.ModelPath)
	if err := vision.Warmup(); err != nil {
		return fmt.Errorf("memuat model OCR: %w", err)
	}
	if cfg.LowMemory {
		vision.Release()
	}

	logx.Banner("memuat model teks : %s", cfg.ThinkingPath)
	if err := text.Warmup(); err != nil {
		return fmt.Errorf("memuat model teks: %w", err)
	}
	if cfg.LowMemory {
		text.Release()
	}

	logx.Banner("kedua model siap.")
	return nil
}

func run() int {
	cfg, err := config.Load(".env")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error konfigurasi:", err)
		return 1
	}
	if err := logx.Init(cfg.LogDir); err != nil {
		fmt.Fprintln(os.Stderr, "error log:", err)
		return 1
	}
	defer logx.Close()

	// Prompt dibaca dari disk saat start supaya dapat disunting tanpa
	// membangun ulang binari; sidik jarinya ikut disimpan per halaman.
	pset, err := prompts.Load(cfg.PromptDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error prompt:", err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Sinyal berhenti: yang PERTAMA meminta berhenti dengan tertib, yang
	// KEDUA keluar seketika.
	//
	// Keluar seketika perlu karena inferensi model adalah satu panggilan
	// panjang yang tidak dapat disela di tengah jalan: penyandian gambar
	// halaman besar bisa memakan beberapa menit, dan selama itu pembatalan
	// konteks belum berpengaruh. Tanpa jalan keluar kedua, Ctrl+C terasa
	// tidak berfungsi. Progres tetap aman karena setiap halaman disimpan ke
	// basis data begitu selesai.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		logx.FinishProgress()
		fmt.Fprintln(os.Stderr, "berhenti… menunggu pekerjaan berjalan selesai "+
			"(tekan Ctrl+C sekali lagi untuk keluar seketika)")
		cancel()

		<-sig
		logx.FinishProgress()
		fmt.Fprintln(os.Stderr, "keluar paksa — progres tersimpan sampai halaman terakhir yang selesai")
		logx.Close()
		os.Exit(130) // 128 + SIGINT, lazimnya untuk penghentian oleh sinyal
	}()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logx.Fatal("", "membuka database: %v", err)
		return 1
	}
	defer st.Close()

	vision, err := localllm.New(localllm.Config{
		ModelPath: cfg.ModelPath, MMProjPath: cfg.MMProjPath,
		LibPath: cfg.LibPath, Verbose: cfg.Verbose,
		ChatTemplate: cfg.ChatTemplate,
	})
	if err != nil {
		logx.Fatal("", "menyiapkan model OCR: %v", err)
		return 1
	}
	// TIDAK ada defer Release di sini dengan sengaja: Release menunggu kunci
	// yang sedang dipegang inferensi yang berjalan, sehingga proses tertahan
	// sampai inferensi selesai — persis yang membuat Ctrl+C tampak mati.
	// Pada saat proses berakhir, sistem operasi yang membebaskan memorinya.

	text, err := localllm.NewText(localllm.TextConfig{
		ModelPath: cfg.ThinkingPath, LibPath: cfg.LibPath, Verbose: cfg.Verbose,
		ChatTemplate: cfg.ChatTemplate,
	})
	if err != nil {
		logx.Fatal("", "menyiapkan model teks: %v", err)
		return 1
	}
	// Sama seperti model OCR: tidak dilepas di sini (lihat catatan di atas).

	// Muat KEDUA model sekarang, sebelum pekerjaan dimulai. Seluruh waktu
	// tunggu terjadi di awal — saat pemakai memang sedang menunggu — bukan
	// mendadak di tengah pekerjaan yang membuat prosesnya tampak berhenti.
	//
	// Pada mode hemat memori keduanya tidak boleh menempati memori
	// bersamaan, jadi masing-masing dimuat lalu dilepas: pemuatan awal ini
	// tetap berguna sebagai pemeriksaan bahwa kedua berkas model memang
	// dapat dipakai, sebelum satu dokumen pun tersentuh.
	if err := warmup(cfg, vision, text); err != nil {
		logx.Fatal("", "%v", err)
		return 1
	}

	pipeline.Run(ctx, pipeline.Deps{
		Store: st, Vision: vision, Text: text,
		Prompts: pset, DataDir: cfg.DataDir, DPI: cfg.DPI,
		LowMemory: cfg.LowMemory,
	})
	return 0
}
