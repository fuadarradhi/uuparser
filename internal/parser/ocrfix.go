package parser

import (
	"regexp"
	"strings"
)

// ocrfix.go memperbaiki kesalahan OCR yang berulang & khas pada dokumen hukum,
// dijalankan per-baris SEBELUM deteksi pola. Hanya menyentuh pola bernomor di
// AWAL baris (penanda ayat/angka), agar tidak merusak isi teks di tengah kalimat.

var (
	// GLM-OCR mengeluarkan Markdown dan sering membungkusnya dengan pagar kode
	// (```markdown / ```text / ```). Baris pagar bukan isi dokumen.
	reCodeFence = regexp.MustCompile("^`{3,}[a-zA-Z]*$")
	// Heading Markdown: "# BAB I" -> "BAB I".
	reMdHeading = regexp.MustCompile(`^#{1,6}\s+`)
	// Penekanan Markdown: **Pasal 1** / __Pasal 1__ -> Pasal 1.
	reMdBold = regexp.MustCompile(`(\*\*|__)(.+?)(\*\*|__)`)
	// Baris pemisah tabel Markdown: |---|---|
	reMdTableSep = regexp.MustCompile(`^\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?$`)

	// Penanda ayat di awal baris yang OCR-nya keliru:
	//  (l) (I) (i) -> (1) ; huruf l/I/i yang berdiri sendiri dalam kurung ayat.
	reAyatOCRDigit = regexp.MustCompile(`^\(\s*[lIi]\s*\)`)
	// (l1) (1l) dst: campuran l/I di antara digit pada label ayat.
	reAyatMixed = regexp.MustCompile(`^\((\s*[0-9lIi]+\s*[a-zA-Z]?\s*)\)`)
	// Bullet OCR yang seharusnya huruf/angka kadang jadi "o." / "0." di awal baris.
)

// fixOCRLine menormalkan satu baris. Mengembalikan baris yang sudah dirapikan.
func fixOCRLine(line string) string {
	s := line

	// Rapikan spasi ganda & trim.
	s = strings.TrimSpace(s)
	s = collapseSpaces(s)
	if s == "" {
		return s
	}

	// Buang sisa penanda Markdown dari keluaran OCR sebelum pencocokan pola,
	// agar "# BAB I" atau "**Pasal 1**" tetap dikenali sebagai penanda struktur.
	if reCodeFence.MatchString(s) || reMdTableSep.MatchString(s) {
		return ""
	}
	s = reMdHeading.ReplaceAllString(s, "")
	s = reMdBold.ReplaceAllString(s, "$2")
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}

	// Perbaiki penanda ayat OCR: (l)/(I)/(i) -> (1).
	if reAyatOCRDigit.MatchString(s) {
		s = reAyatOCRDigit.ReplaceAllString(s, "(1)")
	} else if m := reAyatMixed.FindStringSubmatch(s); m != nil {
		inner := m[1]
		fixed := fixAyatInner(inner)
		if fixed != inner {
			s = "(" + fixed + ")" + s[len(m[0]):]
		}
	}

	return s
}

// fixAyatInner mengganti l/I -> 1 dan O -> 0 HANYA pada bagian numerik label ayat,
// menyisakan huruf sisipan (mis. "1a") apa adanya.
func fixAyatInner(inner string) string {
	inner = strings.TrimSpace(inner)
	var b strings.Builder
	for i, r := range inner {
		switch r {
		case 'l', 'I':
			// jadi digit '1' bila bukan huruf sisipan di posisi akhir.
			if isTrailingLetterSuffix(inner, i) {
				b.WriteRune(r)
			} else {
				b.WriteRune('1')
			}
		case 'O':
			if isTrailingLetterSuffix(inner, i) {
				b.WriteRune(r)
			} else {
				b.WriteRune('0')
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isTrailingLetterSuffix true bila posisi i adalah huruf sisipan di akhir (mis. "1a").
func isTrailingLetterSuffix(s string, i int) bool {
	// huruf sisipan hanya valid bila SEBELUMNYA sudah ada minimal satu digit
	// dan ini karakter non-digit terakhir.
	if i == 0 {
		return false
	}
	hasDigitBefore := false
	for _, r := range s[:i] {
		if r >= '0' && r <= '9' {
			hasDigitBefore = true
			break
		}
	}
	rest := s[i+1:]
	return hasDigitBefore && strings.TrimSpace(rest) == ""
}

var reMultiSpace = regexp.MustCompile(`[ \t]{2,}`)

func collapseSpaces(s string) string {
	return reMultiSpace.ReplaceAllString(s, " ")
}
