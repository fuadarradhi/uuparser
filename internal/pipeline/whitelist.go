package pipeline

import "strings"

// whitelist.go mendaftar JENIS peraturan dan WILAYAH yang dikenal sistem.
// Dokumen yang jenis atau wilayahnya (setelah dinormalisasi) tidak cocok
// salah satu daftar ini ditolak — sistem tidak menyimpan data yang bentuknya
// tidak terjamin konsisten untuk query/RAG nanti.
//
// "QANUN" sengaja HANYA satu entri (bukan "QANUN ACEH" / "QANUN KABUPATEN" /
// "QANUN KOTA" terpisah): kata "QANUN" sendiri tidak membawa informasi
// tingkat — tingkatnya ada di WILAYAH yang mengikutinya ("ACEH" / "KABUPATEN
// ACEH BARAT" / "KOTA BANDA ACEH"), dan itu sudah diperiksa lewat
// WilayahList. Menjadikan "KABUPATEN"/"KOTA" bagian dari JENIS juga akan
// membuat informasi itu dobel-dicek di dua daftar berbeda.
//
// PERATURAN KEPALA DAERAH tetap masuk daftar sebagai istilah payung generik
// meski jarang tertulis literal di judul dokumen (biasanya sudah spesifik:
// Pergub/Perbup/Perwako) — user memilih tetap menyimpannya.

// JenisList adalah jenis peraturan yang dikenal sistem, huruf besar semua,
// spasi tunggal.
var JenisList = []string{
	"UNDANG-UNDANG",
	"PERATURAN PEMERINTAH PENGGANTI UNDANG-UNDANG",
	"PERATURAN PEMERINTAH",
	"PERATURAN PRESIDEN",
	"PERATURAN MENTERI",
	"QANUN",
	"PERATURAN GUBERNUR",
	"PERATURAN BUPATI",
	"PERATURAN WALI KOTA",
	"PERATURAN KEPALA DAERAH",
	"PERATURAN DPRA",
	"PERATURAN DPRK",
	"KEPUTUSAN PRESIDEN",
	"KEPUTUSAN MENTERI",
	"KEPUTUSAN GUBERNUR",
	"KEPUTUSAN BUPATI",
	"KEPUTUSAN WALI KOTA",
	"KEPUTUSAN DPRA",
	"KEPUTUSAN DPRK",
	"KEPUTUSAN PIMPINAN DPRA",
	"KEPUTUSAN PIMPINAN DPRK",
	"INSTRUKSI PRESIDEN",
	"INSTRUKSI MENTERI",
	"INSTRUKSI GUBERNUR",
	"INSTRUKSI BUPATI",
	"INSTRUKSI WALI KOTA",
	"SURAT EDARAN MENTERI",
	"SURAT EDARAN GUBERNUR",
	"SURAT EDARAN BUPATI",
	"SURAT EDARAN WALI KOTA",
}

// WilayahList adalah 25 wilayah yang dikenal sistem: nasional, Pemerintah
// Aceh, dan 23 kabupaten/kota-nya. Sama persis dengan daftar kanonik yang
// dulu ada di parser.CanonicalInstansi, plus NASIONAL.
var WilayahList = []string{
	"NASIONAL",
	"PEMERINTAH ACEH",
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

// normalizeJenis merapikan jenis sebelum dicocokkan ke JenisList: huruf
// besar, spasi tunggal, dan menyatukan ejaan satu-kata/dua-kata yang sama-
// sama lazim dipakai dokumen ("WALIKOTA" vs "WALI KOTA").
func normalizeJenis(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, "WALIKOTA", "WALI KOTA")
	return s
}

// IsJenisValid melaporkan apakah jenis (bentuk mentah dari regex/model,
// belum dirapikan) ada di JenisList setelah dinormalisasi.
func IsJenisValid(jenis string) bool {
	j := normalizeJenis(jenis)
	for _, v := range JenisList {
		if v == j {
			return true
		}
	}
	return false
}

// IsWilayahValid melaporkan apakah wilayah ada di WilayahList. Menerima
// bentuk mentah ATAUPUN yang sudah dirapikan — dirapikan ulang di sini
// supaya aman dipanggil dari mana saja (termasuk dari dalam NormalizeWilayah
// sendiri, lihat resolveKabKota).
func IsWilayahValid(wilayah string) bool {
	w := rapikan(wilayah)
	for _, v := range WilayahList {
		if v == w {
			return true
		}
	}
	return false
}
