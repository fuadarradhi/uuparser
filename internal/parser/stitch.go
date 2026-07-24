package parser

import (
	"regexp"
	"strings"
)

// stitch.go menggabung array halaman OCR menjadi satu daftar baris bersih,
// TIAP BARIS MEMBAWA NOMOR HALAMAN ASALNYA (Line.Page):
//   - buang baris nomor halaman & garis pemisah,
//   - buang header/footer yang berulang identik di banyak halaman,
//   - jalankan fixOCRLine pada tiap baris.
//
// Output: []Line ter-normalisasi, siap disegmentasi.

var (
	rePageNum   = regexp.MustCompile(`^[-–—\s]*\d{1,4}[-–—\s]*$`)           // "- 5 -", "12"
	rePageLabel = regexp.MustCompile(`(?i)^(halaman|hlm|page)\s*\.?\s*\d+`) // "Halaman 5"
	reRuleLine  = regexp.MustCompile(`^[-_=.·•\s]{3,}$`)                    // garis pemisah
	// reWatermark menangkap baris yang HANYA berisi watermark/URL situs JDIH
	// (mis. "www.jdih.acehprov.go.id") — pola furniture halaman yang sama
	// sekali bukan bagian isi peraturan, sama seperti nomor halaman.
	reWatermark = regexp.MustCompile(`(?i)^\s*(https?://|www\.)?[a-z0-9.-]*\.(go\.id|ac\.id|co\.id|or\.id)\S*\s*$`)
	// reCatchword (2026-07-23) menangkap "kata alih" (catchword) khas
	// dokumen resmi lama: baris pendek di bawah halaman berisi pratinjau
	// kata/frasa pembuka halaman berikutnya, diakhiri "…/N" (N = nomor
	// halaman berikutnya) — mis. "Wilayah.../2", "b. Persiapan .../3",
	// "Memperhatikan : .../2". Isinya SELALU terulang penuh di awal halaman
	// berikutnya, jadi baris ini murni artefak tata letak, bukan isi.
	// Ditemukan lewat bug nyata: dibiarkan lolos, ia nyambung jadi teks
	// (mis. "Memperhatikan : .../2 Memperhatikan : Surat Edaran ..." malah
	// mencemari item Mengingat sebelumnya). BUKAN sekadar prefix seperti
	// dedupPageBoundaries menangani (kata ulangnya sering cuma sebagian
	// dari isi sesungguhnya), jadi perlu penanganan terpisah di sini.
	// !looksStructural dipakai sebagai pengaman tambahan (lihat pemakaian
	// di bawah) — baris struktural asli (Pasal/BAB/dst) tidak pernah
	// berbentuk begini, tapi dijaga saja.
	reCatchword = regexp.MustCompile(`(?i)^.{0,40}\.\.\.\s*/\s*\d{1,4}\s*$`)
)

func stitch(pages []string) []Line {
	// 1) pecah tiap halaman jadi baris & normalisasi awal, tandai nomor halaman (1-indexed).
	perPage := make([][]Line, 0, len(pages))
	for pi, p := range pages {
		raw := strings.Split(p, "\n")
		lines := make([]Line, 0, len(raw))
		for _, ln := range raw {
			f := fixOCRLine(ln)
			f = fuzzyFixAnchorLine(f)
			lines = append(lines, Line{Text: f, Page: pi + 1}) // simpan termasuk kosong dulu, untuk deteksi header/footer posisi
		}
		perPage = append(perPage, lines)
	}

	// 2) deteksi header/footer berulang: baris identik yang muncul di >=40% halaman
	//    (dan bukan baris struktural) dianggap boilerplate.
	if len(perPage) >= 3 {
		freq := map[string]int{}
		for _, lines := range perPage {
			seen := map[string]bool{}
			for _, ln := range lines {
				t := strings.TrimSpace(ln.Text)
				if t == "" || seen[t] {
					continue
				}
				seen[t] = true
				freq[t]++
			}
		}
		threshold := (len(perPage)*4 + 9) / 10 // ~40%
		boiler := map[string]bool{}
		for t, c := range freq {
			if c >= threshold && !looksStructural(t) {
				boiler[t] = true
			}
		}
		for pi := range perPage {
			kept := perPage[pi][:0]
			for _, ln := range perPage[pi] {
				if boiler[strings.TrimSpace(ln.Text)] {
					continue
				}
				kept = append(kept, ln)
			}
			perPage[pi] = kept
		}
	}

	// 3) gabung semua halaman, buang nomor halaman/garis, rapikan blank berlebih.
	out := make([]Line, 0, 1024)
	prevBlank := false
	for _, lines := range perPage {
		for _, ln := range lines {
			t := strings.TrimSpace(ln.Text)
			if t == "" {
				if !prevBlank {
					out = append(out, Line{Text: "", Page: ln.Page})
					prevBlank = true
				}
				continue
			}
			if rePageNum.MatchString(t) || rePageLabel.MatchString(t) || reRuleLine.MatchString(t) || reWatermark.MatchString(t) ||
				(reCatchword.MatchString(t) && !looksStructural(t)) {
				continue
			}
			out = append(out, ln)
			prevBlank = false
		}
	}
	// buang blank di ujung.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1].Text) == "" {
		out = out[:len(out)-1]
	}
	return dedupStructuralPageBoundary(dedupPageBoundaries(out))
}

// dedupStructuralPageBoundary menangani gejala BERBEDA dari
// dedupPageBoundaries/reCatchword di atas — [Ditemukan nyata 2026-07-24,
// debug/88]: halaman lama kadang mencetak "kata alih" (catchword) berupa
// PENANDA STRUKTURAL PENUH (bukan sekadar potongan kalimat biasa) di baris
// terakhir sebelum ganti halaman, mis. "KEDUA : .../2" — yang kemudian
// diulang UTUH di awal halaman berikutnya ("KEDUA : Rencana Alokasi Air
// Tahunan ..."). Baik reCatchword maupun dedupPageBoundaries SENGAJA
// memakai !looksStructural sebagai pengaman (supaya tidak salah membuang
// Pasal/Diktum ASLI yang kebetulan berulang) — pengaman itu justru
// membuat kasus INI (catchword yang KEBETULAN berbentuk struktural) lolos
// utuh: openDiktum/openPasal (lihat parse_batangtubuh.go) terpicu dua kali
// dengan label SAMA — yang PERTAMA cuma berisi sisa pratinjau (".../2")
// sebagai teks node, sementara isi ASLI diktum itu nyasar jadi orphan
// tanpa node induk di halaman berikutnya.
//
// Fix: bandingkan baris TAK-KOSONG TERAKHIR sebelum tiap transisi halaman
// dengan baris TAK-KOSONG PERTAMA di halaman berikutnya (baris kosong
// pemisah paragraf di antaranya dilewati) — bila kind+label struktural
// keduanya SAMA PERSIS, baris pertama (versi terpotong/pratinjau) dibuang;
// baris kedua (versi lengkap) yang dipakai. Aman: kind+label struktural
// yang identik pada TEPAT batas halaman hampir mustahil kebetulan untuk
// dua Pasal/Diktum SUNGGUHAN yang berbeda — dokumen asli tidak pernah
// menulis "Pasal 5" dua kali berturut-turut untuk pasal yang sama.
func dedupStructuralPageBoundary(lines []Line) []Line {
	drop := make(map[int]bool)
	lastNonBlank := -1
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i].Text) == "" {
			continue
		}
		if lastNonBlank >= 0 && lines[lastNonBlank].Page != lines[i].Page {
			a := detectStructural(strings.TrimSpace(lines[lastNonBlank].Text))
			b := detectStructural(strings.TrimSpace(lines[i].Text))
			if a.kind != mkNone && a.kind == b.kind && a.label != "" && a.label == b.label {
				drop[lastNonBlank] = true
			}
		}
		lastNonBlank = i
	}
	if len(drop) == 0 {
		return lines
	}
	out := make([]Line, 0, len(lines)-len(drop))
	for i, ln := range lines {
		if drop[i] {
			continue
		}
		out = append(out, ln)
	}
	return out
}

// dedupPageBoundaries menangani satu gejala scan lama: baris terakhir suatu
// halaman terpotong ("...pada dasarnya") lalu halaman berikutnya mengulang
// balik dari situ dengan versi yang lebih lengkap ("pada dasarnya kita
// memahami..."). Ini SENGAJA hanya menangani kasus AMAN — baris pemotong
// adalah PREFIKS PERSIS (case-insensitive, setelah TrimSpace) dari baris
// pembuka halaman berikutnya — dan BUKAN baris struktural (BAB/Pasal/dst).
// Overlap yang tidak persis (mis. parafrase, typo beda) SENGAJA TIDAK
// disentuh — menebak di sini berisiko menghapus isi asli yang sebetulnya
// bukan duplikat, dan proyek ini sudah punya sikap tegas soal itu (lihat
// alasan penolakan LLM text-fix).
func dedupPageBoundaries(lines []Line) []Line {
	if len(lines) < 2 {
		return lines
	}
	const minOverlapLen = 6 // hindari baris pendek kebetulan jadi prefiks (mis. "BAB I")
	out := make([]Line, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if i+1 < len(lines) && lines[i].Page != lines[i+1].Page {
			a := strings.TrimSpace(lines[i].Text)
			b := strings.TrimSpace(lines[i+1].Text)
			if len(a) >= minOverlapLen && !looksStructural(a) &&
				strings.HasPrefix(strings.ToLower(b), strings.ToLower(a)) {
				continue // lewati baris[i]: duplikat terpotong, versi lengkap ada di baris[i+1]
			}
		}
		out = append(out, lines[i])
	}
	return out
}

// looksStructural mencegah baris struktural (BAB, Pasal, dst) terhapus sebagai boilerplate
// meskipun kebetulan berulang.
func looksStructural(t string) bool {
	m := detectStructural(t)
	if m.kind != mkNone {
		return true
	}
	if reMenimbang.MatchString(t) || reMengingat.MatchString(t) ||
		reMemutuskan.MatchString(t) || rePenjelasan.MatchString(t) {
		return true
	}
	return false
}
