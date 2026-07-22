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
	Jenis         string // "PERATURAN DAERAH", "QANUN", "PERATURAN BUPATI", dst
	Instansi      string // frasa instansi APA ADANYA dari teks (utuh, tak dipotong)
	Nomor         string
	Tahun         string
	Tentang       string
	StructureType string // "pasal_ayat" | "diktum" | "unknown"
	Found         bool   // true bila pola judul/jabatan-pembentuk ketemu sama sekali
	PreUU122011   bool   // true bila Tahun < 2011 (aturan format Lampiran II belum berlaku)
}

// jenisAlternation adalah daftar jenis yang DIKENALI regex ekstraksi (bukan
// whitelist — lihat catatan di reHeaderJudulDenganTahun).
const jenisAlternation = `UNDANG-UNDANG|PERATURAN\s+PEMERINTAH\s+PENGGANTI\s+UNDANG-UNDANG|PERATURAN\s+PEMERINTAH|PERATURAN\s+PRESIDEN|PERATURAN\s+MENTERI|PERATURAN\s+DAERAH|PERATURAN\s+GUBERNUR|PERATURAN\s+BUPATI|PERATURAN\s+WALI\s*KOTA|PERATURAN\s+KEPALA\s+DAERAH|PERATURAN\s+DESA|PERATURAN\s+DPRA|PERATURAN\s+DPRK|KEPUTUSAN\s+PRESIDEN|KEPUTUSAN\s+MENTERI|KEPUTUSAN\s+GUBERNUR|KEPUTUSAN\s+BUPATI|KEPUTUSAN\s+WALI\s*KOTA|KEPUTUSAN\s+PIMPINAN\s+DPRA|KEPUTUSAN\s+PIMPINAN\s+DPRK|KEPUTUSAN\s+DPRA|KEPUTUSAN\s+DPRK|INSTRUKSI\s+PRESIDEN|INSTRUKSI\s+MENTERI|INSTRUKSI\s+GUBERNUR|INSTRUKSI\s+BUPATI|INSTRUKSI\s+WALI\s*KOTA|SURAT\s+EDARAN\s+MENTERI|SURAT\s+EDARAN\s+GUBERNUR|SURAT\s+EDARAN\s+BUPATI|SURAT\s+EDARAN\s+WALI\s*KOTA|QANUN`

// reDPRAPanjang/reDPRKPanjang menangkap sebutan LENGKAP dewan perwakilan
// rakyat Aceh — lihat ringkasSebutanDPR. Kata "KABUPATEN"/"KOTA" pada
// sebutan lengkap DPRK sengaja DIBUANG saat disusutkan (bukan disimpan
// terpisah): pipeline.resolveKabKota sudah mencocokkan nama daerahnya ke
// daftar kanonik dari kedua kemungkinan, jadi informasi itu tidak hilang —
// cuma ditentukan belakangan lewat whitelist, bukan di sini. \b di akhir
// mencegah "KABUPATEN"/"KOTA" ikut kepotong dari kata sesudahnya.
var reDPRAPanjang = regexp.MustCompile(`(?i)DEWAN\s+PERWAKILAN\s+RAKYAT\s+ACEH\b`)
var reDPRKPanjang = regexp.MustCompile(`(?i)DEWAN\s+PERWAKILAN\s+RAKYAT\s+(?:KABUPATEN|KOTA)\b`)

// ringkasSebutanDPR menyusutkan sebutan lengkap dewan perwakilan rakyat Aceh
// menjadi singkatan baku (DPRA/DPRK) sebelum regex header dijalankan.
// "PIMPINAN DEWAN PERWAKILAN RAKYAT ACEH" otomatis ikut tersusutkan jadi
// "PIMPINAN DPRA" karena substring "DEWAN PERWAKILAN RAKYAT ACEH" di
// dalamnya tetap cocok — tidak perlu pola terpisah untuk kasus Pimpinan.
func ringkasSebutanDPR(s string) string {
	s = reDPRAPanjang.ReplaceAllString(s, "DPRA")
	s = reDPRKPanjang.ReplaceAllString(s, "DPRK")
	return s
}

// reHeaderJudulDenganTahun menangkap blok "<JENIS> <INSTANSI...> NOMOR <N>
// TAHUN <Y> TENTANG <T>" — bentuk Lampiran II UU 12/2011 yang dipakai
// UU/Perda/Qanun/Perkada dan sebagian besar produk hukum lainnya. Instansi
// ditangkap NON-GREEDY sampai kata "NOMOR" — sengaja ambil frasa PENUH,
// bukan dibatasi jumlah kata, supaya "ACEH BARAT DAYA" tidak terpotong jadi
// "ACEH BARAT" (lihat diskusi soal nama kabupaten yang mirip tipis). Nomor
// ditangkap LONGGAR (bukan cuma digit) karena sebagian jenis (Keputusan)
// menulis nomor gabungan kode/tanggal, mis. "300.2/ 69 /2026".
//
// Daftar jenis di sini SENGAJA lebih longgar daripada whitelist jenis di
// pipeline.JenisList — regex ini hanya bertugas MENGEKSTRAK, bukan
// memvalidasi; jenis yang berhasil diekstrak tapi ternyata tidak ada di
// whitelist tetap ditolak belakangan oleh pipeline.IsJenisValid. Beberapa
// entri (PERATURAN DAERAH, PERATURAN DESA) memang di luar whitelist saat ini
// tapi tetap ditangkap di sini supaya kalau kelak muncul, ditolak dengan
// alasan yang jelas ("jenis tak dikenal") — bukan gagal parse total.
var reHeaderJudulDenganTahun = regexp.MustCompile(
	`(?is)(` + jenisAlternation + `)(?:\s+([A-Z][A-Z\s]*?))?\s*NOMOR\s+([0-9][0-9A-Za-z./\s-]*?)\s+TAHUN\s+([0-9]{4})\s+TENTANG\s+(.+)`)

// reHeaderJudulTanpaTahun menangkap bentuk yang SAMA tapi TANPA klausa
// "TAHUN <Y>" terpisah — banyak Keputusan menulis tahunnya SEBAGAI BAGIAN
// nomor ("NOMOR 300.2/ 69 /2026 TENTANG ...", tanpa "TAHUN 2026" lagi
// sesudahnya). Tahun pada kasus ini diambil dari deretan 4-angka TERAKHIR
// di dalam nomor (lihat ambilTahunDariNomor).
var reHeaderJudulTanpaTahun = regexp.MustCompile(
	`(?is)(` + jenisAlternation + `)(?:\s+([A-Z][A-Z\s]*?))?\s*NOMOR\s+([0-9][0-9A-Za-z./\s-]*?)\s+TENTANG\s+(.+)`)

// reTahunDalamNomor mengambil deretan 4-angka di dalam nomor bebas.
var reTahunDalamNomor = regexp.MustCompile(`(1[89]|20)[0-9]{2}`)

// ambilTahunDariNomor mengembalikan deretan 4-angka TERAKHIR dalam nomor
// (tahun lazimnya di ujung, mis. ".../2026"), atau "" bila tak ada.
func ambilTahunDariNomor(nomor string) string {
	all := reTahunDalamNomor.FindAllString(nomor, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1]
}

// reJabatanPembentuk menangkap baris jabatan pembentuk di Pembukaan, mis.
// "BUPATI ACEH BARAT," atau "GUBERNUR ACEH,". Dipakai sebagai titik ekstraksi
// KEDUA (fallback) bila Judul rusak/OCR gagal tangkap — keduanya sama-sama
// wajib ada per Lampiran II, jadi salah satu cukup untuk validasi wilayah.
var reJabatanPembentuk = regexp.MustCompile(`(?im)^(GUBERNUR|BUPATI|WALI\s*KOTA|PRESIDEN|MENTERI|PIMPINAN\s+DPRA|PIMPINAN\s+DPRK|DPRA|DPRK)\s+([A-Z][A-Z\s]*?)\s*,\s*$`)

// reDiktum menangkap format Keputusan: "KESATU", "KEDUA", dst (Diktum), bukan
// Pasal/Ayat. Deteksinya CUKUP satu diktum di halaman 1 karena Keputusan tidak
// pernah punya Pasal 1 di halaman pertama juga.
var reDiktum = regexp.MustCompile(`(?im)^\s*(KESATU|KEDUA|KETIGA)\s*:`)

// ExtractHeader membaca teks halaman PERTAMA (sudah di-OCR) dan mengekstrak
// identitas peraturan secara deterministik (regex, BUKAN LLM — lihat alasan
// yang sama seperti relations.go: LLM berisiko mengarang nomor/instansi).
// judulSaja memotong teks halaman 1 sampai SEBELUM "Menimbang" (reMenimbang
// didefinisikan di patterns.go, dipakai bersama) — hanya blok Judul (dan
// jabatan pembentuk yang menyertainya) yang boleh dijadikan sumber identitas
// dokumen. Daftar Mengingat/Memperhatikan dan seluruh batang tubuh SELALU
// berada setelah "Menimbang", jadi tidak akan pernah ikut tercari. Bila
// "Menimbang" tidak ditemukan (format menyimpang/OCR gagal menangkapnya),
// teks utuh dipakai apa adanya — lebih baik mencoba pada teks penuh
// daripada gagal total karena satu kata penanda hilang.
//
// Bug yang melatarbelakangi ini (ditemukan dari uji dokumen sungguhan,
// 2026-07-22): tanpa batas ini, "Undang-Undang Nomor 24 Tahun 1956 tentang
// ..." di dalam daftar Mengingat (kutipan peraturan LAIN yang jadi dasar
// hukum) ikut cocok pola judul dan MENIMPA identitas dokumen ini sendiri —
// sebuah Keputusan Gubernur 2026 salah teridentifikasi sebagai "UNDANG-
// UNDANG NOMOR 24 TAHUN 1956" karena judul aslinya tidak punya klausa
// "TAHUN" eksplisit (gagal di reHeaderJudulDenganTahun), sehingga pencarian
// berlanjut ke bawah sampai ketemu kutipan di Mengingat yang kebetulan
// cocok pola itu.
func judulSaja(s string) string {
	if loc := reMenimbang.FindStringIndex(s); loc != nil {
		return s[:loc[0]]
	}
	return s
}

func ExtractHeader(page1Text string) HeaderInfo {
	// Keamanan tambahan (permintaan user, 2026-07-22, menyusul bug hari
	// ini): regex HANYA dipercaya kalau "Menimbang" (reMenimbang, sudah ada
	// di patterns.go) ditemukan di teks — itu satu-satunya penanda yang
	// benar-benar menjamin keamanan jendela "sebelum Menimbang" (judulSaja)
	// sebagai blok Judul, karena Mengingat/Memperhatikan/batang tubuh
	// SELALU berada setelah Menimbang, di mana pun letak persisnya.
	//
	// SEBELUMNYA sempat juga mensyaratkan "Memutuskan" ada di teks yang
	// sama — user mengoreksi (2026-07-22): itu keliru, karena "MEMUTUSKAN"
	// bisa saja jatuh di HALAMAN LAIN kalau Menimbang/Mengingat/
	// Memperhatikan-nya panjang (classify() hanya membaca halaman pertama
	// yang berisi teks), padahal keberadaannya di halaman lain TIDAK
	// mempengaruhi keamanan jendela "sebelum Menimbang" sama sekali —
	// mensyaratkannya cuma memaksa banyak dokumen sah jatuh ke model
	// tanpa alasan.
	//
	// Kalau "Menimbang" tidak ditemukan sama sekali — dokumen menyimpang,
	// atau OCR gagal menangkap katanya — REGEX SAMA SEKALI TIDAK DICOBA;
	// pemanggil (CobaIdentitasDeterministik) akan melihat Found=false dan
	// beralih sepenuhnya ke model teks. Lebih aman mengaku tidak tahu
	// daripada mencari pola judul pada teks yang jendelanya tidak bisa
	// dipastikan aman — itu persis penyebab bug 2026-07-22 (kutipan di
	// badan teks tertangkap sebagai judul dokumen ini sendiri).
	if !reMenimbang.MatchString(page1Text) {
		return HeaderInfo{}
	}

	// Peraturan JARANG memakai singkatan (beda dari kebiasaan sehari-hari) —
	// dokumen sungguhan lebih mungkin menulis sebutan LENGKAP dewan
	// perwakilan rakyat ("Dewan Perwakilan Rakyat Aceh", "...Kabupaten/Kota
	// <nama>") daripada singkatannya (DPRA/DPRK). ringkasSebutanDPR
	// menyusutkannya ke singkatan SEBELUM regex header jalan, supaya seluruh
	// logika DPRA/DPRK yang sudah ada (di reHeaderJudulDenganTahun,
	// reJabatanPembentuk, dan pipeline.wilayahDariJenisInstansi/
	// resolveKabKota) tetap berfungsi tanpa duplikasi pola.
	raw := ringkasSebutanDPR(judulSaja(page1Text))

	var info HeaderInfo

	if m := reHeaderJudulDenganTahun.FindStringSubmatch(raw); m != nil {
		info.Found = true
		info.Jenis = normalizeSpace(m[1])
		info.Instansi = normalizeSpace(m[2])
		info.Nomor = strings.TrimSpace(m[3])
		info.Tahun = strings.TrimSpace(m[4])
		info.Tentang = blokTentang(m[5])
	} else if m := reHeaderJudulTanpaTahun.FindStringSubmatch(raw); m != nil {
		info.Found = true
		info.Jenis = normalizeSpace(m[1])
		info.Instansi = normalizeSpace(m[2])
		info.Nomor = strings.TrimSpace(m[3])
		info.Tahun = ambilTahunDariNomor(info.Nomor)
		info.Tentang = blokTentang(m[4])
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

// reBarisKosong menandai baris kosong (pemisah paragraf) — dipakai
// blokTentang untuk mengambil blok "TENTANG ..." secara UTUH meski judulnya
// melebar ke lebih dari satu baris cetak, tanpa ikut menelan baris
// berikutnya (jabatan pembentuk, "Menimbang", dst) yang di dokumen asli
// SELALU dipisah baris kosong dari judul.
var reBarisKosong = regexp.MustCompile(`\r?\n\s*\r?\n`)

// blokTentang mengambil "TENTANG ..." sampai baris kosong PERTAMA
// setelahnya — BUKAN cuma baris cetak pertama (itu bug nyata yang
// ditemukan dari uji dokumen sungguhan, 2026-07-22: judul yang melebar ke
// baris cetak kedua terpotong oleh firstLine, hanya separuh tersimpan).
// Baris-baris di dalam blok itu disatukan jadi satu kalimat mengalir,
// karena baris cetak bukan akhir kalimat.
func blokTentang(s string) string {
	if loc := reBarisKosong.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.TrimSpace(strings.Join(lines, " "))
}

// Catatan: daftar wilayah kanonik & pencocokan jurisdiksi dulu ada di sini
// (CanonicalInstansi/MatchesJurisdiction) tapi tidak pernah dipanggil dari
// pipeline manapun — mati sejak pivot ke arsitektur dokumen-sentris 2026-07-21
// (lihat CATATAN-MIGRASI.md: "tidak ada lagi pemeriksaan jurisdiksi per
// sumber"). Peran itu sekarang diambil alih pipeline.WilayahList +
// pipeline.IsWilayahValid (2026-07-22), yang memvalidasi wilayah hasil
// ekstraksi terhadap daftar 25 wilayah dikenal — TANPA mencocokkannya ke
// instansi pemilik source (yang memang sudah tidak dipercaya sejak pivot).
