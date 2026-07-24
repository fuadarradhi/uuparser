package pipeline

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fuadarradhi/uuparser/internal/extractor"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/parser"
	"github.com/fuadarradhi/uuparser/internal/store"
)

// debug_writer.go menulis keluaran mode-debug (env DEBUG_RESULT=true) untuk
// SATU dokumen ke <DebugDir>/<id>/: sumber.pdf (salinan PDF asli) + ocr.txt
// + thinking.txt (jika ada panggilan model) + parse.txt + parse_tree.json.
//
// TUJUANNYA CUMA SATU (permintaan user, 2026-07-22): mempermudah menyalin
// hasil OCR/parse untuk dikirim ke Claude untuk dipelajari. Bukan fitur untuk
// pengguna akhir biasa — formatnya sengaja teks polos berpenanda jelas per
// bagian, dipilih supaya gampang dibaca ulang oleh Claude saat ditempelkan
// ke percakapan, bukan supaya enak dilihat di UI.
//
// DebugDir SENGAJA folder terpisah dari DataDir (2026-07-22, permintaan
// user) — sebelumnya berada di data/debug/<id>/, tapi data/* seluruhnya
// di-gitignore sehingga isi debug tidak pernah bisa ikut ter-commit. Bawaan
// DebugDir adalah "debug" (sejajar dengan data/log/models/libs), yang TIDAK
// masuk pola gitignore mana pun, jadi bisa langsung "git add" bila memang
// ingin disertakan di repo.
//
// render.pdf DIHAPUS (2026-07-22) — sudah tidak diperlukan lagi; ocr.txt
// saja sudah cukup untuk peninjauan. Dependensi github.com/go-pdf/fpdf ikut
// dilepas dari berkas ini.
//
// Kegagalan menulis debug output TIDAK BOLEH menghentikan pipeline utama —
// ini cuma alat bantu, bukan bagian dari alur data yang harus benar. Karena
// itu setiap kegagalan cuma di-log (logx.Warn), tidak pernah dikembalikan
// sebagai error ke pemanggil.

type debugWriter struct {
	dir       string
	ocr       strings.Builder
	thinking  strings.Builder
	identitas string // ringkasan identitas dokumen, diisi sekali oleh finishClassify
	n         int    // jumlah halaman yang sudah masuk (untuk header)
}

// newDebugWriter menyiapkan folder <debugDir>/<docID>/ untuk SATU dokumen,
// dan menyalin PDF sumbernya (sumber.pdf) ke situ SEKALI — permintaan user
// (2026-07-24): mempermudah membandingkan hasil OCR/parse dengan berkas
// aslinya tanpa harus bolak-balik ke folder data/. Berbeda dari render.pdf
// yang dihapus 2026-07-22 (itu PDF hasil RENDER ULANG dari gambar OCR, lewat
// dependensi go-pdf/fpdf yang sudah dilepas) — sumber.pdf di sini SALINAN
// APA ADANYA dari berkas yang benar-benar diunduh, sekadar io.Copy, TANPA
// dependensi baru sama sekali.
//
// Gagal menyalin (mis. berkas sumber sudah dipindah/dihapus) HANYA di-log,
// TIDAK menggagalkan apa pun — konsisten dengan prinsip debug_writer.go:
// kegagalan alat bantu ini tidak boleh mengganggu pipeline utama.
//
// Mengembalikan nil (bukan struct kosong) bila gagal membuat foldernya,
// supaya nil-safety di seluruh method (lihat di bawah) cukup satu pola.
func newDebugWriter(debugDir string, docID int64, sourcePDFPath string) *debugWriter {
	dir := filepath.Join(debugDir, strconv.FormatInt(docID, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logx.Warn("debug: buat folder %s: %v", dir, err)
		return nil
	}
	copyDebugSourcePDF(dir, sourcePDFPath)
	return &debugWriter{dir: dir}
}

// copyDebugSourcePDF menyalin sourcePDFPath ke <dir>/sumber.pdf. Dilewati
// (bukan disalin ulang) bila sumber.pdf sudah ada — dokumen yang
// dilanjutkan (resume) memanggil newDebugWriter berkali-kali selama
// prosesnya, dan PDF sumber tidak pernah berubah, jadi menyalin ulang
// setiap kali cuma pemborosan I/O tanpa manfaat.
func copyDebugSourcePDF(dir, sourcePDFPath string) {
	if sourcePDFPath == "" {
		return
	}
	dest := filepath.Join(dir, "sumber.pdf")
	if _, err := os.Stat(dest); err == nil {
		return // sudah ada dari penjalanan sebelumnya
	}

	src, err := os.Open(sourcePDFPath)
	if err != nil {
		logx.Warn("debug: buka PDF sumber %s: %v", sourcePDFPath, err)
		return
	}
	defer src.Close()

	out, err := os.Create(dest)
	if err != nil {
		logx.Warn("debug: buat %s: %v", dest, err)
		return
	}
	defer out.Close()

	if _, err := io.Copy(out, src); err != nil {
		logx.Warn("debug: salin PDF sumber ke %s: %v", dest, err)
	}
}

// tambahHalaman menambah SATU halaman ke dump teks OCR. Aman dipanggil pada
// w == nil (mode debug tidak aktif).
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

	fmt.Fprintf(&w.ocr, "--- %s ---\n", label)
	if r.IsEmpty {
		w.ocr.WriteString("(halaman kosong — OCR dilewati)\n\n")
		return
	}
	w.ocr.WriteString(r.Text)
	w.ocr.WriteString("\n\n")
}

// tutup menulis ocr.txt (+ thinking.txt bila ada) ke disk. Dipanggil SEKALI
// di akhir pemrosesan dokumen (lewat defer di processDocument) — apa pun
// hasilnya (selesai, ditolak, gagal), supaya halaman yang sempat diproses
// tetap bisa ditinjau. Aman dipanggil pada w == nil.
func (w *debugWriter) tutup() {
	if w == nil {
		return
	}
	if w.n == 0 {
		return // tidak ada halaman sama sekali — tidak ada yang perlu ditulis
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

// tulisDebugTinjauan menulis tinjauan.txt — dipakai HANYA saat mekanisme
// tinjauan model (2026-07-23, lihat AskTinjauan di thinking.go dan
// pemanggilannya di parser_worker.go) benar-benar terpicu untuk dokumen
// ini. TIDAK ADA berkas ini sama sekali berarti tidak ada node yang
// dicurigai ANCHOR_LEAK sepanjang dokumen — sinyal berguna dengan
// sendirinya, sama seperti absennya thinking.txt berarti identitas/
// penetapan terselesaikan murni deterministik.
func tulisDebugTinjauan(debugDir string, docID int64, isi string) {
	dir := filepath.Join(debugDir, strconv.FormatInt(docID, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logx.Warn("debug: buat folder %s: %v", dir, err)
		return
	}
	// [2026-07-24] Judul digeneralkan — berkas ini sekarang menampung DUA
	// jenis tinjauan (ANCHOR_LEAK dan teks yatim), masing-masing entri
	// NODE di dalamnya sudah menandai sumbernya sendiri lewat baris "==="
	// yang ditulis pemanggil (lihat parser_worker.go).
	full := "=== TINJAUAN MODEL ===\n\n" + isi
	if err := os.WriteFile(filepath.Join(dir, "tinjauan.txt"), []byte(full), 0o644); err != nil {
		logx.Warn("debug: tulis tinjauan.txt: %v", err)
	}
}

// tulisDebugParse menulis parse.txt ke folder debug dokumen ini — dipanggil
// dari parser_worker.go, terpisah dari debugWriter di atas karena parse
// terjadi di worker & waktu yang berbeda (bisa lama setelah OCR selesai),
// bukan dalam satu instance docSink yang sama.
//
// isi sudah dalam bentuk teks jadi (lihat formatNodesUntukDebug) — fungsi ini
// cuma menulisnya ke lokasi yang benar. Tidak menulis apa pun (diam-diam)
// bila DEBUG_RESULT tidak aktif — dicek oleh pemanggil, bukan di sini.
func tulisDebugParse(debugDir string, docID int64, isi string) {
	dir := filepath.Join(debugDir, strconv.FormatInt(docID, 10))
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
	parser.NodeDiktum:   0, // level akar — dokumen Keputusan tak punya Bab/Pasal di atasnya
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
		// Section DITAMPILKAN eksplisit di sini (2026-07-22, permintaan
		// user) — sebelumnya dump ini hanya print NodeType, sehingga poin
		// Menimbang dan Mengingat (sama-sama node_type "item") terlihat
		// tercampur jadi satu kategori padahal Section-nya sudah benar
		// berbeda sejak semula. Ini murni perbaikan tampilan; datanya
		// sendiri sudah benar terpisah di parse_snapshot/nodes.
		fmt.Fprintf(&b, "%s[%s/%s] %s (hal %d-%d)\n", indent, n.Section, n.NodeType, label, n.StartPage, n.EndPage)
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
	case parser.NodeDiktum:
		p = n.Diktum
	}
	if p == nil {
		return ""
	}
	return *p
}

// ---- parse_tree.json: dump berpohon (parent-child eksplisit) ----
//
// formatNodesUntukDebug di atas sudah pakai indentasi visual, tapi tetap
// berupa daftar DATAR — hubungan parent-child harus ditebak dari indentasi.
// debugTreeNode di bawah membangun POHON SUNGGUHAN dari ParentIdx yang sama
// persis dipakai untuk INSERT ke DB (lihat mapNodesToInserts di
// parser_worker.go), supaya siapa-anak-siapa langsung terlihat dari
// struktur JSON-nya sendiri, bukan dari indentasi teks — berguna saat
// meninjau dokumen dengan hierarki dalam (Bab > Bagian > Paragraf > Pasal >
// Ayat) di mana indentasi teks saja mudah disalahbaca.
type debugTreeNode struct {
	Section   string           `json:"section"`
	NodeType  string           `json:"node_type"`
	Label     string           `json:"label,omitempty"`
	Content   string           `json:"content,omitempty"`
	StartPage int              `json:"start_page"`
	EndPage   int              `json:"end_page"`
	Warnings  json.RawMessage  `json:"warnings,omitempty"`
	Children  []*debugTreeNode `json:"children,omitempty"`
}

// buildDebugTree menyusun ulang nodeRows (flat, dengan ParentIdx relatif ke
// slice yang sama) menjadi pohon. TIDAK menghitung ulang parent — memakai
// PERSIS ParentIdx yang sama yang sudah dipakai InsertParseResult, supaya
// pohon di parse_tree.json dijamin sama dengan yang benar-benar tersimpan
// di kolom parent_id database.
func buildDebugTree(rows []store.NodeInsert) []*debugTreeNode {
	nodes := make([]*debugTreeNode, len(rows))
	for i, r := range rows {
		lbl := ""
		if r.Label != nil {
			lbl = *r.Label
		}
		nodes[i] = &debugTreeNode{
			Section: r.Section, NodeType: r.NodeType, Label: lbl,
			Content: r.Content, StartPage: r.StartPage, EndPage: r.EndPage,
			Warnings: json.RawMessage(r.Warnings),
		}
	}
	var roots []*debugTreeNode
	for i, r := range rows {
		if r.ParentIdx >= 0 && r.ParentIdx < len(nodes) {
			nodes[r.ParentIdx].Children = append(nodes[r.ParentIdx].Children, nodes[i])
		} else {
			roots = append(roots, nodes[i])
		}
	}
	return roots
}

// formatNodeTreeJSON menghasilkan teks JSON siap-tulis dari buildDebugTree.
// Kegagalan marshal (seharusnya tak pernah terjadi — semua field sudah
// bertipe aman) dilaporkan sebagai objek JSON kosong, bukan panik, karena
// ini cuma alat bantu debug (lihat catatan di kepala berkas).
func formatNodeTreeJSON(status string, rows []store.NodeInsert) string {
	payload := struct {
		Status string           `json:"status"`
		Nodes  []*debugTreeNode `json:"nodes"`
	}{Status: status, Nodes: buildDebugTree(rows)}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		logx.Warn("debug: marshal parse_tree.json: %v", err)
		return "{}"
	}
	return string(b)
}

// tulisDebugParseTree menulis parse_tree.json ke folder debug dokumen ini —
// dipanggil bersamaan dengan tulisDebugParse dari parser_worker.go.
func tulisDebugParseTree(debugDir string, docID int64, isi string) {
	dir := filepath.Join(debugDir, strconv.FormatInt(docID, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logx.Warn("debug: buat folder %s: %v", dir, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "parse_tree.json"), []byte(isi), 0o644); err != nil {
		logx.Warn("debug: tulis parse_tree.json: %v", err)
	}
}
