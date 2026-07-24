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

// TahunFilter adalah hasil urai env YEAR — operator perbandingan + nilai
// tahun. Op kosong berarti tanpa saringan sama sekali (lihat Aktif()).
type TahunFilter struct {
	Op    string // "", "=", ">=", "<=", ">", "<"
	Value int
}

// Aktif melaporkan apakah filter ini benar-benar menyaring sesuatu.
func (f TahunFilter) Aktif() bool { return f.Op != "" }

// Cocok membandingkan satu tahun dokumen terhadap filter ini. Hanya masuk
// akal dipanggil bila Aktif() true.
func (f TahunFilter) Cocok(tahun int) bool {
	switch f.Op {
	case "=":
		return tahun == f.Value
	case ">=":
		return tahun >= f.Value
	case "<=":
		return tahun <= f.Value
	case ">":
		return tahun > f.Value
	case "<":
		return tahun < f.Value
	default:
		return true // Op kosong = tidak menyaring apa pun
	}
}

// String untuk log/pesan (mis. "YEAR>=2023").
func (f TahunFilter) String() string {
	if !f.Aktif() {
		return "(tanpa saringan)"
	}
	return f.Op + strconv.Itoa(f.Value)
}

// parseTahunFilter mengurai isi env YEAR. Operator dikenali sebagai PREFIKS
// (urutan pemeriksaan sengaja ">="/"<=" dulu sebelum ">"/"<" tunggal, supaya
// tidak salah potong). Tanpa operator sama sekali -> ">=" (kompatibel dengan
// perilaku lama MIN_TAHUN, yang selalu berarti "minimal tahun ini"). String
// kosong atau angka tak valid -> filter tidak aktif (Op kosong).
func parseTahunFilter(raw string) TahunFilter {
	s := strings.TrimSpace(raw)
	if s == "" {
		return TahunFilter{}
	}
	op := ">="
	switch {
	case strings.HasPrefix(s, ">="):
		op, s = ">=", s[2:]
	case strings.HasPrefix(s, "<="):
		op, s = "<=", s[2:]
	case strings.HasPrefix(s, "="):
		op, s = "=", s[1:]
	case strings.HasPrefix(s, ">"):
		op, s = ">", s[1:]
	case strings.HasPrefix(s, "<"):
		op, s = "<", s[1:]
	}
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || v <= 0 {
		return TahunFilter{}
	}
	return TahunFilter{Op: op, Value: v}
}

// pageLimitNum mengurai MAX_PAGE/MIN_PAGE — BEDA dari num() karena di sini
// 0 adalah nilai SAH (berarti "tanpa batas"/"tanpa minimum"), bukan sinyal
// "pakai nilai bawaan". Hanya string kosong/tak-numerik/negatif yang jatuh
// ke nilai bawaan.
func pageLimitNum(s string, def int) int {
	t := strings.TrimSpace(s)
	if t == "" {
		return def
	}
	v, err := strconv.Atoi(t)
	if err != nil || v < 0 {
		return def
	}
	return v
}

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

	// Tahun menyaring dokumen SAAT DIDAFTARKAN dari sumber, berdasarkan
	// sort_tahun (metadata JDIH, HANYA untuk urutan — lihat
	// downloader.RemoteDoc). Menggantikan MIN_TAHUN (2026-07-23): dulu hanya
	// bisa ">=", sekarang mendukung operator eksplisit lewat env YEAR, mis.
	// "=2023" (persis tahun itu saja), ">=2023" (minimal), ">2020", "<=2019",
	// "<2019". Tanpa operator (mis. "2023" polos) diperlakukan sebagai ">="
	// untuk kompatibilitas dengan perilaku lama. String kosong (bawaan) =
	// tanpa saringan sama sekali.
	//
	// Ini KEBALIKAN dari filosofi "tombol yang jawabannya sudah pasti tidak
	// perlu jadi .env": jawabannya justru BELUM pasti secara sengaja — dipakai
	// untuk memperkecil cakupan uji coba parser (mis. YEAR=>=2020 dulu,
	// diperbesar bertahap) sambil mengamati tahun berapa parser sudah bagus.
	//
	// Saat filter aktif (Tahun.Aktif()): dokumen TANPA sort_tahun (metadata
	// sumber tak menyediakannya) IKUT disaring — tidak didaftarkan. Permintaan
	// user: kalau YEAR diisi, harus benar-benar ada tahun yang memenuhi,
	// bukan lolos karena tidak diketahui. Hanya saat YEAR kosong (tanpa
	// saringan sama sekali) dokumen tanpa tahun boleh masuk.
	Tahun TahunFilter

	// MaxPage (konsep diubah 2026-07-24 — SEBELUMNYA memotong tiap dokumen
	// jadi hanya MaxPage halaman pertama; SEKARANG sebuah SARINGAN
	// ANTRIAN): dokumen yang jumlah halaman ASLINYA (documents.total_pages,
	// dicatat sekali saat unduh) melebihi MaxPage TIDAK PERNAH diambil untuk
	// OCR sama sekali selama MaxPage masih berlaku — bukan diproses
	// sebagian. Begitu sebuah dokumen lolos dan diambil, ia SELALU diproses
	// utuh sampai halaman terakhir (lihat store.ClaimForOCR).
	//
	// Antrian juga diurutkan berdasar total_pages MENAIK (dokumen PENDEK
	// duluan) — permintaan eksplisit user supaya iterasi uji parser tidak
	// tersandera satu peraturan tebal.
	//
	// Dokumen dengan total_pages belum diketahui (NULL — penghitungan saat
	// unduh gagal) TETAP diambil seperti biasa, tidak ikut disaring.
	//
	// SENGAJA masih pengaturan sementara khusus masa debug: 0 berarti TANPA
	// saringan sama sekali (matikan setelah masa debug selesai). Bawaan: 5.
	MaxPage int

	// MinPage (konsep dikoreksi 2026-07-24 — SEBELUMNYA dokumen di bawah
	// jumlah halaman ini ditolak LANGSUNG sebagai "bukan peraturan" TANPA
	// di-OCR sama sekali; SALAH, sudah dihapus). Sekarang: jendela BERAPA
	// HALAMAN yang boleh dicoba classify() (lihat ocr_worker.go) sebelum
	// BENAR-BENAR menyerah menolak dokumen. Halaman pertama yang gagal
	// (mis. cuma sampul/judul tanpa Menimbang/Mengingat) TIDAK langsung
	// berarti dokumennya bukan peraturan — bisa saja konsideransnya ada di
	// halaman 2. Selama window belum habis DAN masih ada halaman lain,
	// classify mencoba dulu halaman berikutnya sebelum menolak.
	//
	// Dokumen yang jumlah halaman ASLINYA lebih pendek dari MinPage (
	// TERMASUK yang cuma 1 halaman) TETAP diproses & diklasifikasi APA
	// ADANYA dari halaman yang tersedia — window otomatis tidak menunggu
	// halaman yang tidak ada. 0 berarti keputusan diambil dari HALAMAN
	// PERTAMA yang dicoba saja (tanpa menunggu halaman lain). Bawaan: 2.
	MinPage int

	// TextCheck (2026-07-24): sebelum OCR penuh, coba dulu halaman 1 (lalu 2
	// bila 1 tak cocok) — bandingkan hasil OCR dengan lapisan teks PDF
	// (`pdftotext`, poppler-utils) untuk halaman yang sama (lihat
	// internal/textcheck). Cocok -> SISA dokumen diisi dari pdftotext, jauh
	// lebih murah (tanpa model visi sama sekali) DAN TANPA batas MaxPage
	// (poppler tidak dibatasi MAX_PAGE, beda dari mode OCR). Tidak cocok di
	// kedua halaman -> OCR biasa seperti sebelum fitur ini ada (dibatasi
	// MaxPage bila diset).
	//
	// Otomatis nonaktif (walau TEXT_CHECK=true) bila biner pdftotext tidak
	// ditemukan di PATH — lihat textcheck.Available(). Bawaan: true.
	TextCheck bool

	// CheapTier (2026-07-24): begitu suatu halaman terdeteksi memuat awal
	// blok PENJELASAN atau LAMPIRAN (lihat parser.HasPenjelasanAnchor/
	// HasLampiranAnchor), halaman-halaman SELANJUTNYA tidak lagi wajib
	// lewat model visi (GLM-OCR) — coba pdftotext dulu, baru Tesseract
	// (tier "penjelasan") atau GLM-OCR (tier "lampiran", karena sering
	// berisi tabel/peta yang tetap butuh model visi) — lihat
	// internal/pipeline/tier.go. Permintaan user: data di bagian ini
	// sekunder, boleh lebih murah daripada batang tubuh.
	//
	// Butuh TEXT_CHECK juga aktif secara EFEKTIF (pdftotext terpasang) agar
	// bermanfaat penuh; tanpa tesseract terpasang, tier "penjelasan" jatuh
	// ke GLM-OCR sebagai jaring pengaman (bukan kehilangan data). Bawaan:
	// true.
	CheapTier bool

	// TesseractLang: kode bahasa untuk `tesseract -l <kode>` (lihat
	// internal/textcheck.RunTesseract). PASTIKAN paket bahasa yang sesuai
	// terpasang (mis. tesseract-ocr-ind untuk "ind") — traineddata "eng"
	// bawaan akan mengacak-acak istilah/imbuhan bahasa Indonesia. Bawaan:
	// "ind".
	TesseractLang string

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

		PromptDir:     get("PROMPT_DIR", filepath.Join(cwd, "prompts")),
		ChatTemplate:  get("CHAT_TEMPLATE", ""),
		LowMemory:     boolean(get("LOW_MEMORY", "false")),
		DPIJelas:      num(get("DPI_SHARP", "100"), 100),
		DPISedang:     num(get("DPI_MEDIUM", "150"), 150),
		DPIBlur:       num(get("DPI_BLUR", "200"), 200),
		AmbangJelas:   floatNum(get("BLUR_THRESHOLD_SHARP", "5e8"), 5e8),
		AmbangSedang:  floatNum(get("BLUR_THRESHOLD_MEDIUM", "5e7"), 5e7),
		Tahun:         parseTahunFilter(get("YEAR", "")),
		MaxPage:       pageLimitNum(get("MAX_PAGE", "5"), 5),
		MinPage:       pageLimitNum(get("MIN_PAGE", "2"), 2),
		TextCheck:     boolean(get("TEXT_CHECK", "true")),
		CheapTier:     boolean(get("CHEAP_TIER", "true")),
		TesseractLang: get("TESSERACT_LANG", "ind"),
		DebugResult:   boolean(get("DEBUG_RESULT", "false")),
		DebugDir:      get("DEBUG_DIR", "debug"),

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
