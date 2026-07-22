package pipeline

import (
	"regexp"
	"strconv"
	"strings"
)

// normalize.go mengubah apa yang DIBACA model/regex menjadi WILAYAH baku
// yang dipakai basis data (lihat whitelist.go untuk 25 wilayah yang sah).
//
// Pembagian tugasnya disengaja: model/regex hanya menyalin apa yang tertulis
// ("GUBERNUR ACEH", "DPRK ACEH BARAT", "300.2/ 69 /2026"), sedangkan
// penyimpulan dikerjakan di sini. Pemetaan jabatan ke wilayah bersifat pasti
// dan berkaidah tetap — menyuruh model kecil menyimpulkannya berarti memberi
// pekerjaan yang bisa salah kepada bagian yang paling sulit diaudit, padahal
// beberapa baris kode menyelesaikannya tanpa pernah keliru.
//
// NormalizeWilayah HANYA memetakan — ia tidak menolak. Penolakan (jenis/
// wilayah tidak dikenal) diperiksa terpisah lewat IsWilayahValid/IsJenisValid
// (whitelist.go), supaya keduanya bisa dipakai baik untuk hasil regex maupun
// hasil model.

var (
	// reJabatanGubernur dkk. menangkap jabatan kepala daerah beserta nama
	// daerahnya.
	reJabatanGubernur = regexp.MustCompile(`(?i)^GUBERNUR\s+(.+)$`)
	reJabatanBupati   = regexp.MustCompile(`(?i)^BUPATI\s+(.+)$`)
	reJabatanWali     = regexp.MustCompile(`(?i)^WALI\s*KOTA\s+(.+)$`)

	// reJabatanPresiden/reJabatanMenteri: nasional. PRESIDEN/MENTERI dipakai
	// sebagai AWALAN (bukan padanan penuh) karena instansi_tertulis biasanya
	// menyertakan nama lengkap ("PRESIDEN REPUBLIK INDONESIA", "MENTERI
	// DALAM NEGERI").
	reJabatanPresiden = regexp.MustCompile(`(?i)^PRESIDEN\b`)
	reJabatanMenteri  = regexp.MustCompile(`(?i)^MENTERI\b`)

	// DPRA selalu di tingkat Aceh (tidak ada nama daerah menyertai). DPRK
	// SELALU menyertai nama daerahnya, tetapi TIDAK menyebut sendiri apakah
	// daerah itu kabupaten atau kota — lihat resolveKabKota.
	reDPRA         = regexp.MustCompile(`(?i)^DPRA\s*$`)
	reDPRK         = regexp.MustCompile(`(?i)^DPRK\s+(.+)$`)
	rePimpinanDPRA = regexp.MustCompile(`(?i)^PIMPINAN\s+DPRA\s*$`)
	rePimpinanDPRK = regexp.MustCompile(`(?i)^PIMPINAN\s+DPRK\s+(.+)$`)

	// reAwalanDaerah menangkap bentuk yang sudah menyebut badan/daerahnya.
	reKabupaten = regexp.MustCompile(`(?i)^KABUPATEN\s+(.+)$`)
	reKota      = regexp.MustCompile(`(?i)^KOTA\s+(.+)$`)
	reProvinsi  = regexp.MustCompile(`(?i)^(?:PROVINSI|PROPINSI)\s+(.+)$`)

	reSpasi = regexp.MustCompile(`\s+`)
)

// NormalizeWilayah mengubah apa yang tertulis di dokumen menjadi salah satu
// dari 25 wilayah baku (lihat WilayahList di whitelist.go).
//
//	PRESIDEN ...                  -> NASIONAL
//	MENTERI ...                   -> NASIONAL
//	DPRA / PIMPINAN DPRA          -> PEMERINTAH ACEH
//	DPRK ACEH BARAT               -> KABUPATEN ACEH BARAT   (lihat resolveKabKota)
//	GUBERNUR ACEH                 -> PEMERINTAH ACEH
//	BUPATI ACEH BARAT             -> KABUPATEN ACEH BARAT
//	WALI KOTA BANDA ACEH          -> KOTA BANDA ACEH
//	ACEH                          -> PEMERINTAH ACEH
//	KABUPATEN ACEH BARAT          -> KABUPATEN ACEH BARAT   (sudah baku)
//	PROPINSI DAERAH ISTIMEWA ACEH -> PEMERINTAH ACEH        (ejaan & nomenklatur lama)
//
// Bentuk yang tidak dikenali dikembalikan apa adanya setelah dirapikan
// spasinya — lebih baik menyimpan yang tertulis daripada menebak. Pemanggil
// yang perlu menolak dokumen dengan wilayah tak dikenal memakai
// IsWilayahValid atas HASIL fungsi ini, bukan mengecek di sini.
func NormalizeWilayah(tertulis string) string {
	s := rapikan(tertulis)
	if s == "" {
		return ""
	}

	// Nasional.
	if reJabatanPresiden.MatchString(s) || reJabatanMenteri.MatchString(s) {
		return "NASIONAL"
	}

	// DPRA/DPRK dan pimpinannya.
	if reDPRA.MatchString(s) || rePimpinanDPRA.MatchString(s) {
		return "PEMERINTAH ACEH"
	}
	if m := reDPRK.FindStringSubmatch(s); m != nil {
		return resolveKabKota(rapikan(m[1]))
	}
	if m := rePimpinanDPRK.FindStringSubmatch(s); m != nil {
		return resolveKabKota(rapikan(m[1]))
	}

	// Jabatan kepala daerah -> wilayahnya.
	if m := reJabatanGubernur.FindStringSubmatch(s); m != nil {
		return provinsiKe(rapikan(m[1]))
	}
	if m := reJabatanBupati.FindStringSubmatch(s); m != nil {
		return "KABUPATEN " + rapikan(m[1])
	}
	if m := reJabatanWali.FindStringSubmatch(s); m != nil {
		return "KOTA " + rapikan(m[1])
	}

	// Sudah menyebut badan/daerahnya.
	if m := reKabupaten.FindStringSubmatch(s); m != nil {
		return "KABUPATEN " + rapikan(m[1])
	}
	if m := reKota.FindStringSubmatch(s); m != nil {
		return "KOTA " + rapikan(m[1])
	}
	if m := reProvinsi.FindStringSubmatch(s); m != nil {
		return provinsiKe(rapikan(m[1]))
	}

	// Nama daerah telanjang: hanya Aceh yang punya bentuk khusus.
	return provinsiKe(s)
}

// resolveKabKota menyelesaikan nama daerah TANPA prefiks ("ACEH BARAT")
// menjadi wilayah baku lengkap dengan prefiksnya ("KABUPATEN ACEH BARAT"
// atau "KOTA BANDA ACEH"), dengan mencocokkan ke WilayahList (whitelist.go).
// Diperlukan karena "DPRK <nama>" di dokumen tidak menyebut sendiri apakah
// daerahnya kabupaten atau kota — satu-satunya cara memastikan adalah
// mencocokkan namanya ke daftar 23 kabupaten/kota kanonik.
func resolveKabKota(nama string) string {
	if IsWilayahValid(nama) {
		return nama
	}
	if kandidat := "KABUPATEN " + nama; IsWilayahValid(kandidat) {
		return kandidat
	}
	if kandidat := "KOTA " + nama; IsWilayahValid(kandidat) {
		return kandidat
	}
	// Tak dikenal di daftar kanonik. Dikembalikan dengan prefiks KABUPATEN
	// supaya tetap tercatat (untuk ditinjau), bukan hilang jadi string
	// kosong — IsWilayahValid oleh pemanggil tetap akan menolaknya.
	return "KABUPATEN " + nama
}

// provinsiKe memetakan nama provinsi ke sebutan bakunya. Aceh diperlakukan
// khusus karena otonomi khususnya: produk hukumnya menulis "Pemerintah Aceh",
// bukan "Provinsi Aceh". Nomenklatur lama ("Daerah Istimewa Aceh",
// "Daerah Tingkat I Aceh") tetap dipetakan ke sebutan yang sama supaya
// dokumen lama dan baru tidak terpecah menjadi dua wilayah berbeda.
func provinsiKe(nama string) string {
	n := rapikan(nama)
	if strings.Contains(n, "ACEH") && !strings.HasPrefix(n, "KABUPATEN ") && !strings.HasPrefix(n, "KOTA ") {
		return "PEMERINTAH ACEH"
	}
	if n == "" {
		return ""
	}
	return "PROVINSI " + n
}

func rapikan(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = reSpasi.ReplaceAllString(s, " ")
	s = strings.Trim(s, " ,.")
	return s
}

// reAngkaPertama mengambil deretan angka pertama dari sebuah nomor peraturan.
var reAngkaPertama = regexp.MustCompile(`[0-9]+`)

// NomorUrut menurunkan angka untuk pengurutan dari nomor peraturan yang
// ditulis bebas.
//
//	"5"                -> 5
//	"12A"              -> 12
//	"300.2/ 69 /2026"  -> 300
//	"-" / ""           -> 0 (tidak diurutkan)
//
// Nomor aslinya TIDAK dibuang: ia disimpan apa adanya di kolom tersendiri,
// sehingga pengurutan tidak pernah menghilangkan bentuk resminya.
func NomorUrut(nomor string) int {
	m := reAngkaPertama.FindString(nomor)
	if m == "" {
		return 0
	}
	v, err := strconv.Atoi(m)
	if err != nil || v < 0 {
		return 0
	}
	return v
}
