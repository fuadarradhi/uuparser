// Package config memuat seluruh pengaturan aplikasi dari berkas .env.
//
// Urutan prioritas: variabel lingkungan proses > isi .env > nilai baku.
// Dengan begitu .env dipakai sehari-hari, sementara variabel lingkungan berguna
// untuk menimpa sekali jalan (mis. di systemd atau kontainer) tanpa menyunting
// berkas.
//
// Pengaturan sengaja dibatasi pada hal yang memang berbeda antar mesin atau
// antar sumber data. Parameter inferensi seperti suhu, top-k, dan ukuran
// konteks bersifat tetap di dalam kode.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config adalah seluruh pengaturan aplikasi.
type Config struct {
	// ---- sumber & jadwal ----
	Endpoints     []string
	DataDir       string
	Interval      time.Duration
	DelayMS       time.Duration
	MaxAttempts   int
	DownloadFirst bool

	// Limit & OnlyID diisi dari flag baris perintah, bukan .env: keduanya alat
	// uji sesaat. Bila disimpan di .env, nilai uji coba mudah tertinggal dan
	// membuat service produksi diam-diam hanya memproses sebagian dokumen.
	Limit  int
	OnlyID []string

	// ---- model (yzma / llama.cpp, di dalam proses) ----
	ModelPath  string
	MMProjPath string
	LibPath    string
	Verbose    bool

	// ---- log ----
	LogDir string

	// ---- prompt ----
	OCRPrompt    string
	OCRMaxTokens int

	// ---- render halaman ----
	DPI        int
	AutoCrop   bool
	ProbePages int
	BlankInk   float64
	SavePNG    bool
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
		if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
			return v // variabel lingkungan menimpa .env
		}
		if v, ok := fileVals[key]; ok && strings.TrimSpace(v) != "" {
			return v
		}
		return def
	}

	c := Config{
		Endpoints:     splitList(get("ENDPOINTS", "http://jdih.acehprov.go.id/integrasi")),
		DataDir:       get("DATA_DIR", "data"),
		Interval:      dur(get("INTERVAL", "60m"), 60*time.Minute),
		DelayMS:       time.Duration(num(get("DELAY_MS", "1500"), 1500)) * time.Millisecond,
		MaxAttempts:   num(get("MAX_ATTEMPTS", "3"), 3),
		DownloadFirst: boolean(get("DOWNLOAD_FIRST", "false")),

		// Bawaan mengikuti tata letak yang lazim: berkas berada di sebelah
		// binari, sehingga service dapat dijalankan tanpa menyunting apa pun.
		ModelPath:  get("MODEL_PATH", filepath.Join(cwd, "models", "ocr.gguf")),
		MMProjPath: get("MMPROJ_PATH", filepath.Join(cwd, "models", "ocr.mmproj.gguf")),
		LibPath:    get("LIB_PATH", filepath.Join(cwd, "libs")),
		Verbose:    boolean(get("VERBOSE", "false")),

		LogDir: get("LOG_DIR", filepath.Join(cwd, "log")),

		OCRPrompt:    get("OCR_PROMPT", "Text Recognition:"),
		OCRMaxTokens: num(get("OCR_MAX_TOKENS", "2048"), 2048),

		DPI:        num(get("DPI", "200"), 200),
		AutoCrop:   boolean(get("AUTO_CROP", "true")),
		ProbePages: num(get("PROBE_PAGES", "5"), 5),
		BlankInk:   flt(get("BLANK_INK", "0.0004"), 0.0004),
		SavePNG:    boolean(get("SAVE_PNG", "false")),
	}

	if len(c.Endpoints) == 0 {
		return c, fmt.Errorf("ENDPOINTS kosong")
	}
	if err := mustExist("MODEL_PATH", c.ModelPath, "berkas model GGUF"); err != nil {
		return c, err
	}
	if err := mustExist("MMPROJ_PATH", c.MMProjPath,
		"berkas proyektor multimodal (mmproj); tanpa ini gambar tidak dapat diproses"); err != nil {
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
		// Buang tanda kutip pembungkus bila ada; isi di dalamnya dipertahankan
		// apa adanya (penting untuk prompt yang mengandung spasi & tanda baca).
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		vals[key] = val
	}
	return vals, sc.Err()
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func num(s string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return v
	}
	return def
}

func flt(s string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return v
	}
	return def
}

func dur(s string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(strings.TrimSpace(s)); err == nil {
		return v
	}
	return def
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
