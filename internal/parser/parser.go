package parser

import "strings"

// Parse adalah satu-satunya entry point publik. Menerima teks OCR per halaman,
// mengembalikan Result berisi Nodes (siap loop-insert ke DB) dan DocumentWarnings.
//
// Alur: stitch (tandai halaman) -> classify(gate) -> segmentize -> parse per-segmen -> gabung.
func Parse(pages []string) (Result, error) {
	if len(pages) == 0 {
		return Result{}, ErrEmptyInput
	}

	lines := stitch(pages)
	if len(lines) == 0 {
		return Result{}, ErrEmptyInput
	}

	cls := classify(lines)
	if !cls.isLegal {
		return Result{}, ErrNotLegalDocument
	}

	segs, docWarns := segmentize(lines, cls)

	var all []Node
	for _, s := range segs {
		var b *builder
		switch s.section {
		case SectionJudul:
			b = parseJudul(s.lines)
		case SectionMenimbang, SectionMengingat:
			b = parseFlat(s.section, s.lines)
		case SectionPenetapan:
			b = parsePenetapan(s.lines)
		case SectionBatangTubuh:
			b = parseBatangTubuh(s.lines)
		case SectionPenjelasanUmum:
			b = parsePenjelasanUmum(s.lines)
		case SectionPenjelasanPasal:
			b = parsePenjelasanPasal(s.lines)
		default:
			continue
		}
		all = append(all, b.nodes...)
		if len(b.leftoverDoc) > 0 {
			joined := strings.Join(b.leftoverDoc, "\n")
			docWarns = append(docWarns, Warning{
				Severity:   SeverityNeedsReview,
				Message:    "Teks tak terstruktur pada section " + string(s.section),
				OrphanText: &joined,
			})
		}
	}

	// DocOrder global monotonik lintas segmen (OrderIndex tetap lokal per parent).
	for i := range all {
		all[i].DocOrder = float64(i+1) * docStep
	}

	if len(all) == 0 {
		docWarns = append(docWarns, Warning{
			Severity: SeverityNeedsReview,
			Message:  "Tidak ada node terbentuk meskipun terklasifikasi sebagai dokumen hukum",
		})
	}

	return Result{Nodes: all, DocumentWarnings: docWarns}, nil
}

// parseJudul memperlakukan blok awal (nama & nomor peraturan, frasa pembuka) sebagai
// baris-baris judul/pembukaan.
func parseJudul(lines []Line) *builder {
	b := newBuilder(SectionJudul)
	for _, raw := range lines {
		line := strings.TrimSpace(raw.Text)
		if line == "" {
			continue
		}
		b.curLinePage = raw.Page
		nt := NodeJudul
		up := strings.ToUpper(line)
		if strings.Contains(up, "RAHMAT TUHAN") ||
			strings.Contains(up, "PRESIDEN") && strings.Contains(up, "REPUBLIK INDONESIA") && len(line) < 60 ||
			strings.HasPrefix(up, "DENGAN ") {
			nt = NodePembukaan
		}
		b.oiPasal += orderStep
		b.nodes = append(b.nodes, Node{
			Section:    SectionJudul,
			NodeType:   nt,
			OrderIndex: b.oiPasal,
			DocOrder:   b.nextDoc(),
			Text:       line,
			StartPage:  raw.Page,
			EndPage:    raw.Page,
		})
		b.activeIdx = len(b.nodes) - 1
	}
	return b
}

// parsePenetapan menangani blok MEMUTUSKAN / Menetapkan.
func parsePenetapan(lines []Line) *builder {
	b := newBuilder(SectionPenetapan)
	for _, raw := range lines {
		line := strings.TrimSpace(raw.Text)
		if line == "" {
			continue
		}
		b.curLinePage = raw.Page
		if reMemutuskan.MatchString(line) {
			b.emitPenetapan(line)
			continue
		}
		if reMenetapkan.MatchString(line) {
			b.emitPenetapan(line)
			continue
		}
		// lanjutan (mis. judul peraturan yang ditetapkan).
		if b.activeIdx >= 0 {
			b.appendText(line)
		} else {
			b.emitPenetapan(line)
		}
	}
	return b
}

func (b *builder) emitPenetapan(text string) {
	b.oiPasal += orderStep
	b.nodes = append(b.nodes, Node{
		Section:    SectionPenetapan,
		NodeType:   NodePenetapan,
		OrderIndex: b.oiPasal,
		DocOrder:   b.nextDoc(),
		Text:       text,
		StartPage:  b.curLinePage,
		EndPage:    b.curLinePage,
	})
	b.activeIdx = len(b.nodes) - 1
}
