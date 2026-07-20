package parser

import (
	"regexp"
	"strings"
)

// parse_penjelasan.go: parser terpisah untuk penjelasan_umum & penjelasan_pasal.
// Sengaja dipisah dari parse_batangtubuh.go meski pola dasar mirip, agar kekhususan
// penjelasan (mis. "Cukup jelas.", ayat berprefiks kata "Ayat (1)") bisa ditangani
// tanpa berisiko merusak parsing batang tubuh. Keduanya berbagi helper patterns.go.

// Di penjelasan, ayat sering ditulis dengan prefiks kata: "Ayat (1)".
var reAyatWord = regexp.MustCompile(`^(?i:ayat)\s*\(\s*([0-9]+\s*[a-zA-Z]?)\s*\)\s*(.*)$`)

// Header romawi/angka pada Penjelasan Umum, mis. "1." atau "A." sebagai sub-bab naratif.
var rePenjHead = regexp.MustCompile(`^([IVXLCDM]+|[A-Z])\.\s+(.+)$`)

func parsePenjelasanUmum(lines []string) *builder {
	b := newBuilder(SectionPenjelasanUmum)
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// buang baris header "PENJELASAN" / "UMUM" itu sendiri.
		if rePenjelasan.MatchString(line) || reUmumHead.MatchString(line) {
			continue
		}
		// sub-heading naratif (opsional) -> node paragraf baru.
		if m := rePenjHead.FindStringSubmatch(line); m != nil {
			b.emitNarrative(m[2])
			continue
		}
		// paragraf naratif: baris non-kosong memulai/ melanjutkan paragraf.
		// Heuristik: baris yang diakhiri tanda titik dianggap akhir paragraf,
		// tapi untuk kesederhanaan tiap baris kita gabung ke node aktif kecuali
		// belum ada node -> mulai node baru.
		if b.activeIdx < 0 || b.nodes[b.activeIdx].Section != SectionPenjelasanUmum {
			b.emitNarrative(line)
		} else {
			b.appendText(line)
		}
	}
	b.flushOrphan()
	return b
}

// emitNarrative membuat node paragraf naratif untuk Penjelasan Umum.
func (b *builder) emitNarrative(text string) {
	b.oiPasal += orderStep
	n := Node{
		Section:    SectionPenjelasanUmum,
		NodeType:   NodeParagrafIsi,
		OrderIndex: b.oiPasal,
		DocOrder:   b.nextDoc(),
		Text:       text,
	}
	b.nodes = append(b.nodes, n)
	b.activeIdx = len(b.nodes) - 1
}

func parsePenjelasanPasal(lines []string) *builder {
	b := newBuilder(SectionPenjelasanPasal)

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// buang header blok.
		if rePasalDemi.MatchString(line) {
			continue
		}

		// ayat berprefiks kata "Ayat (1)" khas penjelasan.
		if m := reAyatWord.FindStringSubmatch(line); m != nil {
			if b.curPasal == "" {
				// ayat penjelasan tanpa pasal induk: tetap catat, beri warning.
				b.openAyat(normalizeAyatLabel(m[1]), strings.TrimSpace(m[2]))
				b.warnActive(SeverityNeedsReview, "Ayat penjelasan tanpa Pasal induk yang jelas")
			} else {
				b.openAyat(normalizeAyatLabel(m[1]), strings.TrimSpace(m[2]))
			}
			continue
		}

		m := detectStructural(line)
		switch m.kind {
		case mkPasal:
			b.openPasal(m.label)
		case mkAyat:
			b.openAyat(m.label, m.text)
		case mkHuruf:
			if b.curPasal == "" {
				b.appendText(line)
			} else {
				b.openHuruf(m.label, m.text)
			}
		case mkAngka:
			if b.curHuruf == "" {
				b.appendText(line)
			} else {
				b.openAngka(m.label, m.text)
			}
		default:
			// "Cukup jelas." dan penjelasan lain -> teks node aktif (biasanya Pasal/Ayat).
			b.appendText(line)
		}
	}

	if leftover, noNode := b.flushOrphan(); noNode && len(leftover) > 0 {
		b.leftoverDoc = leftover
	}
	return b
}
