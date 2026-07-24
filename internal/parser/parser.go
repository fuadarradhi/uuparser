package parser

import (
	"regexp"
	"strings"
)

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
		case SectionMenimbang, SectionMengingat, SectionMemperhatikan:
			b = parseFlat(s.section, s.lines)
		case SectionPenetapan:
			b = parsePenetapan(s.lines)
		case SectionBatangTubuh:
			b = parseBatangTubuh(s.lines)
		case SectionPenjelasanUmum:
			b = parsePenjelasanUmum(s.lines)
		case SectionPenjelasanPasal:
			b = parsePenjelasanPasal(s.lines)
		case SectionLampiran:
			b = parseLampiran(s.lines)
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

	// IsDictum/IsTitle (2026-07-24): satu pass terpusat di sini, BUKAN diset
	// di tiap sub-parser — sama seperti alasan IsAppendix dulu, supaya tidak
	// ada titik emit yang lupa menyetelnya. Lihat classifyContentFlags &
	// catatan IsDictum/IsTitle di types.go.
	for i := range all {
		all[i].IsDictum, all[i].IsTitle = classifyContentFlags(all[i].Section, all[i].NodeType)
	}

	if len(all) == 0 {
		docWarns = append(docWarns, Warning{
			Severity: SeverityNeedsReview,
			Message:  "Tidak ada node terbentuk meskipun terklasifikasi sebagai dokumen hukum",
		})
	}

	return Result{Nodes: all, DocumentWarnings: docWarns}, nil
}

// classifyContentFlags menentukan IsDictum/IsTitle sebuah node murni dari
// (Section, NodeType) — lihat catatan lengkap di types.go pada field
// IsDictum/IsTitle Node. Dikumpulkan di satu fungsi (bukan diset tersebar di
// tiap sub-parser) supaya aturannya mudah diaudit/diubah di satu tempat.
func classifyContentFlags(section Section, nt NodeType) (isDictum, isTitle bool) {
	switch nt {
	case NodePasal, NodeAyat, NodeDiktum:
		// Pasal/Ayat yang SAMA juga muncul di section penjelasan_pasal
		// (komentar, bukan aturan mengikat) — hanya yang di batang_tubuh
		// yang benar-benar dictum.
		return section == SectionBatangTubuh, false
	case NodeJudul, NodePembukaan, NodePenetapan, NodeBab, NodeBagian, NodeParagraf, NodeSectionHeader:
		return false, true
	case NodeParagrafIsi:
		// NodeParagrafIsi dipakai di penutup (boilerplate tempat-tanggal-ttd
		// — label/formalitas, bukan isi) DAN di penjelasan_umum (komentar
		// substantif, bukan label) — harus dibedakan lewat section.
		return false, section == SectionPenutup
	default:
		// NodeItem (poin Menimbang/Mengingat/Memperhatikan): data preamble
		// yang nyata, bukan judul, tapi juga bukan dictum yang mengikat.
		return false, false
	}
}

// reJudulFieldLine menandai baris yang SELALU jadi field/node tersendiri di
// blok Judul, tak peduli ada baris kosong sebelumnya atau tidak: "NOMOR ..."
// (nomor peraturan) dan "TENTANG" (penanda, berdiri sendiri). Baris LAIN yang
// menyambung tanpa baris kosong di antaranya (mis. judul yang wrap 2+ baris
// cetak) digabung jadi SATU node — lihat parseJudul.
var reJudulFieldLine = regexp.MustCompile(`(?i)^(NOMOR\b|TENTANG$)`)

// parseJudul memperlakukan blok awal (nama & nomor peraturan, frasa pembuka)
// sebagai baris-baris judul/pembukaan.
//
// [Diperbaiki 2026-07-23] Sebelumnya SETIAP baris OCR langsung jadi Node
// baru tanpa syarat — judul yang tercetak wrap 2+ baris (mis. "PENETAPAN
// PERPANJANGAN STATUS...\nBENCANA HIDROMETEOROLOGI DI ACEH", satu kalimat
// yang kepotong lebar kertas) berakhir sebagai node_judul terpisah-pisah,
// padahal itu satu judul. Sekarang baris kosong pada Line (SUDAH disimpan
// stitch.go sebagai penanda batas paragraf asli, sebelumnya dibuang begitu
// saja di sini) dipakai sebagai batas: node baru HANYA dibuka ketika (a)
// belum ada node aktif, (b) ada baris kosong sejak baris sebelumnya, (c)
// jenis node berubah (mis. dari judul ke pembukaan "DENGAN RAHMAT..."), atau
// (d) baris ini sendiri ATAU baris SEBELUMNYA cocok reJudulFieldLine (field
// "NOMOR .../"TENTANG" selalu berdiri sendiri, tak pernah menyambung ke
// tetangganya meski tanpa baris kosong di antaranya). Selain itu, baris
// digabung ke node aktif (appendText) sebagai sambungan kalimat yang sama.
func parseJudul(lines []Line) *builder {
	b := newBuilder(SectionJudul)
	boundary := true // baris pertama selalu membuka node baru
	prevNT := NodeType("")
	prevWasField := false

	for _, raw := range lines {
		line := strings.TrimSpace(raw.Text)
		if line == "" {
			boundary = true
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
		isField := reJudulFieldLine.MatchString(strings.TrimSpace(up))

		startNew := boundary || b.activeIdx < 0 || nt != prevNT || isField || prevWasField
		if startNew {
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
		} else {
			b.appendText(line)
		}
		boundary = false
		prevNT = nt
		prevWasField = isField
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
