// Package textcheck membandingkan hasil OCR (model visi) dengan lapisan
// teks yang sudah tertanam di PDF (diambil lewat `pdftotext`, poppler-utils)
// untuk halaman yang SAMA, supaya pipeline bisa memutuskan: dokumen ini
// aman memakai `pdftotext` untuk sisa halamannya (jauh lebih murah, tanpa
// model visi sama sekali), atau tetap perlu di-OCR penuh.
//
// Paket ini SENGAJA tidak tahu apa pun soal Postgres, extractor, atau aturan
// bisnis dokumen — hanya dua hal: menjalankan `pdftotext`, dan membandingkan
// dua string. Keputusan "pakai yang mana" ada di internal/pipeline
// (lihat resolveTextSource di ocr_worker terkait).
package textcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ErrUnavailable dikembalikan bila biner `pdftotext` tidak ada di PATH.
var ErrUnavailable = errors.New("textcheck: biner pdftotext tidak ditemukan di PATH (install poppler-utils)")

// Available memeriksa apakah `pdftotext` ada di PATH. Dipanggil sekali per
// dokumen oleh pemanggil (bukan di-cache di sini) — biayanya kecil
// (exec.LookPath, bukan proses baru) dan menghindari status basi bila biner
// dipasang/dilepas selagi service berjalan.
func Available() bool {
	_, err := exec.LookPath("pdftotext")
	return err == nil
}

// ExtractRange menjalankan `pdftotext -layout -f a -l b file -` dan
// mengembalikan teks GABUNGAN halaman a..b (dipakai untuk probe 1 halaman:
// a==b).
//
// -layout dipakai (bukan mode alir bebas bawaan) supaya baris tetap
// mengikuti tata letak halaman aslinya — dokumen legal Indonesia sangat
// bergantung pada Pasal/Ayat berdiri di barisnya sendiri, dan parser hilir
// (internal/parser) mengandalkan itu. Ini pilihan yang WAJAR tapi belum
// diuji terhadap keluaran parser sungguhan — bila hasil parse dari halaman
// pdftotext ternyata lebih buruk daripada dari OCR, coba dulu tanpa -layout
// sebelum mencurigai bagian lain.
func ExtractRange(ctx context.Context, pdfPath string, from, to int) (string, error) {
	if !Available() {
		return "", ErrUnavailable
	}
	cmd := exec.CommandContext(ctx, "pdftotext", "-layout",
		"-f", strconv.Itoa(from), "-l", strconv.Itoa(to), pdfPath, "-")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftotext hal %d-%d: %w (%s)", from, to, err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

// ExtractPages menjalankan `pdftotext` SEKALI dari halaman `from` sampai
// akhir dokumen, lalu memecah hasilnya per halaman berdasarkan form-feed
// (\f) yang disisipkan poppler di tiap batas halaman — jauh lebih murah
// daripada memanggil pdftotext satu kali per halaman untuk dokumen tebal.
//
// nAsli adalah jumlah halaman ASLI dokumen (dari extractor.PageCount).
// Dipakai untuk menjamin potongan selalu berjumlah (nAsli-from+1) elemen
// walau form-feed di halaman terakhir kadang hilang atau dokumennya punya
// form-feed liar di tengah teks (dianggap bagian dari halaman itu, bukan
// dipotong lagi — lihat pemangkasan di bawah).
func ExtractPages(ctx context.Context, pdfPath string, from, nAsli int) ([]string, error) {
	if !Available() {
		return nil, ErrUnavailable
	}
	if from > nAsli {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "pdftotext", "-layout", "-f", strconv.Itoa(from), pdfPath, "-")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pdftotext dari hal %d: %w (%s)", from, err, strings.TrimSpace(stderr.String()))
	}

	want := nAsli - from + 1
	pages := strings.Split(out.String(), "\f")

	// poppler biasanya menyisipkan \f SETELAH tiap halaman TERMASUK yang
	// terakhir, jadi split menghasilkan satu elemen kosong ekstra di ujung.
	if len(pages) > want && strings.TrimSpace(pages[len(pages)-1]) == "" {
		pages = pages[:len(pages)-1]
	}
	// Jaga-jaga terhadap dokumen dengan form-feed tak beraturan: potong atau
	// tambal dengan string kosong daripada memetakan nomor halaman keliru.
	// Ini best-effort — bila terpicu, artinya asumsi "1 \f = 1 batas
	// halaman" dilanggar dokumen ini; layak dicek manual kalau sering
	// terjadi.
	if len(pages) > want {
		pages = pages[:want]
	}
	for len(pages) < want {
		pages = append(pages, "")
	}
	return pages, nil
}

// ErrTesseractUnavailable dikembalikan bila biner `tesseract` tidak ada di
// PATH.
var ErrTesseractUnavailable = errors.New("textcheck: biner tesseract tidak ditemukan di PATH (install tesseract-ocr + tesseract-ocr-ind)")

// TesseractAvailable memeriksa apakah `tesseract` ada di PATH.
func TesseractAvailable() bool {
	_, err := exec.LookPath("tesseract")
	return err == nil
}

// RunTesseract menjalankan `tesseract stdin stdout -l <lang>` atas SATU
// gambar (PNG dari memori, tanpa berkas sementara) dan mengembalikan
// teksnya. lang kosong berarti "ind" — PASTIKAN paket bahasa
// tesseract-ocr-ind terpasang (bukan cuma tesseract-ocr bawaan yang
// biasanya cuma menyertakan "eng"), atau imbuhan dan istilah hukum bahasa
// Indonesia akan berantakan.
//
// Dipakai HANYA untuk halaman tier hemat (lihat internal/pipeline/tier.go)
// — TIDAK PERNAH untuk halaman batang tubuh, yang tetap wajib GLM-OCR.
func RunTesseract(ctx context.Context, png []byte, lang string) (string, error) {
	if !TesseractAvailable() {
		return "", ErrTesseractUnavailable
	}
	if lang == "" {
		lang = "ind"
	}
	cmd := exec.CommandContext(ctx, "tesseract", "stdin", "stdout", "-l", lang)
	cmd.Stdin = bytes.NewReader(png)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tesseract: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

// SimilarityThreshold: ambang kemiripan (setelah normalisasi whitespace)
// supaya lapisan teks PDF dianggap "sama" dengan hasil OCR. Konstanta kode,
// bukan .env — beda dari AmbangJelas/AmbangSedang milik extractor (yang
// memang perlu dikalibrasi per korpus), threshold ini bukan tentang gambar
// sehingga tidak butuh kalibrasi ulang tiap sumber. Naikkan/turunkan di
// sini langsung kalau di lapangan ternyata perlu.
const SimilarityThreshold = 0.95

var wsRe = regexp.MustCompile(`\s+`)
var digitRe = regexp.MustCompile(`\d+`)

// CompareResult merangkum kecocokan OCR vs pdftotext untuk halaman yang SAMA.
type CompareResult struct {
	// Similarity: 1 - (jarak-edit ternormalisasi / panjang terpanjang).
	// Dihitung dari teks yang whitespace-nya sudah diratakan, jadi beda
	// perataan/pembungkusan baris antara OCR dan pdftotext TIDAK menghukum
	// skor ini — yang dihukum adalah beda KARAKTER.
	Similarity float64
	// DigitsMatch: urutan SEMUA digit di halaman itu identik antara OCR dan
	// pdftotext. Ini gerbang TERPISAH dari Similarity dengan sengaja: teks
	// panjang bisa mencapai kemiripan >95% walau ada satu-dua digit yang
	// salah (mis. "Pasal 5" jadi "Pasal 8") — satu digit itu cuma menyumbang
	// sebagian kecil dari total kemiripan, tapi fatal untuk parser yang
	// membaca nomor Pasal/Ayat. CATATAN: false-negative mungkin muncul bila
	// AUTO_CROP memotong footer/nomor halaman dari gambar yang di-OCR
	// padahal pdftotext tetap menyertakannya — kalau dokumen yang jelas
	// identik tetap ditolak gerbang ini, cek dulu apakah itu penyebabnya.
	DigitsMatch bool
	// Trusted = Similarity >= SimilarityThreshold DAN DigitsMatch.
	Trusted bool
}

// normalize meratakan whitespace (spasi/newline berulang -> satu spasi) dan
// melipat ke huruf kecil, supaya beda perataan baris antara OCR dan
// pdftotext tidak dihitung sebagai perbedaan karakter.
func normalize(s string) string {
	s = strings.ToLower(s)
	s = wsRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// Compare membandingkan teks OCR dan teks pdftotext untuk halaman yang SAMA.
func Compare(ocrText, pdfText string) CompareResult {
	a, b := normalize(ocrText), normalize(pdfText)

	dist := levenshtein(a, b)
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	sim := 1.0
	if maxLen > 0 {
		sim = 1 - float64(dist)/float64(maxLen)
	}

	digitsMatch := sameDigitSequence(ocrText, pdfText)

	return CompareResult{
		Similarity:  sim,
		DigitsMatch: digitsMatch,
		Trusted:     sim >= SimilarityThreshold && digitsMatch,
	}
}

// sameDigitSequence membandingkan SEMUA runtun digit di kedua teks (bukan
// versi ternormalisasi — kapitalisasi tidak relevan untuk digit) secara
// berurutan dan persis. Bukan hitung-per-halaman yang toleran: satu beda
// saja dianggap TIDAK cocok, sesuai catatan di CompareResult.DigitsMatch.
func sameDigitSequence(a, b string) bool {
	da := digitRe.FindAllString(a, -1)
	db := digitRe.FindAllString(b, -1)
	if len(da) != len(db) {
		return false
	}
	for i := range da {
		if da[i] != db[i] {
			return false
		}
	}
	return true
}

// levenshtein: jarak-edit iteratif dua-baris (hemat memori — cukup untuk
// teks per-halaman, ribuan karakter, bukan jutaan). Pure Go, tanpa
// dependency eksternal, konsisten dengan gaya paket lain di uuparser.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}
