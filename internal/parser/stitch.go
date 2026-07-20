package parser

import (
	"regexp"
	"strings"
)

// stitch.go menggabung array halaman OCR menjadi satu daftar baris bersih:
//   - buang baris nomor halaman & garis pemisah,
//   - buang header/footer yang berulang identik di banyak halaman,
//   - jalankan fixOCRLine pada tiap baris.
//
// Output: []string baris-baris ter-normalisasi, siap disegmentasi.

var (
	rePageNum   = regexp.MustCompile(`^[-–—\s]*\d{1,4}[-–—\s]*$`)           // "- 5 -", "12"
	rePageLabel = regexp.MustCompile(`(?i)^(halaman|hlm|page)\s*\.?\s*\d+`) // "Halaman 5"
	reRuleLine  = regexp.MustCompile(`^[-_=.·•\s]{3,}$`)                    // garis pemisah
)

func stitch(pages []string) []string {
	// 1) pecah tiap halaman jadi baris & normalisasi awal.
	perPage := make([][]string, 0, len(pages))
	for _, p := range pages {
		raw := strings.Split(p, "\n")
		lines := make([]string, 0, len(raw))
		for _, ln := range raw {
			f := fixOCRLine(ln)
			lines = append(lines, f) // simpan termasuk kosong dulu, untuk deteksi header/footer posisi
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
				t := strings.TrimSpace(ln)
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
				if boiler[strings.TrimSpace(ln)] {
					continue
				}
				kept = append(kept, ln)
			}
			perPage[pi] = kept
		}
	}

	// 3) gabung semua halaman, buang nomor halaman/garis, rapikan blank berlebih.
	out := make([]string, 0, 1024)
	prevBlank := false
	for _, lines := range perPage {
		for _, ln := range lines {
			t := strings.TrimSpace(ln)
			if t == "" {
				if !prevBlank {
					out = append(out, "")
					prevBlank = true
				}
				continue
			}
			if rePageNum.MatchString(t) || rePageLabel.MatchString(t) || reRuleLine.MatchString(t) {
				continue
			}
			out = append(out, ln)
			prevBlank = false
		}
	}
	// buang blank di ujung.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
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
