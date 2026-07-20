package parser

import (
	"regexp"
	"strings"
)

// relations.go mengekstrak relasi antar-peraturan dari hasil parsing:
// peraturan yang DICABUT, DIUBAH, atau dijadikan DASAR HUKUM oleh dokumen ini.
//
// Sengaja deterministik (regex + kata kerja), bukan lewat LLM: kalimat pencabutan
// dalam peraturan Indonesia sangat formulaik, sehingga pola dapat diandalkan DAN
// hasilnya dapat diaudit. Model bahasa berisiko mengarang nomor/tahun peraturan —
// kesalahan yang sulit terdeteksi. Rujukan yang polanya tidak yakin tetap
// dikeluarkan dengan Confidence="perlu_review" agar ditinjau manusia.

// RelationType jenis hubungan terhadap peraturan lain.
type RelationType string

const (
	RelMencabut   RelationType = "mencabut"    // dicabut & dinyatakan tidak berlaku
	RelMengubah   RelationType = "mengubah"    // perubahan atas peraturan lain
	RelDasarHukum RelationType = "dasar_hukum" // disebut pada Mengingat
	RelDisebut    RelationType = "disebut"     // dirujuk, maksud hubungan belum jelas
)

// Confidence tingkat keyakinan ekstraksi.
const (
	ConfTinggi      = "tinggi"
	ConfPerluReview = "perlu_review"
)

// Relation satu rujukan ke peraturan lain.
type Relation struct {
	Type RelationType `json:"type"`

	// Key adalah kunci kanonik untuk join di database, dibentuk dari
	// jenis dasar + nomor + tahun, mis. "peraturan_daerah|5|2010".
	// Sengaja TIDAK memuat nama instansi agar bisa dicocokkan lintas dokumen.
	Key string `json:"key"`

	Jenis    string `json:"jenis"`              // jenis dasar, mis. "PERATURAN DAERAH"
	Instansi string `json:"instansi,omitempty"` // penyerta bila ada, mis. "KABUPATEN CONTOH"

	Nomor      string  `json:"nomor"`             // mis. "5"
	Tahun      string  `json:"tahun"`             // mis. "2010"
	Tentang    string  `json:"tentang,omitempty"` // judul bila tercantum
	Confidence string  `json:"confidence"`
	Section    Section `json:"section"` // bagian dokumen tempat ditemukan
	Pasal      *string `json:"pasal,omitempty"`
	DocOrder   float64 `json:"doc_order"` // node tempat rujukan ditemukan
	Kutipan    string  `json:"kutipan"`   // kalimat sumber (untuk audit manual)
}

// reCitation menangkap rujukan peraturan: <jenis> [pengubah] Nomor N Tahun YYYY [tentang ...].
var reCitation = regexp.MustCompile(
	`(?i)(Undang-Undang Dasar|Undang-Undang|Peraturan Pemerintah Pengganti Undang-Undang|` +
		`Peraturan Pemerintah|Peraturan Presiden|Peraturan Menteri|Peraturan Daerah|` +
		`Peraturan Gubernur|Peraturan Bupati|Peraturan Walikota|Peraturan Wali Kota|` +
		`Qanun Aceh|Qanun|Keputusan Presiden|Instruksi Presiden|Keputusan Menteri)` +
		`((?:\s+[^,;.()]{1,50}?)?)\s+Nomor\s+([0-9]+[A-Za-z]?)\s+Tahun\s+([0-9]{4})` +
		`(?:\s+[Tt]entang\s+([^.;]{3,200}))?`)

// kata kerja penanda hubungan.
var (
	reCabut  = regexp.MustCompile(`(?i)\b(dicabut|tidak berlaku|dinyatakan tidak berlaku|mencabut)\b`)
	reUbah   = regexp.MustCompile(`(?i)\b(diubah|perubahan atas|perubahan kedua atas|perubahan ketiga atas|mengubah)\b`)
	reSpaces = regexp.MustCompile(`\s+`)
)

// ExtractRelations memindai seluruh node dan mengembalikan rujukan peraturan lain.
// Rujukan identik (jenis+nomor+tahun+type) hanya dikeluarkan sekali.
func ExtractRelations(res Result) []Relation {
	var out []Relation
	seen := map[string]bool{}

	for _, n := range res.Nodes {
		text := strings.TrimSpace(n.Text)
		if text == "" {
			continue
		}
		matches := reCitation.FindAllStringSubmatch(text, -1)
		if matches == nil {
			continue
		}
		for _, m := range matches {
			jenis := normalizeSpace(m[1])
			instansi := normalizeSpace(m[2])
			rel := Relation{
				Jenis:    strings.ToUpper(jenis),
				Instansi: strings.ToUpper(instansi),
				Nomor:    normalizeSpace(m[3]),
				Tahun:    normalizeSpace(m[4]),
				Tentang:  trimTentang(normalizeSpace(m[5])),
				Section:  n.Section,
				Pasal:    n.Pasal,
				DocOrder: n.DocOrder,
				Kutipan:  trimQuote(text),
			}
			rel.Type, rel.Confidence = classifyRelation(n, text)
			rel.Key = canonicalKey(rel.Jenis, rel.Nomor, rel.Tahun)

			dedup := string(rel.Type) + "#" + rel.Key
			if seen[dedup] {
				continue
			}
			seen[dedup] = true
			out = append(out, rel)
		}
	}
	return suppressWeakDuplicates(out)
}

// suppressWeakDuplicates membuang entri "disebut" bila peraturan yang sama (Key
// sama) sudah punya klasifikasi yang lebih kuat (mencabut/mengubah/dasar_hukum).
// Tujuannya mengurangi kebisingan saat peninjauan manual: satu peraturan cukup
// muncul dengan hubungan terkuatnya.
func suppressWeakDuplicates(rels []Relation) []Relation {
	strong := map[string]bool{}
	for _, r := range rels {
		if r.Type != RelDisebut {
			strong[r.Key] = true
		}
	}
	out := rels[:0]
	for _, r := range rels {
		if r.Type == RelDisebut && strong[r.Key] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// classifyRelation menentukan jenis hubungan dari konteks node.
func classifyRelation(n Node, text string) (RelationType, string) {
	switch n.Section {
	case SectionMengingat:
		// Daftar "Mengingat" memang dasar hukum — pola sangat baku.
		return RelDasarHukum, ConfTinggi
	case SectionPenjelasanUmum, SectionPenjelasanPasal:
		// Penjelasan hanya menjelaskan; jangan diperlakukan sebagai tindakan hukum.
		return RelDisebut, ConfPerluReview
	}
	switch {
	case reCabut.MatchString(text):
		return RelMencabut, ConfTinggi
	case reUbah.MatchString(text):
		return RelMengubah, ConfTinggi
	default:
		// Dirujuk di batang tubuh tanpa kata kerja pencabutan/perubahan yang jelas.
		return RelDisebut, ConfPerluReview
	}
}

func normalizeSpace(s string) string {
	return strings.TrimSpace(reSpaces.ReplaceAllString(s, " "))
}

func trimQuote(s string) string {
	s = normalizeSpace(s)
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}

// reTentangTail memotong judul peraturan pada kata kerja/klausa lanjutan yang
// bukan bagian judul (mis. "... tentang Izin Gangguan dicabut dan dinyatakan ...").
var reTentangTail = regexp.MustCompile(`(?i)\s+(dicabut|diubah|dinyatakan|sebagaimana|menjadi|dihapus|berlaku|mulai berlaku|dan dinyatakan|sebagai berikut)\b.*$`)

// trimTentang membersihkan judul hasil tangkapan regex.
func trimTentang(s string) string {
	s = reTentangTail.ReplaceAllString(s, "")
	s = strings.TrimRight(s, " ,;:.")
	return strings.TrimSpace(s)
}

// canonicalKey membentuk kunci join yang stabil: jenis_dasar|nomor|tahun.
// Contoh: ("PERATURAN DAERAH", "5", "2010") -> "peraturan_daerah|5|2010".
func canonicalKey(jenis, nomor, tahun string) string {
	j := strings.ToLower(strings.TrimSpace(jenis))
	j = strings.Join(strings.Fields(j), "_")
	j = strings.ReplaceAll(j, "-", "_")
	return j + "|" + strings.ToLower(strings.TrimSpace(nomor)) + "|" + strings.TrimSpace(tahun)
}
