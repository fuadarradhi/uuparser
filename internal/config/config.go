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

	// DPI render halaman sebelum OCR. Satu-satunya parameter render yang
	// dapat diubah: nilai terbaiknya bergantung mutu pindaian korpus Anda
	// dan hanya bisa ditentukan dengan mengukur (lihat CATATAN-MIGRASI.md).
	DPI int

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

		PromptDir: get("PROMPT_DIR", filepath.Join(cwd, "prompts")),
		DPI:       num(get("DPI", "200"), 200),

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
