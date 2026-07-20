package parser

import (
	"regexp"
	"strings"
)

// classify.go adalah gerbang (gate): menentukan apakah kumpulan baris ini layak
// diperlakukan sebagai dokumen hukum. Jika tidak, Parse berhenti lebih awal.
//
// Prinsip: butuh minimal satu anchor KUAT. Anchor terkuat adalah adanya pola
// "Pasal N" (hampir semua level regulasi punya), disusul "BAB <romawi>" dan
// kata kunci konsiderans.

type classifyResult struct {
	isLegal       bool
	hasPasal      bool
	hasBab        bool
	hasMenimbang  bool
	hasMengingat  bool
	hasMemutuskan bool
	pasalCount    int
}

func classify(lines []string) classifyResult {
	joined := strings.Join(lines, "\n")
	var r classifyResult

	r.pasalCount = len(rePasalAnywhere.FindAllString(joined, -1))
	r.hasPasal = r.pasalCount > 0
	r.hasBab = reBabAnywhere.MatchString(joined)
	r.hasMenimbang = reMenimbang.MatchString(joined)
	r.hasMengingat = reMengingat.MatchString(joined)
	r.hasMemutuskan = reMemutuskan.MatchString(joined)

	// Aturan kelayakan:
	//  - Ada >=1 "Pasal N": kuat -> legal.
	//  - Atau: ada BAB romawi DAN salah satu kata kunci konsiderans.
	//  - Atau: ada Menimbang DAN Mengingat DAN Memutuskan (struktur pembuka lengkap).
	switch {
	case r.hasPasal:
		r.isLegal = true
	case r.hasBab && (r.hasMenimbang || r.hasMengingat || r.hasMemutuskan):
		r.isLegal = true
	case r.hasMenimbang && r.hasMengingat && r.hasMemutuskan:
		r.isLegal = true
	default:
		r.isLegal = false
	}
	return r
}

// LooksLegal melaporkan apakah teks (gabungan beberapa halaman OCR) sudah
// menunjukkan ciri dokumen perundang-undangan. Dipakai sebagai gate awal saat
// ekstraksi: bila beberapa halaman pertama tidak menunjukkan ciri apa pun, OCR
// halaman berikutnya bisa dibatalkan (hemat, untuk PDF non-peraturan yang tebal).
func LooksLegal(pages []string) bool {
	text := strings.Join(pages, "\n")
	return classify(strings.Split(text, "\n")).isLegal
}

// reJudulPeraturan menangkap baris judul peraturan pada halaman sampul.
// [UPDATE UU 13/2022 & UU 15/2019] Menambahkan PERATURAN KEPALA DAERAH, PERATURAN DESA,
// dan PERATURAN LEMBAGA NEGARA sesuai penyesuaian hierarki dan pengakuan jenis peraturan.
var reJudulPeraturan = regexp.MustCompile(`(?i)(UNDANG-UNDANG|PERATURAN DAERAH|PERATURAN KEPALA DAERAH|PERATURAN DESA|PERATURAN LEMBAGA NEGARA|PERATURAN PEMERINTAH|PERATURAN PRESIDEN|PERATURAN MENTERI|PERATURAN GUBERNUR|PERATURAN BUPATI|PERATURAN WALI ?KOTA|QANUN)\b[\s\S]{0,80}?\bNOMOR\b`)

// LooksLegalProbe adalah gerbang LONGGAR untuk beberapa halaman pertama saat
// ekstraksi. Tujuannya hanya menyaring PDF yang jelas-jelas bukan peraturan
// (mis. buku/laporan ratusan halaman) agar tidak di-OCR seluruhnya.
//
// Sengaja jauh lebih longgar daripada classify(): satu sinyal saja sudah cukup
// untuk melanjutkan. Salah-tolak peraturan asli jauh lebih merugikan daripada
// meng-OCR beberapa halaman tambahan; keputusan akhir tetap di tangan Parse().
func LooksLegalProbe(pages []string) bool {
	text := strings.Join(pages, "\n")
	switch {
	case rePasalAnywhere.MatchString(text),
		reBabAnywhere.MatchString(text),
		reMenimbang.MatchString(text),
		reMengingat.MatchString(text),
		reMemutuskan.MatchString(text),
		reJudulPeraturan.MatchString(text):
		return true
	}
	return false
}
