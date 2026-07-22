package parser

import "strings"

// builder.go menyediakan infrastruktur bersama untuk semua sub-parser:
//   - menyimpan konteks label ancestor aktif (bab/bagian/paragraf/pasal/ayat/huruf),
//   - memberi OrderIndex (lokal per parent) & DocOrder (global) yang renggang,
//   - melacak StartPage/EndPage node aktif dari curLinePage,
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
	curHuruf    string // bookkeeping saja sejak huruf tak lagi jadi node (lihat foldHuruf)
	curDiktum   string // "KESATU"/"KEDUA"/dst — eksklusif dgn curPasal (satu dokumen tak pernah pakai keduanya)

	// order lokal per level (di-reset saat parent berganti)
	oiBab, oiBagian, oiParagraf, oiPasal, oiAyat, oiHuruf, oiAngka float64

	// index node aktif (untuk append continuation / attach warning)
	activeIdx int

	// curLinePage adalah halaman baris yang SEDANG diproses oleh sub-parser
	// pemanggil, di-set sebelum tiap panggilan emit/append di dalam loop-nya.
	// Dipakai emit()/extendPage() untuk mengisi StartPage/EndPage node otomatis.
	curLinePage int

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
		Diktum:     ptr(b.curDiktum),
		OrderIndex: oi,
		DocOrder:   b.nextDoc(),
		Text:       text,
		StartPage:  b.curLinePage,
		EndPage:    b.curLinePage,
	}
	b.nodes = append(b.nodes, n)
	b.activeIdx = len(b.nodes) - 1

	// bila ada orphan tertunda, tempel sebagai warning "before" pada node ini.
	if len(b.pendingOrphan) > 0 {
		b.attachOrphan(b.activeIdx, "before", b.pendingOrphan)
		b.pendingOrphan = nil
	}
}

// extendPage memperluas EndPage node aktif ke curLinePage (dipanggil tiap kali
// teks ditambahkan ke node yang sudah ada, sehingga node yang melintasi
// beberapa halaman punya EndPage yang benar).
func (b *builder) extendPage() {
	if b.activeIdx < 0 || b.curLinePage == 0 {
		return
	}
	n := &b.nodes[b.activeIdx]
	if n.StartPage == 0 {
		n.StartPage = b.curLinePage
	}
	if b.curLinePage > n.EndPage {
		n.EndPage = b.curLinePage
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
	b.extendPage()
}

// appendLabeledLine menambah baris berlabel (huruf/angka batang tubuh) sebagai
// BARIS BARU (dipisah newline, bukan digabung jadi satu kalimat) pada teks node
// aktif, TANPA membuat node terpisah — lihat foldHuruf/foldAngka. Newline
// menjaga agar daftar huruf/angka tetap terlihat sebagai daftar saat dibaca
// atau di-split lagi belakangan, meski disimpan sebagai satu Node.
func (b *builder) appendLabeledLine(label, text string) {
	t := strings.TrimSpace(text)
	line := label
	if t != "" {
		line = label + " " + t
	}
	if b.activeIdx < 0 {
		b.pendingOrphan = append(b.pendingOrphan, line)
		return
	}
	cur := &b.nodes[b.activeIdx]
	if cur.Text == "" {
		cur.Text = line
	} else {
		cur.Text += "\n" + line
	}
	b.extendPage()
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
	b.curBagian, b.curParagraf, b.curPasal, b.curAyat, b.curHuruf, b.curDiktum = "", "", "", "", "", ""
	b.oiBab += orderStep
	b.oiBagian, b.oiParagraf, b.oiPasal, b.oiAyat, b.oiHuruf, b.oiAngka = 0, 0, 0, 0, 0, 0
	b.emit(NodeBab, b.oiBab, title)
}

func (b *builder) openBagian(label, title string) {
	b.curBagian = "Bagian " + label
	b.curParagraf, b.curPasal, b.curAyat, b.curHuruf, b.curDiktum = "", "", "", "", ""
	b.oiBagian += orderStep
	b.oiParagraf, b.oiPasal, b.oiAyat, b.oiHuruf, b.oiAngka = 0, 0, 0, 0, 0
	b.emit(NodeBagian, b.oiBagian, title)
}

func (b *builder) openParagraf(label, title string) {
	b.curParagraf = label
	b.curPasal, b.curAyat, b.curHuruf, b.curDiktum = "", "", "", ""
	b.oiParagraf += orderStep
	b.oiPasal, b.oiAyat, b.oiHuruf, b.oiAngka = 0, 0, 0, 0
	b.emit(NodeParagraf, b.oiParagraf, title)
}

func (b *builder) openPasal(label string) {
	b.curPasal = label
	b.curAyat, b.curHuruf, b.curDiktum = "", "", ""
	b.oiPasal += orderStep
	b.oiAyat, b.oiHuruf, b.oiAngka = 0, 0, 0
	b.emit(NodePasal, b.oiPasal, "")
}

// openDiktum membuka satu poin Diktum (KESATU/KEDUA/dst) — padanan openPasal
// untuk dokumen Keputusan/Instruksi yang tak berstruktur Bab/Pasal/Ayat.
// Memakai kanal order yang sama dengan Pasal (oiPasal) karena keduanya
// EKSKLUSIF dalam satu dokumen (tidak pernah campur), sehingga tidak perlu
// kanal order baru. curPasal/curAyat/curHuruf direset supaya folding
// huruf/angka di bawah (lihat parseBatangTubuh) menempel ke Diktum aktif,
// bukan ke Pasal basi dari segmen lain.
func (b *builder) openDiktum(label, text string) {
	b.curDiktum = label
	b.curPasal, b.curAyat, b.curHuruf = "", "", ""
	b.oiPasal += orderStep
	b.oiAyat, b.oiHuruf, b.oiAngka = 0, 0, 0
	b.emit(NodeDiktum, b.oiPasal, text)
}

func (b *builder) openAyat(label, text string) {
	b.curAyat = label
	b.curHuruf = ""
	b.oiAyat += orderStep
	b.oiHuruf, b.oiAngka = 0, 0
	b.emit(NodeAyat, b.oiAyat, text)
}

// foldHuruf melipat baris huruf (a., b., dst) ke DALAM teks node aktif (Ayat,
// atau Pasal bila belum ada Ayat) alih-alih membuat node terpisah — keputusan
// produk 2026-07-20: huruf/angka dianggap granularitas berlebihan untuk unit
// simpan (memutus konteks kalimat pembuka ayat bila jadi baris DB sendiri).
// curHuruf tetap di-set (tanpa emit node) supaya deteksi Angka di bawahnya
// pada parseBatangTubuh/parsePenjelasanPasal tetap berfungsi.
func (b *builder) foldHuruf(label, text string) {
	b.curHuruf = label
	b.appendLabeledLine(label+".", text)
}

// foldAngka melipat baris angka (1), 2), dst — sub-item di bawah huruf) ke
// dalam teks node aktif yang sama, dengan alasan yang sama seperti foldHuruf.
func (b *builder) foldAngka(label, text string) {
	b.appendLabeledLine("("+label+")", text)
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
