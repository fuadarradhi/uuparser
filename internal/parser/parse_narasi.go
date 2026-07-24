package parser

import "strings"

// parse_narasi.go menangani dokumen TANPA anchor struktural apa pun — bukan
// cuma tanpa Pasal, tapi juga tanpa Menimbang/Mengingat/Memperhatikan/
// Memutuskan/Menetapkan/BAB/Diktum SAMA SEKALI. Dipakai HANYA lewat jalur
// ParseAllowNonRegulation (lihat parser.go & segmentize di segment.go) untuk
// jenis dokumen yang memang SAH berformat begitu (mis. Surat Edaran naratif
// biasa) — Peraturan/Qanun/Keputusan asli TIDAK PERNAH masuk jalur ini
// karena selalu punya Menimbang/Mengingat/Diktum, jadi segmentize() akan
// selalu menemukan batangStart lewat jalur normal (SectionBatangTubuh)
// untuk dokumen semacam itu.
//
// Poin bernomor (1. 2. 3...) atau berhuruf (a. b. c...) jadi NodeItem
// (label terisi) — reuse detectStructural yang SAMA dipakai parseFlat/
// parseBatangTubuh, supaya definisi "ini poin bernomor yang sah" konsisten
// di seluruh parser, BUKAN regex terpisah. Baris lain (kop surat, tanggal,
// alamat tujuan, paragraf pembuka/penutup, tanda tangan) jadi NodeParagrafIsi
// datar tanpa label — TIDAK ada upaya mengenali strukturnya lebih jauh
// (mis. memisahkan kop dari paragraf pembuka); itu di luar cakupan yang
// diminta untuk perbaikan ini.
//
// KETERBATASAN YANG DISENGAJA: sub-poin angka Romawi (mis. "I. Jadwal
// Pelaksanaan...", ditemukan nyata di dokumen uji user pada poin 11) TIDAK
// dikenali detectStructural sebagai anchor tersendiri (fungsi itu hanya
// mengenali romawi untuk BAB) — baris begitu ikut tersambung sebagai
// lanjutan teks poin induknya (appendText), bukan pecah jadi node terpisah.
// Cukup untuk sekarang; bisa ditambah nanti bila benar-benar dibutuhkan.
func parseNarasi(lines []Line) *builder {
	b := newBuilder(SectionNarasi)

	handleItem := func(m lineMatch) bool {
		switch m.kind {
		case mkHuruf:
			b.emitItem(m.label, "", m.text)
			return true
		case mkAngka:
			b.emitItem("", m.label, m.text)
			return true
		}
		return false
	}

	boundary := true // baris pertama selalu membuka node baru
	for _, raw := range lines {
		line := strings.TrimSpace(raw.Text)
		if line == "" {
			boundary = true
			continue
		}
		b.curLinePage = raw.Page

		if handleItem(detectStructural(line)) {
			boundary = false
			continue
		}

		// Bukan poin bernomor/berhuruf: paragraf biasa. Baris SETELAH
		// baris kosong membuka node baru (paragraf baru); tanpa itu,
		// disambung ke node aktif (baris cetak yang wrap/melebar).
		if boundary || b.activeIdx < 0 {
			b.oiPasal += orderStep
			b.nodes = append(b.nodes, Node{
				Section:    SectionNarasi,
				NodeType:   NodeParagrafIsi,
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
	}
	return b
}
