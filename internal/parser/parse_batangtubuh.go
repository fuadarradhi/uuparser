package parser

import (
	"regexp"
	"strings"
)

// parse_batangtubuh.go: state machine penuh untuk section batang_tubuh.
// Hierarki: Bab -> Bagian -> Paragraf -> Pasal -> Ayat. Huruf dan Angka TIDAK
// lagi jadi node sendiri (2026-07-20) — teksnya dilipat ke Ayat aktif (atau
// Pasal bila belum ada Ayat) lewat builder.foldHuruf/foldAngka.

// Penanda penutup: setelah ini, konten adalah pengesahan/tempat-tanggal/tanda tangan,
// bukan bagian struktur pasal.
// [UPDATE UU 15/2019 & UU 13/2022] Menambahkan deteksi "Berita Negara" dan "Berita Daerah"
// sebagai sarana pengundangan resmi yang setara dengan Lembaran Negara/Daerah.
var reClosing = regexp.MustCompile(`(?i)^(ditetapkan\s+di|diundangkan\s+di|agar\s+setiap\s+orang|lembaran\s+(negara|daerah)\b|tambahan\s+lembaran|berita\s+(negara|daerah)\b|tambahan\s+berita)`)

func parseBatangTubuh(lines []Line) *builder {
	b := newBuilder(SectionBatangTubuh)
	inClosing := false
	boundary := true // baris pertama pada penutup selalu membuka node baru

	for _, raw := range lines {
		line := strings.TrimSpace(raw.Text)
		if line == "" {
			boundary = true
			continue
		}
		b.curLinePage = raw.Page

		if !inClosing && reClosing.MatchString(line) {
			inClosing = true
		}
		if inClosing {
			// [Diperbaiki 2026-07-23] Sebelumnya SETIAP baris di bagian
			// penutup jadi node paragraf_isi terpisah tanpa syarat — baris
			// lanjutan yang di-wrap (mis. "Ditetapkan di Banda Aceh" lalu
			// "pada tanggal, 7 Januari 2026" tanpa baris kosong di antaranya,
			// keduanya satu kalimat/keterangan yang sama) berakhir sebagai
			// dua paragraf_isi terpisah, padahal itu satu item. Sekarang:
			// node baru HANYA dibuka saat ada baris kosong sejak baris
			// sebelumnya (boundary), belum ada node aktif sama sekali, ATAU
			// baris ini sendiri mencocokkan salah satu penanda baku
			// reClosing (setiap penanda seperti itu SELALU memulai field
			// baru — "Ditetapkan di ..." tak pernah menyambung ke
			// "Diundangkan di ..." sebelumnya meski tanpa baris kosong).
			// Baris lain digabung sebagai sambungan ke node aktif.
			startNew := boundary || b.activeIdx < 0 || b.nodes[b.activeIdx].Section != SectionPenutup ||
				reClosing.MatchString(line)
			if startNew {
				b.emitPenutup(line)
			} else {
				b.appendText(line)
			}
			boundary = false
			continue
		}
		boundary = false

		m := detectStructural(line)
		switch m.kind {
		case mkBab:
			b.openBab(m.label, m.title)
		case mkBagian:
			b.openBagian(m.label, m.title)
		case mkParagraf:
			b.openParagraf(m.label, m.title)
		case mkPasal:
			b.openPasal(m.label)
		case mkAyat:
			b.openAyat(m.label, m.text)
		case mkDiktum:
			b.openDiktum(m.label, m.text)
		case mkHuruf:
			// hanya valid bila sudah ada Pasal ATAU Diktum aktif (keduanya
			// eksklusif); jika tidak, ini kemungkinan teks biasa (mis. daftar
			// dalam kalimat) -> perlakukan sebagai lanjutan.
			if b.curPasal == "" && b.curDiktum == "" {
				b.appendText(line)
			} else {
				b.foldHuruf(m.label, m.text)
			}
		case mkAngka:
			// angka sub-huruf hanya valid bila ada Huruf aktif; jika tidak, lanjutan.
			if (b.curPasal == "" && b.curDiktum == "") || b.curHuruf == "" {
				b.appendText(line)
			} else {
				b.foldAngka(m.label, m.text)
			}
		default:
			b.appendText(line)
		}
	}

	if leftover, noNode := b.flushOrphan(); noNode && len(leftover) > 0 {
		// ditangani oleh caller (Parse) sebagai document warning.
		b.leftoverDoc = leftover
	}
	return b
}

// emitPenutup menambahkan node penutup (paragraf naratif tanpa penomoran).
func (b *builder) emitPenutup(text string) {
	b.oiPasal += orderStep // pakai kanal order pasal agar urut stabil di ekor dokumen
	n := Node{
		Section:    SectionPenutup,
		NodeType:   NodeParagrafIsi,
		OrderIndex: b.oiPasal,
		DocOrder:   b.nextDoc(),
		Text:       text,
		StartPage:  b.curLinePage,
		EndPage:    b.curLinePage,
	}
	b.nodes = append(b.nodes, n)
	b.activeIdx = len(b.nodes) - 1
}
