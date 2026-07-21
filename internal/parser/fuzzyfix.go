package parser

import "strings"

// fuzzyfix.go: koreksi OCR pada kata kunci ANCHOR STRUKTURAL (Menimbang,
// Mengingat, Memutuskan, Menetapkan, Penjelasan) yang salah tangkap satu-dua
// huruf (mis. OCR membaca "Menimbing" padahal aslinya "Menimbang"). Regex di
// patterns.go/classify.go butuh kecocokan pasti di awal baris — typo satu
// huruf membuat baris itu TIDAK PERNAH dikenali sebagai anchor, padahal
// secara visual/posisi jelas itu section header.
//
// Deteksi di sini DETERMINISTIK (jarak Levenshtein, BUKAN LLM — sama seperti
// alasan penolakan LLM text-fix sebelumnya: butuh sesuatu yang bisa diaudit,
// bukan ditebak). Koreksi HANYA menyentuh kata PERTAMA satu baris yang
// langsung diikuti ':' atau akhir baris (pola pemakaian anchor yang
// sebenarnya) — supaya kata yang sama muncul di TENGAH kalimat biasa (mis.
// "...perlu ditimbing..." — kalau ada) tidak ikut "dikoreksi" secara keliru.

var fuzzyAnchors = []string{
	"MENIMBANG", "MENGINGAT", "MEMUTUSKAN", "MENETAPKAN", "MEMPERHATIKAN", "PENJELASAN",
}

// fuzzyFixAnchorLine memeriksa kata pertama satu baris; bila itu salah-eja
// tipis dari salah satu anchor DAN posisinya cocok pola pemakaian anchor
// (langsung diikuti ':' atau itu saja isi barisnya), kata itu diganti ke
// ejaan baku supaya regex hilir bisa mengenalinya. Baris lain tidak disentuh.
func fuzzyFixAnchorLine(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	leadingSpace := line[:len(line)-len(trimmed)]

	end := 0
	for end < len(trimmed) && isAsciiLetter(trimmed[end]) {
		end++
	}
	if end < 4 { // terlalu pendek untuk anchor manapun, hindari salah-cocok
		return line
	}
	word := trimmed[:end]
	rest := trimmed[end:]

	upper := strings.ToUpper(word)
	if containsStr(fuzzyAnchors, upper) {
		return line // sudah persis benar
	}

	restTrim := strings.TrimLeft(rest, " \t")
	looksLikeAnchorUsage := restTrim == "" || strings.HasPrefix(restTrim, ":")
	if !looksLikeAnchorUsage {
		return line
	}

	best := ""
	bestDist := 1 << 30
	for _, cand := range fuzzyAnchors {
		if d := levenshtein(upper, cand); d < bestDist {
			bestDist, best = d, cand
		}
	}
	threshold := 1
	if len(best) >= 10 {
		threshold = 2
	}
	if bestDist == 0 || bestDist > threshold {
		return line
	}

	return leadingSpace + best + rest
}

func isAsciiLetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// levenshtein menghitung jarak edit klasik (insert/delete/substitute).
// Pemanggil sudah menyeragamkan huruf besar sebelum memanggil ini.
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}
