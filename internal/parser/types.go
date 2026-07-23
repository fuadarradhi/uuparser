package parser

import "errors"

// ErrNotLegalDocument dikembalikan oleh Parse ketika teks masukan tidak
// menunjukkan ciri dokumen perundang-undangan sama sekali (tidak ada satupun
// anchor kuat seperti "Pasal N", "BAB <romawi>", atau kata kunci konsiderans).
// Caller sebaiknya memperlakukan ini sebagai "lewati dokumen ini", bukan bug.
var ErrNotLegalDocument = errors.New("uuparser: teks tidak dikenali sebagai dokumen hukum")

// ErrEmptyInput dikembalikan ketika tidak ada halaman/teks sama sekali.
var ErrEmptyInput = errors.New("uuparser: input kosong")

// Section adalah bagian besar (macro-section) dokumen.
type Section string

const (
	SectionJudul     Section = "judul"
	SectionMenimbang Section = "menimbang"
	SectionMengingat Section = "mengingat"
	// SectionMemperhatikan (2026-07-23): section opsional antara Mengingat
	// dan Penetapan — lihat catatan reMemperhatikan di patterns.go.
	SectionMemperhatikan   Section = "memperhatikan"
	SectionPenetapan       Section = "penetapan"
	SectionBatangTubuh     Section = "batang_tubuh"
	SectionPenjelasanUmum  Section = "penjelasan_umum"
	SectionPenjelasanPasal Section = "penjelasan_pasal"
	SectionPenutup         Section = "penutup" // pengesahan/tempat-tanggal/ttd di akhir batang tubuh
	// SectionLampiran (2026-07-23): lampiran (attachment) yang menyusul
	// SETELAH tanda tangan penutup — dokumen sendiri, punya identitas
	// ulang (KEPUTUSAN .../NOMOR/TENTANG) dan sub-bagian berlabel huruf
	// (A./B./C./dst) sendiri, BUKAN bagian dari penutup. Ditemukan lewat
	// bug nyata: sebelum ini, sekali reClosing (lihat parse_batangtubuh.go)
	// aktif, ia tetap aktif SELAMANYA sampai akhir dokumen — jadi seluruh
	// isi Lampiran ikut tersedot masuk sebagai node penutup.
	SectionLampiran Section = "lampiran"
)

// NodeType adalah jenis unit struktural pada satu baris.
type NodeType string

const (
	NodeJudul       NodeType = "judul"
	NodePembukaan   NodeType = "pembukaan" // frasa pembuka spt "DENGAN RAHMAT TUHAN YANG MAHA ESA"
	NodeItem        NodeType = "item"      // poin datar di Menimbang/Mengingat (huruf/angka)
	NodePenetapan   NodeType = "penetapan"
	NodeBab         NodeType = "bab"
	NodeBagian      NodeType = "bagian"
	NodeParagraf    NodeType = "paragraf"
	NodePasal       NodeType = "pasal"
	NodeAyat        NodeType = "ayat"
	NodeParagrafIsi NodeType = "paragraf_isi" // paragraf naratif tanpa penomoran (mis. Penjelasan Umum)
	// NodeDiktum ditambahkan 2026-07-22 setelah ditemukan bug: dokumen jenis
	// Keputusan/Instruksi tidak berstruktur Pasal/Ayat, melainkan Diktum
	// bernomor kata (KESATU/KEDUA/KETIGA/dst). Sebelum ini, baris "KESATU :"
	// tidak dikenali sama sekali oleh parseBatangTubuh (hanya kenal Bab/
	// Bagian/Paragraf/Pasal/Ayat/Huruf/Angka), jatuh ke default->appendText
	// tanpa node aktif untuk ditempeli, sehingga SELURUH isi diktum (justru
	// substansi keputusannya) hilang dan hanya muncul sebagai warning
	// generik "Teks tidak dikenali struktur". Lihat parse_batangtubuh.go.
	NodeDiktum NodeType = "diktum"
	// NodeHuruf/NodeAngka SENGAJA DIHAPUS (2026-07-20): huruf/angka pada batang
	// tubuh tidak lagi jadi node terpisah — teksnya dilipat ke dalam Node Ayat
	// (atau Pasal bila belum ada Ayat) oleh builder.foldHuruf/foldAngka, supaya
	// tidak memutus konteks kalimat pembuka ayat. NodeItem (poin Menimbang/
	// Mengingat) TIDAK terpengaruh — itu daftar datar yang berbeda konteks.
)

// Severity tingkat keparahan sebuah warning.
type Severity string

const (
	SeverityInfo        Severity = "info"
	SeverityNeedsReview Severity = "needs_review"
)

// Warning adalah catatan untuk human review. Bisa menempel pada Node (level baris)
// atau berdiri di Result.DocumentWarnings (level dokumen).
type Warning struct {
	Severity   Severity `json:"severity"`
	Message    string   `json:"message"`
	OrphanText *string  `json:"orphan_text,omitempty"` // teks asli yang tidak ter-parse (jika ada)
	Position   string   `json:"position,omitempty"`    // "before" | "after" — relatif terhadap node pembawa
}

// Node adalah satu baris hasil parsing, siap di-loop insert ke DB.
// Field label (Bab..Angka) berisi NOMOR/LABEL LANGSUNG (string), bukan FK ke row lain,
// sehingga filter seperti "semua ayat pada Pasal 2" cukup mencocokkan kolom.
type Node struct {
	Section  Section  `json:"section"`
	NodeType NodeType `json:"node_type"`

	Bab      *string `json:"bab,omitempty"`
	Bagian   *string `json:"bagian,omitempty"`
	Paragraf *string `json:"paragraf,omitempty"`
	Pasal    *string `json:"pasal,omitempty"`
	Ayat     *string `json:"ayat,omitempty"`
	Huruf    *string `json:"huruf,omitempty"`
	Angka    *string `json:"angka,omitempty"`
	Diktum   *string `json:"diktum,omitempty"` // "KESATU"/"KEDUA"/dst — hanya untuk dokumen berstruktur Diktum

	// StartPage/EndPage = halaman OCR (1-indexed) tempat node ini mulai/berakhir.
	// StartPage == EndPage bila node tidak melintasi batas halaman.
	StartPage int `json:"start_page"`
	EndPage   int `json:"end_page"`

	// OrderIndex = urutan lokal dalam parent langsung.
	// DocOrder   = urutan baca linear seluruh dokumen (tidak reset).
	// Keduanya renggang (kelipatan step) agar UI drag-insert tak perlu renumber massal.
	OrderIndex float64 `json:"order_index"`
	DocOrder   float64 `json:"doc_order"`

	Text     string    `json:"text"`
	Warnings []Warning `json:"warnings,omitempty"`

	// IsAppendix (2026-07-23): true HANYA untuk node dari SectionLampiran
	// — lihat parseLampiran. Isi Lampiran berguna saat menelusuri dokumen
	// tapi tidak relevan saat benar-benar mencari isi aturan (Pasal/Ayat/
	// Diktum); flag ini biarkan query/RAG memilih menyertakan atau
	// mengecualikannya tanpa perlu menghapus datanya. Default false untuk
	// SEMUA node lain — jangan diset manual di tempat lain.
	IsAppendix bool `json:"is_appendix,omitempty"`
}

// Result adalah keluaran Parse.
type Result struct {
	Nodes            []Node    `json:"nodes"`
	DocumentWarnings []Warning `json:"document_warnings,omitempty"`
}

// ptr util kecil untuk field label opsional.
func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
