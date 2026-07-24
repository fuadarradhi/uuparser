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
	isLegal          bool
	hasPasal         bool
	hasBab           bool
	hasMenimbang     bool
	hasMengingat     bool
	hasMemperhatikan bool
	hasMemutuskan    bool
	hasMenetapkan    bool
	hasDiktum        bool
	pasalCount       int
}

func classify(lines []Line) classifyResult {
	joined := joinLineText(lines)
	var r classifyResult

	r.pasalCount = len(rePasalAnywhere.FindAllString(joined, -1))
	r.hasPasal = r.pasalCount > 0
	r.hasBab = reBabAnywhere.MatchString(joined)
	r.hasMenimbang = reMenimbang.MatchString(joined)
	r.hasMengingat = reMengingat.MatchString(joined)
	r.hasMemperhatikan = reMemperhatikan.MatchString(joined)
	r.hasMemutuskan = reMemutuskan.MatchString(joined)
	r.hasMenetapkan = reMenetapkan.MatchString(joined)
	r.hasDiktum = hasDiktumAnchor(lines)

	// Aturan kelayakan:
	//  - Ada >=1 "Pasal N" (termasuk angka Romawi "Pasal I/II" — lihat
	//    rePasalAnywhere di patterns.go): kuat -> legal.
	//  - Atau: ada diktum bernomor kata (KESATU/KEDUA/dst, tervalidasi
	//    ordinalWord) — dokumen Instruksi/Keputusan kerap TIDAK punya
	//    Menimbang/Mengingat/Memutuskan SAMA SEKALI, langsung dari kalimat
	//    pembuka ke diktum (ditemukan nyata di debug/34: Instruksi Gubernur
	//    "Dalam rangka menindaklanjuti ..." lalu langsung "KESATU :").
	//  - Atau: ada BAB romawi DAN salah satu kata kunci konsiderans.
	//  - Atau: ADA SALAH SATU dari Menimbang/Mengingat/Memperhatikan/
	//    Memutuskan/Menetapkan.
	//
	// [Diperbaiki 2026-07-24, permintaan user] Aturan terakhir SEBELUMNYA
	// mensyaratkan Menimbang DAN Mengingat DAN Memutuskan SEKALIGUS —
	// menolak dokumen sah yang formatnya menyimpang dari pola itu persis,
	// mis. Surat Edaran yang HANYA punya Menimbang/Mengingat tanpa
	// "MEMUTUSKAN" formal, atau (ditemukan nyata di debug/32) Surat Edaran
	// yang HANYA punya "Memperhatikan" tanpa Menimbang/Mengingat sama
	// sekali. User eksplisit: dokumen dengan salah SATU SAJA dari kata
	// kunci konsiderans ini sudah cukup menunjukkan "formatnya seperti
	// produk hukum" dan harus diterima — jangan ada yang tertolak untuk
	// format begitu.
	switch {
	case r.hasPasal:
		r.isLegal = true
	case r.hasDiktum:
		r.isLegal = true
	case r.hasBab && (r.hasMenimbang || r.hasMengingat || r.hasMemperhatikan || r.hasMemutuskan || r.hasMenetapkan):
		r.isLegal = true
	case r.hasMenimbang || r.hasMengingat || r.hasMemperhatikan || r.hasMemutuskan || r.hasMenetapkan:
		r.isLegal = true
	default:
		r.isLegal = false
	}
	return r
}

// hasDiktumAnchor memindai SETIAP baris lewat detectStructural yang SAMA
// dipakai parseBatangTubuh (bukan regex terpisah) — memastikan definisi
// "ini diktum yang sah" konsisten dengan yang benar-benar dipakai saat
// parsing batang tubuh (tervalidasi ordinalWord, bukan sekadar "KATA :"
// apa saja).
func hasDiktumAnchor(lines []Line) bool {
	for _, l := range lines {
		if detectStructural(strings.TrimSpace(l.Text)).kind == mkDiktum {
			return true
		}
	}
	return false
}

// HasKonsideransAnchor melaporkan apakah teks memuat salah satu penanda
// format produk hukum yang kuat — Menimbang/Mengingat/Memperhatikan/
// Memutuskan/Menetapkan, ATAU diktum bernomor kata (KESATU/KEDUA/dst,
// dicek per baris via hasDiktumAnchor — perlu Line berpisah baris, bukan
// string mentah, karena itu fungsi ini menstitch dulu). Sinyal deterministik
// "formatnya seperti produk hukum" yang dipakai pipeline (lihat
// internal/pipeline/ocr_worker.go) untuk MENGALAHKAN penolakan model teks
// (gerbang produk hukum, atau jenis/wilayah di luar whitelist) —
// permintaan user eksplisit: dokumen dengan ciri ini tidak boleh tertolak,
// apa pun kata model atau apakah jenis/wilayahnya baku. Konsisten dengan
// prinsip "model membaca, kode menyimpulkan" yang dipakai di seluruh
// proyek ini.
func HasKonsideransAnchor(text string) bool {
	switch {
	case reMenimbang.MatchString(text),
		reMengingat.MatchString(text),
		reMemperhatikan.MatchString(text),
		reMemutuskan.MatchString(text),
		reMenetapkan.MatchString(text):
		return true
	}
	return hasDiktumAnchor(stitch([]string{text}))
}

// HasPenjelasanAnchor melaporkan apakah teks memuat baris awal blok
// PENJELASAN (rePenjelasan — sama persis dipakai segmentize untuk memotong
// batang tubuh). Diekspor (2026-07-24) untuk dipakai pipeline
// (internal/pipeline/ocr_worker.go) mendeteksi, PER HALAMAN selagi OCR
// masih berjalan, kapan dokumen memasuki bagian penjelasan resmi — data
// sekunder yang boleh memakai jalur OCR lebih murah (pdftotext/Tesseract)
// daripada model visi penuh.
func HasPenjelasanAnchor(text string) bool {
	return rePenjelasan.MatchString(text)
}

// HasLampiranAnchor melaporkan apakah teks memuat baris awal blok LAMPIRAN
// (reLampiran). Pemakaian sama seperti HasPenjelasanAnchor, TAPI beda
// keputusan hilir: LAMPIRAN sering berisi tabel/peta/struktur organisasi
// yang butuh model visi, jadi pipeline tetap memakai GLM-OCR untuk tier
// ini (hanya Tesseract yang dilewati, bukan model visi-nya).
func HasLampiranAnchor(text string) bool {
	return reLampiran.MatchString(text)
}

// LooksLegal melaporkan apakah teks (gabungan beberapa halaman OCR) sudah
// menunjukkan ciri dokumen perundang-undangan. Dipakai sebagai gate awal saat
// ekstraksi: bila beberapa halaman pertama tidak menunjukkan ciri apa pun, OCR
// halaman berikutnya bisa dibatalkan (hemat, untuk PDF non-peraturan yang tebal).
func LooksLegal(pages []string) bool {
	return classify(stitch(pages)).isLegal
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
		reMemperhatikan.MatchString(text),
		reMemutuskan.MatchString(text),
		reMenetapkan.MatchString(text),
		reJudulPeraturan.MatchString(text):
		return true
	}
	return hasDiktumAnchor(stitch(pages))
}
