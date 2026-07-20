package parser

import "strings"

// builder.go menyediakan infrastruktur bersama untuk semua sub-parser:
//   - menyimpan konteks label ancestor aktif (bab/bagian/paragraf/pasal/ayat/huruf),
//   - memberi OrderIndex (lokal per parent) & DocOrder (global) yang renggang,
//   - menampung baris lanjutan (continuation) ke node aktif,
//   - menampung baris yatim (orphan) menjadi Warning pada node tetangga.

const (
	docStep   = 1000.0 // jarak antar DocOrder
	orderStep = 1000.0 // jarak antar OrderIndex dalam satu parent
)

type builder struct {
	section Section
	nodes   []Node

	docCursor float64

	// konteks label aktif
	curBab      string
	curBagian   string
	curParagraf string
	curPasal    string
	curAyat     string
	curHuruf    string

	// order lokal per level (di-reset saat parent berganti)
	oiBab, oiBagian, oiParagraf, oiPasal, oiAyat, oiHuruf, oiAngka float64

	// index node aktif (untuk append continuation / attach warning)
	activeIdx int

	// buffer orphan yang belum punya node sebelumnya untuk ditempeli
	pendingOrphan []string

	// leftoverDoc: orphan yang tak punya node sama sekali di segmen ini,
	// diangkat ke level dokumen oleh caller.
	leftoverDoc []string
}

func newBuilder(section Section) *builder {
	return &builder{section: section, activeIdx: -1}
}

func (b *builder) nextDoc() float64 {
	b.docCursor += docStep
	return b.docCursor
}

// emit menambahkan node baru dengan konteks label saat ini & order yang diberikan.
func (b *builder) emit(nt NodeType, oi float64, text string) {
	n := Node{
		Section:    b.section,
		NodeType:   nt,
		Bab:        ptr(b.curBab),
		Bagian:     ptr(b.curBagian),
		Paragraf:   ptr(b.curParagraf),
		Pasal:      ptr(b.curPasal),
		Ayat:       ptr(b.curAyat),
		Huruf:      ptr(b.curHuruf),
		OrderIndex: oi,
		DocOrder:   b.nextDoc(),
		Text:       text,
	}
	b.nodes = append(b.nodes, n)
	b.activeIdx = len(b.nodes) - 1

	// bila ada orphan tertunda, tempel sebagai warning "before" pada node ini.
	if len(b.pendingOrphan) > 0 {
		b.attachOrphan(b.activeIdx, "before", b.pendingOrphan)
		b.pendingOrphan = nil
	}
}

// setLabelField meng-override field label spesifik pada node aktif (mis. huruf/angka),
// karena emit hanya menyalin konteks curXxx.
func (b *builder) setActiveHuruf(h string) {
	if b.activeIdx >= 0 {
		b.nodes[b.activeIdx].Huruf = ptr(h)
	}
}
func (b *builder) setActiveAngka(a string) {
	if b.activeIdx >= 0 {
		b.nodes[b.activeIdx].Angka = ptr(a)
	}
}

// appendText menambahkan baris lanjutan ke teks node aktif. Bila belum ada node
// aktif, baris masuk ke buffer orphan.
func (b *builder) appendText(line string) {
	t := strings.TrimSpace(line)
	if t == "" {
		return
	}
	if b.activeIdx < 0 {
		b.pendingOrphan = append(b.pendingOrphan, t)
		return
	}
	cur := &b.nodes[b.activeIdx]
	if cur.Text == "" {
		cur.Text = t
	} else {
		cur.Text += " " + t
	}
}

// attachOrphan menempelkan warning teks-yatim pada node index idx.
func (b *builder) attachOrphan(idx int, position string, texts []string) {
	if idx < 0 || idx >= len(b.nodes) {
		return
	}
	joined := strings.Join(texts, "\n")
	b.nodes[idx].Warnings = append(b.nodes[idx].Warnings, Warning{
		Severity:   SeverityNeedsReview,
		Message:    "Teks tidak dikenali struktur; perlu tinjauan / kemungkinan sisip baris",
		OrphanText: &joined,
		Position:   position,
	})
}

// warnActive menambahkan warning non-orphan ke node aktif.
func (b *builder) warnActive(sev Severity, msg string) {
	if b.activeIdx < 0 {
		return
	}
	b.nodes[b.activeIdx].Warnings = append(b.nodes[b.activeIdx].Warnings, Warning{
		Severity: sev, Message: msg,
	})
}

// ---- transisi konteks (dipanggil sub-parser saat menemukan penanda) ----

func (b *builder) openBab(label, title string) {
	b.curBab = "BAB " + label
	b.curBagian, b.curParagraf, b.curPasal, b.curAyat, b.curHuruf = "", "", "", "", ""
	b.oiBab += orderStep
	b.oiBagian, b.oiParagraf, b.oiPasal, b.oiAyat, b.oiHuruf, b.oiAngka = 0, 0, 0, 0, 0, 0
	b.emit(NodeBab, b.oiBab, title)
}

func (b *builder) openBagian(label, title string) {
	b.curBagian = "Bagian " + label
	b.curParagraf, b.curPasal, b.curAyat, b.curHuruf = "", "", "", ""
	b.oiBagian += orderStep
	b.oiParagraf, b.oiPasal, b.oiAyat, b.oiHuruf, b.oiAngka = 0, 0, 0, 0, 0
	b.emit(NodeBagian, b.oiBagian, title)
}

func (b *builder) openParagraf(label, title string) {
	b.curParagraf = label
	b.curPasal, b.curAyat, b.curHuruf = "", "", ""
	b.oiParagraf += orderStep
	b.oiPasal, b.oiAyat, b.oiHuruf, b.oiAngka = 0, 0, 0, 0
	b.emit(NodeParagraf, b.oiParagraf, title)
}

func (b *builder) openPasal(label string) {
	b.curPasal = label
	b.curAyat, b.curHuruf = "", ""
	b.oiPasal += orderStep
	b.oiAyat, b.oiHuruf, b.oiAngka = 0, 0, 0
	b.emit(NodePasal, b.oiPasal, "")
}

func (b *builder) openAyat(label, text string) {
	b.curAyat = label
	b.curHuruf = ""
	b.oiAyat += orderStep
	b.oiHuruf, b.oiAngka = 0, 0
	b.emit(NodeAyat, b.oiAyat, text)
}

func (b *builder) openHuruf(label, text string) {
	b.curHuruf = label
	b.oiHuruf += orderStep
	b.oiAngka = 0
	b.emit(NodeHuruf, b.oiHuruf, text)
	b.setActiveHuruf(label)
}

func (b *builder) openAngka(label, text string) {
	b.oiAngka += orderStep
	b.emit(NodeAngka, b.oiAngka, text)
	b.setActiveAngka(label)
}

// flushOrphan dipanggil di akhir segmen: bila masih ada orphan tertunda tanpa
// node berikutnya, tempel ke node terakhir sebagai "after"; jika tak ada node
// sama sekali, kembalikan true agar caller mengangkatnya ke level dokumen.
func (b *builder) flushOrphan() (leftover []string, hadNoNode bool) {
	if len(b.pendingOrphan) == 0 {
		return nil, false
	}
	if len(b.nodes) == 0 {
		lo := b.pendingOrphan
		b.pendingOrphan = nil
		return lo, true
	}
	b.attachOrphan(len(b.nodes)-1, "after", b.pendingOrphan)
	b.pendingOrphan = nil
	return nil, false
}
