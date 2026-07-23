package parser

import "strings"

// parse_lampiran.go menangani SectionLampiran (2026-07-23) — lampiran yang
// menyusul SETELAH tanda tangan penutup dokumen utama. Isinya biasanya
// naratif berlabel huruf (A. Latar Belakang, B. Dasar Hukum, C. Maksud &
// Tujuan, dst — pola sama seperti sub-heading di Penjelasan Umum, lihat
// rePenjHead di parse_penjelasan.go), kadang berisi daftar bernomor huruf/
// angka di dalamnya (a./b./c., 1./2./3.) sebagai bagian dari satu paragraf,
// bukan node terpisah — konsisten dengan cara batang tubuh melipat huruf/
// angka ke node aktif (lihat builder.foldHuruf/foldAngka).
//
// Sengaja TIDAK memakai foldHuruf/foldAngka: itu spesifik untuk konteks
// Pasal/Ayat/Diktum (butuh curPasal/curDiktum aktif). Di sini cukup
// appendText biasa — sub-poin huruf/angka dalam satu bagian berlabel A/B/C
// tetap tergabung sebagai teks lanjutan yang sama, sama seperti perilaku
// (yang sudah benar) di parsePenjelasanUmum.
func parseLampiran(lines []Line) *builder {
	b := newBuilder(SectionLampiran)
	boundary := true // baris pertama selalu membuka node baru
	for _, raw := range lines {
		line := strings.TrimSpace(raw.Text)
		if line == "" {
			boundary = true
			continue
		}
		b.curLinePage = raw.Page

		// Sub-heading naratif berlabel huruf/romawi (mis. "A. Latar
		// Belakang") -> SELALU node baru, walau tanpa baris kosong di
		// depannya (heading kadang nempel langsung setelah paragraf
		// sebelumnya tanpa jarak). Baris heading disimpan UTUH (termasuk
		// label "A. ") sebagai awal teks node — beda dari
		// parsePenjelasanUmum yang membuang labelnya; di sini label
		// dipertahankan supaya pembaca tahu urutan bagian aslinya tanpa
		// perlu kolom label terpisah.
		if rePenjHead.MatchString(line) {
			b.oiPasal += orderStep
			b.nodes = append(b.nodes, Node{
				Section:    SectionLampiran,
				NodeType:   NodeParagrafIsi,
				OrderIndex: b.oiPasal,
				DocOrder:   b.nextDoc(),
				Text:       line,
				StartPage:  raw.Page,
				EndPage:    raw.Page,
				IsAppendix: true,
			})
			b.activeIdx = len(b.nodes) - 1
			boundary = false
			continue
		}

		// Baris biasa: baris kosong sebelumnya (atau belum ada node
		// Lampiran sama sekali) -> paragraf baru; selain itu, menyambung
		// (wrap) paragraf yang sama seperti baris sebelumnya.
		if boundary || b.activeIdx < 0 || b.nodes[b.activeIdx].Section != SectionLampiran {
			b.oiPasal += orderStep
			b.nodes = append(b.nodes, Node{
				Section:    SectionLampiran,
				NodeType:   NodeParagrafIsi,
				OrderIndex: b.oiPasal,
				DocOrder:   b.nextDoc(),
				Text:       line,
				StartPage:  raw.Page,
				EndPage:    raw.Page,
				IsAppendix: true,
			})
			b.activeIdx = len(b.nodes) - 1
		} else {
			b.appendText(line)
		}
		boundary = false
	}
	b.flushOrphan()
	return b
}
