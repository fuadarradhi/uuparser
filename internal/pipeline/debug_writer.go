package pipeline

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fuadarradhi/uuparser/internal/extractor"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/parser"
	"github.com/fuadarradhi/uuparser/internal/store"
	"github.com/go-pdf/fpdf"
)

// debug_writer.go menulis keluaran mode-debug (env DEBUG_RESULT=true) untuk
// SATU dokumen: sebuah PDF berisi PERSIS gambar yang dikirim ke model OCR
// (berlabel DPI & skor ketajaman), dan dump teks hasil OCR + hasil parse.
//
// TUJUANNYA CUMA SATU (permintaan user, 2026-07-22): mempermudah menyalin
// hasil OCR/parse untuk dikirim ke Claude untuk dipelajari. Bukan fitur untuk
// pengguna akhir biasa — formatnya sengaja teks polos berpenanda jelas per
// bagian, dipilih supaya gampang dibaca ulang oleh Claude saat ditempelkan
// ke percakapan, bukan supaya enak dilihat di UI.
//
// Kegagalan menulis debug output TIDAK BOLEH menghentikan pipeline utama —
// ini cuma alat bantu, bukan bagian dari alur data yang harus benar. Karena
// itu setiap kegagalan cuma di-log (logx.Warn), tidak pernah dikembalikan
// sebagai error ke pemanggil.

type debugWriter struct {
	dir       string
	pdf       *fpdf.Fpdf
	ocr       strings.Builder
	thinking  strings.Builder
	identitas string // ringkasan identitas dokumen, diisi sekali oleh finishClassify
	n         int    // jumlah halaman yang sudah masuk (untuk header)
}

// newDebugWriter menyiapkan folder data/debug/<docID>/ untuk SATU dokumen.
// Mengembalikan nil (bukan struct kosong) bila gagal membuat foldernya,
// supaya nil-safety di seluruh method (lihat di bawah) cukup satu pola.
func newDebugWriter(dataDir string, docID int64) *debugWriter {
	dir := filepath.Join(dataDir, "debug", strconv.FormatInt(docID, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logx.Warn("debug: buat folder %s: %v", dir, err)
		return nil
	}
	return &debugWriter{dir: dir, pdf: fpdf.New("P", "mm", "A4", "")}
}

// tambahHalaman menambah SATU halaman ke PDF debug (gambar + label DPI) dan
// ke dump teks OCR. Aman dipanggil pada w == nil (mode debug tidak aktif).
func (w *debugWriter) tambahHalaman(r extractor.PageResult) {
	if w == nil {
		return
	}
	w.n++

	label := fmt.Sprintf("Halaman %d — DPI %d", r.Page, r.DPI)
	if r.BlurScoreProbe > 0 {
		label += fmt.Sprintf(" (blur_score probe: %.0f)", r.BlurScoreProbe)
	} else {
		label += " (DPI mengikuti halaman 1)"
	}

	if len(r.PNG) > 0 {
		w.pdf.AddPage()
		w.pdf.SetFont("Helvetica", "", 10)
		w.pdf.SetXY(10, 8)
		w.pdf.CellFormat(190, 6, label, "", 1, "L", false, 0, "")
		opt := fpdf.ImageOptions{ImageType: "PNG"}
		name := fmt.Sprintf("hal%d", r.Page)
		w.pdf.RegisterImageOptionsReader(name, opt, bytes.NewReader(r.PNG))
		// Lebar 190mm (margin A4 wajar kiri-kanan 10mm), tinggi 0 = ikuti
		// rasio gambar aslinya — supaya proporsi halaman tidak gepeng/molor.
		w.pdf.ImageOptions(name, 10, 16, 190, 0, false, opt, 0, "")
	}

	fmt.Fprintf(&w.ocr, "--- %s ---\n", label)
	if r.IsEmpty {
		w.ocr.WriteString("(halaman kosong — OCR dilewati)\n\n")
		return
	}
	w.ocr.WriteString(r.Text)
	w.ocr.WriteString("\n\n")
}

// tutup menulis render.pdf dan ocr.txt ke disk. Dipanggil SEKALI di akhir
// pemrosesan dokumen (lewat defer di processDocument) — apa pun hasilnya
// (selesai, ditolak, gagal), supaya halaman yang sempat diproses tetap bisa
// ditinjau. Aman dipanggil pada w == nil.
func (w *debugWriter) tutup() {
	if w == nil {
		return
	}
	if w.n == 0 {
		return // tidak ada halaman sama sekali — tidak ada yang perlu ditulis
	}
	if err := w.pdf.OutputFileAndClose(filepath.Join(w.dir, "render.pdf")); err != nil {
		logx.Warn("debug: tulis render.pdf: %v", err)
	}

	header := fmt.Sprintf("=== HASIL OCR — %d halaman ===\n\n", w.n)
	if w.identitas != "" {
		header += "--- IDENTITAS DOKUMEN (hasil classify) ---\n" + w.identitas +
			"(lihat thinking.txt bila kolom KEPUTUSAN di atas = model teks — " +
			"berisi PERSIS apa yang ditanyakan & dijawab)\n\n"
	} else {
		header += "--- IDENTITAS DOKUMEN ---\n(belum sampai tahap classify — lihat catatan halaman di bawah)\n\n"
	}
	isi := header + w.ocr.String()
	if err := os.WriteFile(filepath.Join(w.dir, "ocr.txt"), []byte(isi), 0o644); err != nil {
		logx.Warn("debug: tulis ocr.txt: %v", err)
	}

	// thinking.txt HANYA ditulis kalau memang ada pemanggilan model —
	// dokumen yang identitasnya lolos lewat regex (lihat
	// identity_trigger.go) tidak pernah memanggil model sama sekali, jadi
	// TIDAK adanya berkas ini pun sinyal yang berguna: berarti keputusannya
	// murni deterministik.
	if w.thinking.Len() > 0 {
		isi := "=== PANGGILAN MODEL TEKS (gate.md / identity.md / penetapan.md) ===\n\n" + w.thinking.String()
		if err := os.WriteFile(filepath.Join(w.dir, "thinking.txt"), []byte(isi), 0o644); err != nil {
			logx.Warn("debug: tulis thinking.txt: %v", err)
		}
	}
}

// catatIdentitas mencatat KESIMPULAN classify — diterima (jenis/wilayah/
// nomor/tahun/tentang lengkap) ATAU ditolak (lihat sumber="DITOLAK — ...")
// — beserta SUMBER keputusannya (regex atau model). Ditulis ke bagian atas
// ocr.txt lewat tutup(). Dipanggil dari finishClassify (diterima/duplikat)
// MAUPUN langsung dari classify() di kedua titik penolakan — supaya dokumen
// yang ditolak pun punya ringkasan di ocr.txt, bukan cuma pesan generik
// "belum sampai tahap classify". Aman dipanggil pada w == nil.
//
// Ini bagian yang paling penting ditinjau kalau parser salah menyimpulkan
// sesuatu: mempertemukan teks OCR mentah dengan kesimpulan akhirnya dalam
// SATU berkas, supaya langsung kelihatan di titik mana penalaran meleset —
// salah baca OCR, salah pola regex, atau model yang salah menjawab.
func (w *debugWriter) catatIdentitas(sumber string, meta store.DocMeta) {
	if w == nil {
		return
	}
	w.identitas = fmt.Sprintf(
		"Diputuskan oleh  : %s\nJenis            : %s\nWilayah          : %s\nInstansi tertulis: %s\nNomor            : %s\nTahun            : %s\nTentang          : %s\n\n",
		sumber, meta.Jenis, meta.Wilayah, meta.InstansiTertulis, meta.Nomor, meta.Tahun, meta.Tentang)
}

// catatModel mencatat SATU pemanggilan model teks (gate/identity/penetapan)
// ke thinking.txt: prompt yang dipakai, teks yang dikirim, dan jawaban
// MENTAH model — SEBELUM divalidasi/dinormalisasi kode (lihat thinking.go).
//
// Ini yang membedakan thinking.txt dari ocr.txt/parse.txt: dua berkas itu
// menunjukkan HASIL AKHIR (yang sudah lolos validasi kode); thinking.txt
// menunjukkan APA SEBENARNYA yang dijawab model — termasuk jawaban yang
// GAGAL divalidasi. Kalau kesimpulan akhir salah padahal OCR-nya benar,
// berkas inilah yang menunjukkan apakah salahnya di model atau di kode
// validasinya. Aman dipanggil pada w == nil.
func (w *debugWriter) catatModel(tahap, promptTerpakai, masukan, jawabanMentah string, callErr error) {
	if w == nil {
		return
	}
	fmt.Fprintf(&w.thinking, "=== %s ===\n", tahap)
	w.thinking.WriteString("--- PROMPT ---\n")
	w.thinking.WriteString(promptTerpakai)
	w.thinking.WriteString("\n\n--- TEKS YANG DIKIRIM ---\n")
	w.thinking.WriteString(masukan)
	w.thinking.WriteString("\n\n--- JAWABAN MENTAH MODEL ---\n")
	if jawabanMentah == "" {
		w.thinking.WriteString("(kosong)")
	} else {
		w.thinking.WriteString(jawabanMentah)
	}
	w.thinking.WriteString("\n")
	if callErr != nil {
		fmt.Fprintf(&w.thinking, "--- GAGAL DIURAI: %v ---\n", callErr)
	}
	w.thinking.WriteString("\n")
}

// tulisDebugParse menulis parse.txt ke folder debug dokumen ini — dipanggil
// dari parser_worker.go, terpisah dari debugWriter di atas karena parse
// terjadi di worker & waktu yang berbeda (bisa lama setelah OCR selesai),
// bukan dalam satu instance docSink yang sama.
//
// isi sudah dalam bentuk teks jadi (lihat formatNodesUntukDebug) — fungsi ini
// cuma menulisnya ke lokasi yang benar. Tidak menulis apa pun (diam-diam)
// bila DEBUG_RESULT tidak aktif — dicek oleh pemanggil, bukan di sini.
func tulisDebugParse(dataDir string, docID int64, isi string) {
	dir := filepath.Join(dataDir, "debug", strconv.FormatInt(docID, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logx.Warn("debug: buat folder %s: %v", dir, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "parse.txt"), []byte(isi), 0o644); err != nil {
		logx.Warn("debug: tulis parse.txt: %v", err)
	}
}

// nodeIndentLevel memetakan node_type ke kedalaman indentasi untuk dump
// teks debug — sekadar untuk keterbacaan, TIDAK dipakai untuk apa pun di
// luar berkas ini.
var nodeIndentLevel = map[parser.NodeType]int{
	parser.NodeBab:      0,
	parser.NodeBagian:   1,
	parser.NodeParagraf: 2,
	parser.NodePasal:    2,
	parser.NodeAyat:     3,
}

// formatNodesUntukDebug menyusun hasil parse jadi teks polos beri-indentasi —
// format dipilih supaya gampang ditempel ke percakapan dengan Claude, bukan
// supaya cantik ditampilkan. Isi TEKS SETIAP NODE ditulis LENGKAP (tidak
// dipotong): tujuannya meninjau kualitas parsing, memotong teks akan
// menyembunyikan justru bagian yang mungkin perlu ditinjau.
func formatNodesUntukDebug(status string, nodes []parser.Node) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== HASIL PARSE — status=%s — %d node ===\n\n", status, len(nodes))
	for _, n := range nodes {
		depth := nodeIndentLevel[n.NodeType]
		indent := strings.Repeat("  ", depth)
		label := labelUntukDebug(n)
		fmt.Fprintf(&b, "%s[%s] %s (hal %d-%d)\n", indent, n.NodeType, label, n.StartPage, n.EndPage)
		if n.Text != "" {
			for _, line := range strings.Split(n.Text, "\n") {
				fmt.Fprintf(&b, "%s  %s\n", indent, line)
			}
		}
		for _, w := range n.Warnings {
			fmt.Fprintf(&b, "%s  ! PERINGATAN: %s\n", indent, w.Message)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// labelUntukDebug mengambil label level node ITU SENDIRI (bukan ancestor)
// untuk header baris — sumbernya sama seperti ownLevelLabel di
// parser_worker.go, disalin di sini supaya debug_writer.go tidak perlu
// bergantung ke urutan definisi di berkas lain.
func labelUntukDebug(n parser.Node) string {
	var p *string
	switch n.NodeType {
	case parser.NodeBab:
		p = n.Bab
	case parser.NodeBagian:
		p = n.Bagian
	case parser.NodeParagraf:
		p = n.Paragraf
	case parser.NodePasal:
		p = n.Pasal
	case parser.NodeAyat:
		p = n.Ayat
	}
	if p == nil {
		return ""
	}
	return *p
}
