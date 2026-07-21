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
	logx.Info("log galat: %s", logx.ErrorLogPath())

	// Prompt dibaca dari disk saat start supaya dapat disunting tanpa
	// membangun ulang binari; sidik jarinya ikut disimpan per halaman.
	pset, err := prompts.Load(cfg.PromptDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error prompt:", err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		logx.Info("sinyal berhenti diterima — progres di database aman")
		cancel()
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
	})
	if err != nil {
		logx.Fatal("", "menyiapkan model OCR: %v", err)
		return 1
	}
	defer vision.Release()

	text, err := localllm.NewText(localllm.TextConfig{
		ModelPath: cfg.ThinkingPath, LibPath: cfg.LibPath, Verbose: cfg.Verbose,
	})
	if err != nil {
		logx.Fatal("", "menyiapkan model teks: %v", err)
		return 1
	}
	defer text.Release()

	// Backend llama.cpp dipakai bersama kedua model, jadi hanya dibebaskan
	// di sini — setelah kedua Release di atas dijalankan.
	defer localllm.Shutdown()

	pipeline.Run(ctx, pipeline.Deps{
		Store: st, Vision: vision, Text: text,
		Prompts: pset, DataDir: cfg.DataDir, DPI: cfg.DPI,
	})
	return 0
}
