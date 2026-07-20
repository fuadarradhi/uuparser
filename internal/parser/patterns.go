package parser

import (
	"regexp"
	"strings"
)

// ---- Regex level struktur. Semua di-anchor ke awal baris (setelah trim). ----

var (
	reBab      = regexp.MustCompile(`^BAB\s+([IVXLCDM]+|[0-9]+)\b\s*(.*)$`)
	reBagian   = regexp.MustCompile(`^(?i:bagian)\s+(.+?)\s*$`)
	reParagraf = regexp.MustCompile(`^(?i:paragraf)\s+([0-9]+)\b\s*(.*)$`)
	// Pasal: "Pasal 27" atau sisipan "Pasal 27A"/"Pasal 27 A".
	rePasal = regexp.MustCompile(`^(?i:pasal)\s+([0-9]+\s*[A-Za-z]?)\s*$`)
	// Ayat: "(1)", "(1a)", termasuk hasil OCR yang longgar sudah dinormalisasi dulu.
	reAyat = regexp.MustCompile(`^\(\s*([0-9]+\s*[a-zA-Z]?)\s*\)\s*(.*)$`)
	// Huruf: "a.", "ab." (setelah z lanjut aa, ab, ...). Maksimal 2 huruf.
	reHuruf = regexp.MustCompile(`^([a-z]{1,2})\.\s+(.*)$`)
	// Angka: "1." "12)" — sub dari huruf pada batang tubuh, atau item Mengingat.
	reAngka = regexp.MustCompile(`^([0-9]+)[.\)]\s+(.*)$`)
)

// ---- Kata kunci macro-section (dipakai classify & segment). ----

var (
	reMenimbang  = regexp.MustCompile(`(?im)^\s*Menimbang\s*:?`)
	reMengingat  = regexp.MustCompile(`(?im)^\s*Mengingat\s*:?`)
	reMemutuskan = regexp.MustCompile(`(?im)^\s*MEMUTUSKAN\s*:?`)
	reMenetapkan = regexp.MustCompile(`(?im)^\s*Menetapkan\s*:`)
	rePenjelasan = regexp.MustCompile(`(?im)^\s*PENJELASAN\b`)
	rePasalDemi  = regexp.MustCompile(`(?im)^\s*(?:I{1,3}\.\s*)?PASAL\s+DEMI\s+PASAL\b`)
	reUmumHead   = regexp.MustCompile(`(?im)^\s*(?:I\.\s*)?UMUM\s*$`)
	reCukupJelas = regexp.MustCompile(`(?i)^cukup\s+jelas\.?\s*$`)
)

// reBabAnywhere & rePasalAnywhere dipakai gate klasifikasi (boleh di tengah baris).
var (
	reBabAnywhere   = regexp.MustCompile(`(?im)^\s*BAB\s+[IVXLCDM]+\b`)
	rePasalAnywhere = regexp.MustCompile(`(?im)^\s*Pasal\s+[0-9]+`)
)

// matchKind hasil deteksi satu baris.
type matchKind int

const (
	mkNone matchKind = iota
	mkBab
	mkBagian
	mkParagraf
	mkPasal
	mkAyat
	mkHuruf
	mkAngka
)

// lineMatch hasil klasifikasi satu baris oleh detectStructural.
type lineMatch struct {
	kind  matchKind
	label string // nomor/huruf yang dinormalisasi, mis "27A", "1a", "a", "3"
	title string // judul yang menempel di baris yang sama (mis nama Bab), bisa kosong
	text  string // sisa teks isi pada baris yang sama (mis isi ayat), bisa kosong
}

// detectStructural mengklasifikasikan satu baris (sudah di-trim & di-OCR-fix).
// Tidak memutuskan hierarki — hanya "baris ini pola apa".
func detectStructural(line string) lineMatch {
	if m := reBab.FindStringSubmatch(line); m != nil {
		return lineMatch{kind: mkBab, label: normalizeRoman(m[1]), title: strings.TrimSpace(m[2])}
	}
	if m := reParagraf.FindStringSubmatch(line); m != nil {
		return lineMatch{kind: mkParagraf, label: strings.TrimSpace(m[1]), title: strings.TrimSpace(m[2])}
	}
	if m := rePasal.FindStringSubmatch(line); m != nil {
		return lineMatch{kind: mkPasal, label: normalizePasalLabel(m[1])}
	}
	if m := reAyat.FindStringSubmatch(line); m != nil {
		return lineMatch{kind: mkAyat, label: normalizeAyatLabel(m[1]), text: strings.TrimSpace(m[2])}
	}
	// Bagian dicek SETELAH pasal/ayat karena "Bagian" bisa jadi kata biasa;
	// kita batasi hanya bila diikuti kata bilangan tingkat (Kesatu, Kedua, ...).
	if m := reBagian.FindStringSubmatch(line); m != nil {
		title := strings.TrimSpace(m[1])
		if ord := ordinalWord(title); ord != "" {
			return lineMatch{kind: mkBagian, label: ord, title: strings.TrimSpace(strings.TrimPrefix(title, ordinalSurface(title)))}
		}
	}
	if m := reHuruf.FindStringSubmatch(line); m != nil {
		return lineMatch{kind: mkHuruf, label: m[1], text: strings.TrimSpace(m[2])}
	}
	if m := reAngka.FindStringSubmatch(line); m != nil {
		return lineMatch{kind: mkAngka, label: m[1], text: strings.TrimSpace(m[2])}
	}
	return lineMatch{kind: mkNone}
}

// normalizePasalLabel merapikan "27 A" -> "27A", "27a" -> "27A".
func normalizePasalLabel(s string) string {
	s = strings.ReplaceAll(s, " ", "")
	return strings.ToUpper(s)
}

// normalizeAyatLabel merapikan "1 a" -> "1a" (huruf sisipan tetap huruf kecil).
func normalizeAyatLabel(s string) string {
	s = strings.ReplaceAll(s, " ", "")
	return strings.ToLower(s)
}

// normalizeRoman membiarkan romawi apa adanya (uppercase) atau angka arab.
func normalizeRoman(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// ---- Kata bilangan tingkat untuk Bagian ("Bagian Kesatu" dst). ----

var ordinalMap = map[string]string{
	"kesatu": "1", "pertama": "1",
	"kedua": "2", "ketiga": "3", "keempat": "4", "kelima": "5",
	"keenam": "6", "ketujuh": "7", "kedelapan": "8", "kesembilan": "9",
	"kesepuluh": "10", "kesebelas": "11", "kedua belas": "12", "ketiga belas": "13",
	"keempat belas": "14", "kelima belas": "15", "keenam belas": "16",
	"ketujuh belas": "17", "kedelapan belas": "18", "kesembilan belas": "19",
	"kedua puluh": "20",
}

// ordinalWord mengembalikan urutan numerik dari awal string bila diawali kata bilangan tingkat.
func ordinalWord(s string) string {
	low := strings.ToLower(strings.TrimSpace(s))
	// cek frasa dua-kata dulu (mis "kedua belas") baru satu kata.
	for _, k := range []string{
		"kesembilan belas", "kedelapan belas", "ketujuh belas", "keenam belas",
		"kelima belas", "keempat belas", "ketiga belas", "kedua belas",
		"kedua puluh", "kesebelas", "kesepuluh", "kesembilan", "kedelapan",
		"ketujuh", "keenam", "kelima", "keempat", "ketiga", "kedua",
		"kesatu", "pertama",
	} {
		if strings.HasPrefix(low, k) {
			return ordinalMap[k]
		}
	}
	return ""
}

// ordinalSurface mengembalikan bentuk permukaan kata bilangan yang cocok (untuk memotong dari judul).
func ordinalSurface(s string) string {
	low := strings.ToLower(strings.TrimSpace(s))
	for _, k := range []string{
		"kesembilan belas", "kedelapan belas", "ketujuh belas", "keenam belas",
		"kelima belas", "keempat belas", "ketiga belas", "kedua belas",
		"kedua puluh", "kesebelas", "kesepuluh", "kesembilan", "kedelapan",
		"ketujuh", "keenam", "kelima", "keempat", "ketiga", "kedua",
		"kesatu", "pertama",
	} {
		if strings.HasPrefix(low, k) {
			return s[:len(k)]
		}
	}
	return ""
}

// isBlank true bila baris kosong setelah trim.
func isBlank(s string) bool { return strings.TrimSpace(s) == "" }
