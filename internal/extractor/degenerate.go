package extractor

import (
	"strings"
)

// degenerate.go menangani keluaran model yang "macet mengulang".
//
// Model visi kecil kerap masuk pengulangan tanpa henti pada halaman yang sebagian
// besar kosong — misalnya halaman berisi dua baris teks lalu menghasilkan
// "1 1 1 1 1 ..." sampai batas token. Batas num_predict memutus panjangnya, tetapi
// teks sampah itu tetap tidak boleh masuk ke korpus: bila dibiarkan, parser akan
// melihat ratusan baris tak dikenal dan hasil akhirnya menyesatkan.
//
// Karena itu keluaran diperiksa; bagian yang jelas berulang dipangkas, dan bila
// isinya didominasi pengulangan, halaman ditandai untuk ditinjau manusia.

const (
	// batas pengulangan berturut-turut yang masih dianggap wajar.
	maxRepeatLines  = 3
	maxRepeatTokens = 4
)

// cleanupResult hasil pemeriksaan keluaran OCR satu halaman.
type cleanupResult struct {
	Text       string // teks setelah pemangkasan
	Degenerate bool   // true bila keluaran didominasi pengulangan
	Removed    int    // jumlah baris yang dipangkas
}

// cleanOCRText memangkas pengulangan berlebihan dan melaporkan apakah keluaran
// tampak degeneratif.
func cleanOCRText(raw string) cleanupResult {
	if strings.TrimSpace(raw) == "" {
		return cleanupResult{Text: raw}
	}

	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	removed := 0

	var prev string
	repeat := 0
	for _, ln := range lines {
		cleaned := collapseRepeatedTokens(ln)
		key := strings.TrimSpace(cleaned)

		if key != "" && key == prev {
			repeat++
			if repeat >= maxRepeatLines {
				removed++
				continue // lewati pengulangan berlebih
			}
		} else {
			repeat = 0
			prev = key
		}
		out = append(out, cleaned)
	}

	text := strings.Join(out, "\n")
	return cleanupResult{
		Text:       text,
		Degenerate: looksDegenerate(raw),
		Removed:    removed,
	}
}

// collapseRepeatedTokens memangkas token identik yang berulang dalam satu baris,
// mis. "1 1 1 1 1 1 1 1" menjadi "1 1 1 1".
func collapseRepeatedTokens(line string) string {
	fields := strings.Fields(line)
	if len(fields) <= maxRepeatTokens {
		return line
	}
	out := make([]string, 0, len(fields))
	var prev string
	run := 0
	for _, f := range fields {
		if f == prev {
			run++
			if run >= maxRepeatTokens {
				continue
			}
		} else {
			prev = f
			run = 0
		}
		out = append(out, f)
	}
	if len(out) == len(fields) {
		return line // tidak ada yang dipangkas: pertahankan spasi asli
	}
	return strings.Join(out, " ")
}

// looksDegenerate menilai apakah keluaran didominasi pengulangan sehingga tidak
// layak dipercaya sebagai hasil OCR.
func looksDegenerate(raw string) bool {
	fields := strings.Fields(raw)
	if len(fields) < 40 {
		return false // terlalu pendek untuk dinilai
	}

	// 1) satu token mendominasi seluruh keluaran.
	freq := map[string]int{}
	for _, f := range fields {
		freq[f]++
	}
	top := 0
	for _, c := range freq {
		if c > top {
			top = c
		}
	}
	if float64(top)/float64(len(fields)) > 0.5 {
		return true
	}

	// 2) keragaman token sangat rendah (banyak kata, sedikit kata unik).
	if float64(len(freq))/float64(len(fields)) < 0.05 {
		return true
	}

	// 3) satu baris identik berulang sangat banyak.
	lines := strings.Split(raw, "\n")
	if len(lines) >= 20 {
		lineFreq := map[string]int{}
		for _, ln := range lines {
			if t := strings.TrimSpace(ln); t != "" {
				lineFreq[t]++
			}
		}
		for _, c := range lineFreq {
			if c > 15 {
				return true
			}
		}
	}
	return false
}
