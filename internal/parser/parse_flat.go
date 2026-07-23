package parser

import "strings"

// parse_flat.go menangani Menimbang, Mengingat & Memperhatikan (opsional):
// daftar poin datar (huruf a.b.c. untuk Menimbang, angka 1.2.3. untuk
// Mengingat/Memperhatikan), tanpa hierarki Bab/Pasal.
// Semua poin menjadi NodeItem (dengan label huruf/angka terisi), agar tidak
// tercampur dengan konteks huruf/angka batang tubuh. TIDAK terpengaruh oleh
// keputusan fold-huruf-ke-Ayat (itu spesifik batang tubuh) — poin di sini
// tetap satu node per poin karena memang bukan bagian struktur Pasal/Ayat.

func parseFlat(section Section, lines []Line) *builder {
	b := newBuilder(section)

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

	for _, raw := range lines {
		line := strings.TrimSpace(raw.Text)
		if line == "" {
			continue
		}
		b.curLinePage = raw.Page

		// Baris header ("Menimbang :", "Mengingat :", "Memperhatikan :"):
		// buang kata kunci, tangkap sisa.
		if reMenimbang.MatchString(line) || reMengingat.MatchString(line) || reMemperhatikan.MatchString(line) {
			if i := strings.Index(line, ":"); i >= 0 {
				rest := strings.TrimSpace(line[i+1:])
				if rest != "" {
					if !handleItem(detectStructural(rest)) {
						b.emitItem("", "", rest)
					}
				}
			}
			continue
		}

		if !handleItem(detectStructural(line)) {
			b.appendText(line) // lanjutan poin sebelumnya
		}
	}
	return b
}

// emitItem menambahkan satu NodeItem datar dengan label huruf/angka opsional.
func (b *builder) emitItem(huruf, angka, text string) {
	b.oiHuruf += orderStep
	n := Node{
		Section:    b.section,
		NodeType:   NodeItem,
		OrderIndex: b.oiHuruf,
		DocOrder:   b.nextDoc(),
		Huruf:      ptr(huruf),
		Angka:      ptr(angka),
		Text:       text,
		StartPage:  b.curLinePage,
		EndPage:    b.curLinePage,
	}
	b.nodes = append(b.nodes, n)
	b.activeIdx = len(b.nodes) - 1
	if len(b.pendingOrphan) > 0 {
		b.attachOrphan(b.activeIdx, "before", b.pendingOrphan)
		b.pendingOrphan = nil
	}
}
