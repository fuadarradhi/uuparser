package pipeline

import (
	"regexp"
	"strconv"
	"strings"
)

// normalize.go mengubah apa yang DIBACA model menjadi bentuk baku yang
// dipakai basis data.
//
// Pembagian tugasnya disengaja: model hanya menyalin apa yang tertulis
// ("GUBERNUR ACEH", "300.2/ 69 /2026"), sedangkan penyimpulan dikerjakan di
// sini. Pemetaan jabatan ke badan pemerintahan bersifat pasti dan berkaidah
// tetap — menyuruh model kecil menyimpulkannya berarti memberi pekerjaan yang
// bisa salah kepada bagian yang paling sulit diaudit, padahal beberapa baris
// kode menyelesaikannya tanpa pernah keliru.

var (
	// reJabatan menangkap jabatan kepala daerah beserta nama daerahnya.
	reJabatanGubernur = regexp.MustCompile(`(?i)^GUBERNUR\s+(.+)$`)
	reJabatanBupati   = regexp.MustCompile(`(?i)^BUPATI\s+(.+)$`)
	reJabatanWali     = regexp.MustCompile(`(?i)^WALI\s*KOTA\s+(.+)$`)

	// reAwalanDaerah menangkap bentuk yang sudah menyebut badan/daerahnya.
	reKabupaten = regexp.MustCompile(`(?i)^KABUPATEN\s+(.+)$`)
	reKota      = regexp.MustCompile(`(?i)^KOTA\s+(.+)$`)
	reProvinsi  = regexp.MustCompile(`(?i)^(?:PROVINSI|PROPINSI)\s+(.+)$`)

	reSpasi = regexp.MustCompile(`\s+`)
)

// NormalizeInstansi mengubah apa yang tertulis di dokumen menjadi nama badan
// pemerintahan yang membentuknya.
//
//	GUBERNUR ACEH            -> PEMERINTAH ACEH
//	BUPATI ACEH BARAT        -> KABUPATEN ACEH BARAT
//	WALI KOTA BANDA ACEH     -> KOTA BANDA ACEH
//	ACEH                     -> PEMERINTAH ACEH
//	KABUPATEN ACEH BARAT     -> KABUPATEN ACEH BARAT   (sudah baku)
//	PROPINSI DAERAH ISTIMEWA ACEH -> PEMERINTAH ACEH   (ejaan & nomenklatur lama)
//
// Bentuk yang tidak dikenali dikembalikan apa adanya setelah dirapikan
// spasinya — lebih baik menyimpan yang tertulis daripada menebak.
func NormalizeInstansi(tertulis string) string {
	s := rapikan(tertulis)
	if s == "" {
		return ""
	}

	// Jabatan kepala daerah -> badan pemerintahannya.
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

// provinsiKe memetakan nama provinsi ke sebutan bakunya. Aceh diperlakukan
// khusus karena otonomi khususnya: produk hukumnya menulis "Pemerintah Aceh",
// bukan "Provinsi Aceh". Nomenklatur lama ("Daerah Istimewa Aceh",
// "Daerah Tingkat I Aceh") tetap dipetakan ke sebutan yang sama supaya
// dokumen lama dan baru tidak terpecah menjadi dua instansi berbeda.
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
