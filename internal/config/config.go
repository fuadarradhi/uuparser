// Package config memuat pengaturan yang BENAR-BENAR berbeda antar instalasi:
// lokasi database, lokasi berkas model, dan folder data/log. Parameter yang
// sudah punya jawaban jelas (prompt OCR, DPI, ambang halaman-kosong, jeda
// sopan ke server JDIH, batas percobaan) SENGAJA dijadikan konstanta di kode
// (lihat internal/extractor dan main.go), bukan kunci .env — permintaan
// eksplisit user (2026-07-20): tombol yang jawabannya sudah pasti hanya
// menambah risiko salah-ubah tanpa manfaat.
//
// Urutan prioritas: variabel lingkungan proses > isi .env > nilai baku.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config adalah seluruh pengaturan aplikasi — HANYA hal yang berbeda per mesin.
type Config struct {
	// DatabaseURL: connection string Postgres PRODUKSI (lihat schema.sql).
	DatabaseURL string

	// DataDir: folder tempat PDF disimpan (satu-satunya berkas yang masih
	// hidup di disk selain log — lihat CATATAN-MIGRASI.md).
	DataDir string

	// ---- model (yzma / llama.cpp, di dalam proses) ----
	ModelPath    string // model OCR (visi)
	MMProjPath   string // proyektor multimodal untuk model visi
	ThinkingPath string // model teks: klasifikasi halaman 1 + perbaikan salah ketik
	LibPath      string
	Verbose      bool

	// PromptDir: folder berisi classify.md & fix_page.md (dapat disunting
	// tanpa membangun ulang binari).
	PromptDir string

	// LowMemory membuat kedua model BERGANTIAN menempati memori alih-alih
	// hidup berdampingan: model visi dilepas sebelum model teks dipakai, dan
	// sebaliknya. Perlu bila jumlah kedua model melebihi RAM yang tersedia —
	// gejalanya proses berhenti mendadak tanpa pesan apa pun saat model
	// kedua dimuat (dihentikan paksa oleh sistem karena kehabisan memori).
	//
	// Harganya nyata: tiap halaman memuat ulang model, menambah puluhan
	// detik per halaman. Matikan begitu RAM mencukupi.
	LowMemory bool

	// ChatTemplate menimpa deteksi otomatis format percakapan. Biarkan
	// kosong pada keadaan normal — model GGUF biasanya menyertakan
	// templatnya sendiri. Isi hanya bila log memberi peringatan bahwa
	// template model tidak dikenali (mis. "gemma", "llama3", "chatml"),
	// karena memakai format yang salah menurunkan mutu jawaban tanpa
	// menimbulkan galat.
	ChatTemplate string

	// Render adaptif per halaman (2026-07-22): DPI dipilih otomatis lewat
	// skor ketajaman (varians Laplacian, lihat raster.blurScore), bukan satu
	// angka tetap untuk seluruh dokumen. Halaman JELAS dirender di DPIJelas
	// SEKALIGUS dipakai sebagai probe pengukur ketajaman — kalau skornya
	// sudah cukup, tidak ada render ulang (kasus paling murah & paling
	// sering: dokumen bersih). Halaman kurang tajam dirender ULANG di
	// DPISedang; yang benar-benar blur di DPIBlur.
	//
	// AmbangJelas/AmbangSedang HARUS dikalibrasi terhadap korpus Anda —
	// setiap halaman mencatat skor mentahnya ke log/info.log
	// ("blur_score=..."), jadi jalankan dulu dengan nilai bawaan, lihat
	// sebaran skornya di log, baru sesuaikan ambangnya. Angka bawaan di sini
	// sekadar titik awal, bukan hasil pengukuran — beda dari nilai bawaan
	// lain di berkas ini yang sudah pernah diuji.
	DPIJelas     int
	DPISedang    int
	DPIBlur      int
	AmbangJelas  float64
	AmbangSedang float64

	// MinTahun menyaring dokumen SAAT DIDAFTARKAN dari sumber: hanya yang
	// sort_tahun-nya (metadata JDIH, HANYA untuk urutan — lihat
	// downloader.RemoteDoc) >= MinTahun yang didaftarkan. 0 berarti tanpa
	// saringan. Ini KEBALIKAN dari filosofi "tombol yang jawabannya sudah
	// pasti tidak perlu jadi .env": jawabannya justru BELUM pasti secara
	// sengaja — dipakai untuk memperkecil cakupan uji coba parser
	// (mis. MIN_TAHUN=2020 dulu, diperbesar bertahap) sambil mengamati tahun
	// berapa parser sudah bagus dan tahun berapa masih perlu perbaikan.
	//
	// Saat MinTahun > 0: dokumen TANPA sort_tahun (metadata sumber tak
	// menyediakannya) IKUT disaring — tidak didaftarkan. Permintaan user:
	// kalau MIN_TAHUN diisi, harus benar-benar ada tahun yang memenuhi,
	// bukan lolos karena tidak diketahui. Hanya saat MinTahun == 0 (tanpa
	// saringan sama sekali) dokumen tanpa tahun boleh masuk.
	MinTahun int

	// DebugResult (2026-07-22): saat true, tiap dokumen menulis
	// <DebugDir>/<id>/ocr.txt + thinking.txt (jika ada panggilan model) +
	// parse.txt + parse_tree.json — sekadar mempermudah menyalin hasil
	// OCR/parse untuk dikirim ke Claude untuk dipelajari. TIDAK untuk
	// dinyalakan terus (menulis berkas tambahan per dokumen); nyalakan
	// sebentar saat memang perlu meninjau, matikan lagi sesudahnya.
	//
	// render.pdf DIHAPUS (2026-07-22) — sudah tidak diperlukan, ocr.txt
	// sudah cukup untuk peninjauan tanpa perlu render gambar per halaman.
	DebugResult bool

	// DebugDir (2026-07-22): folder TERPISAH dari DataDir — sengaja BUKAN
	// sub-folder data/ (yang seluruhnya di-gitignore, lihat .gitignore),
	// supaya isi debug BISA di-commit ke git (mis. untuk ditempelkan ke
	// percakapan dengan Claude tanpa perlu menyalin manual). Bawaan
	// "debug", sejajar dengan data/log/models/libs.
	DebugDir string

	// ---- log ----
	LogDir string
}

// Load membaca berkas .env (bila ada) lalu menyusun Config.
// path kosong berarti ".env" di direktori kerja.
func Load(path string) (Config, error) {
	if path == "" {
		path = ".env"
	}
	fileVals, err := parseEnvFile(path)
	if err != nil {
		return Config{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	get := func(key, def string) string {
		if v, ok := os.LookupEnv(key); ok {
			if t := strings.TrimSpace(v); t != "" {
				return t // variabel lingkungan menimpa .env (dipangkas — spasi liar bikin galat koneksi yang membingungkan)
			}
		}
		if v, ok := fileVals[key]; ok && strings.TrimSpace(v) != "" {
			return v
		}
		return def
	}

	c := Config{
		DatabaseURL: get("DATABASE_URL", ""),
		DataDir:     get("DATA_DIR", "data"),

		// Bawaan mengikuti tata letak yang lazim: berkas berada di sebelah
		// binari, sehingga service dapat dijalankan tanpa menyunting apa pun.
		ModelPath:    get("MODEL_PATH", filepath.Join(cwd, "models", "ocr.gguf")),
		MMProjPath:   get("MMPROJ_PATH", filepath.Join(cwd, "models", "ocr.mmproj.gguf")),
		ThinkingPath: get("THINKING_PATH", filepath.Join(cwd, "models", "thinking.gguf")),
		LibPath:      get("LIB_PATH", filepath.Join(cwd, "libs")),
		Verbose:      boolean(get("VERBOSE", "false")),

		PromptDir:    get("PROMPT_DIR", filepath.Join(cwd, "prompts")),
		ChatTemplate: get("CHAT_TEMPLATE", ""),
		LowMemory:    boolean(get("LOW_MEMORY", "false")),
		DPIJelas:     num(get("DPI_JELAS", "100"), 100),
		DPISedang:    num(get("DPI_SEDANG", "150"), 150),
		DPIBlur:      num(get("DPI_BLUR", "200"), 200),
		AmbangJelas:  floatNum(get("BLUR_AMBANG_JELAS", "5e8"), 5e8),
		AmbangSedang: floatNum(get("BLUR_AMBANG_SEDANG", "5e7"), 5e7),
		MinTahun:     num(get("MIN_TAHUN", "0"), 0),
		DebugResult:  boolean(get("DEBUG_RESULT", "false")),
		DebugDir:     get("DEBUG_DIR", "debug"),

		LogDir: get("LOG_DIR", filepath.Join(cwd, "log")),
	}

	if strings.TrimSpace(c.DatabaseURL) == "" {
		return c, fmt.Errorf("DATABASE_URL kosong — isi connection string Postgres " +
			"(mis. postgres://user:pass@localhost:5432/uuparser); jalankan schema.sql " +
			"terlebih dahulu bila database masih kosong")
	}
	if err := mustExist("MODEL_PATH", c.ModelPath, "berkas model GGUF"); err != nil {
		return c, err
	}
	if err := mustExist("MMPROJ_PATH", c.MMProjPath,
		"berkas proyektor multimodal (mmproj); tanpa ini gambar tidak dapat diproses"); err != nil {
		return c, err
	}
	if err := mustExist("THINKING_PATH", c.ThinkingPath,
		"berkas model teks GGUF; dipakai membaca halaman pertama dan memperbaiki salah ketik"); err != nil {
		return c, err
	}
	return c, nil
}

// parseEnvFile membaca berkas bergaya .env. Berkas yang tidak ada bukan galat —
// seluruh nilai baku tetap berlaku.
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vals := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		vals[key] = val
	}
	return vals, sc.Err()
}

func num(s string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// floatNum mem-parse angka pecahan dari env — dipakai untuk ambang skor blur
// (varians Laplacian), yang tidak masuk akal dibatasi ">0 saja seperti int
// (skor 0 itu sah: berarti halaman kosong/putih polos).
func floatNum(s string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return def
	}
	return v
}

func boolean(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
}

// mustExist memberi pesan yang menyebut kunci, jalur yang dicari, dan apa yang
// seharusnya ada di sana — supaya kesalahan pemasangan langsung jelas.
func mustExist(key, path, what string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%s kosong — isi jalur %s", key, what)
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s tidak ditemukan di %s — letakkan %s di sana, "+
			"atau tunjuk lokasinya lewat %s", key, path, what, key)
	}
	return nil
}
