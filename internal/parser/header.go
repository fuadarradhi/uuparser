package parser

import (
	"regexp"
	"strconv"
	"strings"
)

// header.go: gerbang klasifikasi halaman-1 SAJA, menggantikan probe gate lama
// yang meng-OCR sampai PROBE_PAGES (default 5) halaman. Per Lampiran II
// UU 12/2011 jo. UU 13/2022 (Teknik Penyusunan Peraturan Perundang-undangan),
// identitas peraturan WAJIB ada di halaman pertama, muncul di DUA tempat:
// Judul (jenis + instansi + nomor + tahun + tentang) dan Jabatan Pembentuk di
// Pembukaan sebelum "Menimbang" (mis. "BUPATI ACEH BARAT,"). Regulasi tidak
// pakai halaman sampul terpisah, jadi cukup satu halaman untuk diperiksa.

// HeaderInfo adalah hasil ekstraksi deterministik dari halaman pertama.
type HeaderInfo struct {
	Jenis           string // "PERATURAN DAERAH", "QANUN", "PERATURAN BUPATI", dst
	Instansi        string // frasa instansi APA ADANYA dari teks (utuh, tak dipotong)
	Nomor           string
	Tahun           string
	Tentang         string
	StructureType   string // "pasal_ayat" | "diktum" | "unknown"
	Found           bool   // true bila pola judul/jabatan-pembentuk ketemu sama sekali
	PreUU122011     bool   // true bila Tahun < 2011 (aturan format Lampiran II belum berlaku)
}

// reHeaderJudul menangkap blok "<JENIS> <INSTANSI...> NOMOR <N> TAHUN <Y> TENTANG <T>".
// Instansi ditangkap NON-GREEDY sampai kata "NOMOR" — sengaja ambil frasa PENUH,
// bukan dibatasi jumlah kata, supaya "ACEH BARAT DAYA" tidak terpotong jadi
// "ACEH BARAT" (lihat diskusi soal nama kabupaten yang mirip tipis).
var reHeaderJudul = regexp.MustCompile(`(?is)(UNDANG-UNDANG|PERATURAN\s+PEMERINTAH\s+PENGGANTI\s+UNDANG-UNDANG|PERATURAN\s+PEMERINTAH|PERATURAN\s+PRESIDEN|PERATURAN\s+MENTERI|PERATURAN\s+DAERAH|PERATURAN\s+GUBERNUR|PERATURAN\s+BUPATI|PERATURAN\s+WALI\s*KOTA|PERATURAN\s+KEPALA\s+DAERAH|PERATURAN\s+DESA|QANUN)\s+([A-Z][A-Z\s]*?)\s*NOMOR\s+([0-9]+[A-Z]?)\s+TAHUN\s+([0-9]{4})\s+TENTANG\s+(.+)`)

// reJabatanPembentuk menangkap baris jabatan pembentuk di Pembukaan, mis.
// "BUPATI ACEH BARAT," atau "GUBERNUR ACEH,". Dipakai sebagai titik ekstraksi
// KEDUA (fallback) bila Judul rusak/OCR gagal tangkap — keduanya sama-sama
// wajib ada per Lampiran II, jadi salah satu cukup untuk validasi instansi.
var reJabatanPembentuk = regexp.MustCompile(`(?im)^(GUBERNUR|BUPATI|WALI\s*KOTA|PRESIDEN|MENTERI)\s+([A-Z][A-Z\s]*?)\s*,\s*$`)

// reDiktum menangkap format Keputusan: "KESATU", "KEDUA", dst (Diktum), bukan
// Pasal/Ayat. Deteksinya CUKUP satu diktum di halaman 1 karena Keputusan tidak
// pernah punya Pasal 1 di halaman pertama juga.
var reDiktum = regexp.MustCompile(`(?im)^\s*(KESATU|KEDUA|KETIGA)\s*:`)

// ExtractHeader membaca teks halaman PERTAMA (sudah di-OCR) dan mengekstrak
// identitas peraturan secara deterministik (regex, BUKAN LLM — lihat alasan
// yang sama seperti relations.go: LLM berisiko mengarang nomor/instansi).
func ExtractHeader(page1Text string) HeaderInfo {
	raw := page1Text

	var info HeaderInfo

	if m := reHeaderJudul.FindStringSubmatch(raw); m != nil {
		info.Found = true
		info.Jenis = normalizeSpace(m[1])
		info.Instansi = normalizeSpace(m[2])
		info.Nomor = strings.TrimSpace(m[3])
		info.Tahun = strings.TrimSpace(m[4])
		info.Tentang = strings.TrimSpace(firstLine(m[5]))
	} else if m := reJabatanPembentuk.FindStringSubmatch(raw); m != nil {
		// fallback: Judul tak tertangkap (rusak/OCR), tapi Jabatan Pembentuk ada.
		info.Found = true
		info.Instansi = normalizeSpace(m[2])
	}

	if y, err := strconv.Atoi(info.Tahun); err == nil && y > 0 && y < 2011 {
		info.PreUU122011 = true
	}

	// Deteksi jenis struktur pada teks MENTAH (dengan newline): kedua regex
	// di bawah ber-anchor awal-baris (?m)^ dan tidak akan pernah cocok pada
	// teks yang sudah diratakan jadi satu baris.
	switch {
	case reDiktum.MatchString(raw):
		info.StructureType = "diktum"
	case rePasalAnywhere.MatchString(raw):
		info.StructureType = "pasal_ayat"
	default:
		info.StructureType = "unknown"
	}

	return info
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return s
}

// ---- Kecocokan jurisdiksi ----

// CanonicalInstansi adalah 23 kabupaten/kota Aceh + entri provinsi. Nama HARUS
// persis seperti yang tertulis di dokumen resmi (UU/Qanun tidak pernah pakai
// singkatan), sehingga exact-match (bukan Contains) aman dipakai — mencegah
// "ACEH BARAT" salah cocok dengan "ACEH BARAT DAYA" atau sebaliknya.
var CanonicalInstansi = []string{
	// Provinsi — Aceh TIDAK menulis "Provinsi" untuk dokumen modern (Qanun era
	// UU 11/2006 Pemerintahan Aceh menulis langsung "ACEH" / "PEMERINTAH ACEH").
	// Dokumen format lama (pra-UU 11/2006) mungkin masih menulis "DAERAH
	// ISTIMEWA ACEH" atau "PROVINSI DAERAH TINGKAT I ACEH" — ditangani lewat
	// PreUU122011 -> review_manual, bukan exact-match di sini.
	"ACEH",
	"PEMERINTAH ACEH",
	// 23 kabupaten/kota.
	"KABUPATEN ACEH BARAT",
	"KABUPATEN ACEH BARAT DAYA",
	"KABUPATEN ACEH BESAR",
	"KABUPATEN ACEH JAYA",
	"KABUPATEN ACEH SELATAN",
	"KABUPATEN ACEH SINGKIL",
	"KABUPATEN ACEH TAMIANG",
	"KABUPATEN ACEH TENGAH",
	"KABUPATEN ACEH TENGGARA",
	"KABUPATEN ACEH TIMUR",
	"KABUPATEN ACEH UTARA",
	"KABUPATEN BENER MERIAH",
	"KABUPATEN BIREUEN",
	"KABUPATEN GAYO LUES",
	"KABUPATEN NAGAN RAYA",
	"KABUPATEN PIDIE",
	"KABUPATEN PIDIE JAYA",
	"KABUPATEN SIMEULUE",
	"KOTA BANDA ACEH",
	"KOTA LANGSA",
	"KOTA LHOKSEUMAWE",
	"KOTA SABANG",
	"KOTA SUBULUSSALAM",
}

// MatchesJurisdiction melaporkan apakah instansi yang terekstrak dari halaman 1
// cocok PERSIS dengan instansi pemilik source (setelah normalisasi kapital/
// spasi) — EXACT match, bukan substring, khusus untuk menghindari salah-cocok
// pada nama kabupaten yang mirip tipis (mis. "Aceh Barat" vs "Aceh Barat Daya").
func MatchesJurisdiction(extractedInstansi, sourceInstansiName string) bool {
	a := normalizeInstansi(extractedInstansi)
	b := normalizeInstansi(sourceInstansiName)
	if a == "" || b == "" {
		return false
	}
	return a == b
}

func normalizeInstansi(s string) string {
	s = strings.ToUpper(s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimSuffix(s, ",")
	// Ejaan lama (dokumen sebelum EYD/reformasi ejaan) menulis "Propinsi"
	// dengan p — dokumen modern menulis "Provinsi". Normalisasi ke satu
	// bentuk supaya keduanya bisa dicocokkan ke daftar kanonik yang sama.
	s = strings.ReplaceAll(s, "PROPINSI", "PROVINSI")
	return strings.TrimSpace(s)
}
