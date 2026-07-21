package parser

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// diagnose.go menyediakan pemeriksaan kesehatan (lint) atas hasil Parse:
// apakah struktur wajar untuk sebuah peraturan, dan apa yang perlu ditinjau.
// Dipakai oleh test runner, tapi juga bisa dipanggil caller mana pun.

type Status string

const (
	StatusSuccess Status = "SUCCESS" // tak ada masalah berarti
	StatusWarning Status = "WARNING" // ada hal yang perlu ditinjau, tapi terparse
	StatusFail    Status = "FAIL"    // tidak layak/parse gagal
)

// Issue satu temuan diagnosa.
type Issue struct {
	Severity Severity `json:"severity"` // info | needs_review
	Code     string   `json:"code"`     // kode ringkas, mis "PASAL_GAP"
	Message  string   `json:"message"`
}

// Stats ringkasan jumlah node per jenis.
type Stats struct {
	Bab           int `json:"bab"`
	Bagian        int `json:"bagian"`
	Paragraf      int `json:"paragraf"`
	Pasal         int `json:"pasal"`
	Ayat          int `json:"ayat"`
	ItemPreamble  int `json:"item_preamble"`
	NodeWarnings  int `json:"node_warnings"`
	TotalNodes    int `json:"total_nodes"`
	SectionsFound int `json:"sections_found"`
}

// Report hasil diagnosa satu dokumen.
type Report struct {
	Status Status  `json:"status"`
	Issues []Issue `json:"issues"`
	Stats  Stats   `json:"stats"`
}

// DiagnoseParse menjalankan Parse lalu Diagnose sekaligus. err hanya untuk
// kegagalan fatal (mis. bukan dokumen hukum / input kosong).
func DiagnoseParse(pages []string) (Report, Result, error) {
	res, err := Parse(pages)
	if err != nil {
		return Report{
			Status: StatusFail,
			Issues: []Issue{{SeverityNeedsReview, "PARSE_ERROR", err.Error()}},
		}, res, err
	}
	return Diagnose(res), res, nil
}

// Diagnose memeriksa Result dan mengembalikan Report.
func Diagnose(res Result) Report {
	var issues []Issue
	st := computeStats(res)

	// warning level dokumen dari parser diteruskan sebagai issue.
	for _, w := range res.DocumentWarnings {
		issues = append(issues, Issue{w.Severity, "DOC_WARNING", w.Message})
	}

	// kumpulkan node warning (orphan dll) jadi ringkasan.
	if st.NodeWarnings > 0 {
		issues = append(issues, Issue{
			SeverityNeedsReview, "NODE_WARNINGS",
			fmt.Sprintf("%d node memiliki warning (mis. teks tak terstruktur/orphan)", st.NodeWarnings),
		})
	}

	// --- cek keberadaan batang tubuh ---
	if st.Pasal == 0 {
		issues = append(issues, Issue{SeverityNeedsReview, "NO_PASAL",
			"Tidak ada satupun Pasal terdeteksi di batang tubuh"})
	}

	// --- cek celah penomoran Pasal (batang tubuh) ---
	issues = append(issues, checkPasalSequence(res)...)

	// --- cek celah ayat per pasal ---
	// (HURUF_GAP dihapus 2026-07-20: huruf tak lagi jadi node terpisah, lihat
	// builder.foldHuruf — urutan huruf tetap ada sebagai teks di dalam Ayat,
	// tapi tak lagi dicek otomatis di level ini.)
	issues = append(issues, checkAyatSequence(res)...)

	// --- cek pasal Penjelasan yang tak ada di batang tubuh ---
	issues = append(issues, checkPenjelasanRefs(res)...)

	// --- cek pasal "kosong" (tak punya teks, ayat, maupun huruf) ---
	issues = append(issues, checkEmptyPasal(res)...)

	// tentukan status akhir.
	status := StatusSuccess
	for _, is := range issues {
		if is.Severity == SeverityNeedsReview {
			status = StatusWarning
			break
		}
	}
	if st.Pasal == 0 {
		status = StatusFail
	}

	return Report{Status: status, Issues: issues, Stats: st}
}

func computeStats(res Result) Stats {
	var s Stats
	sections := map[Section]bool{}
	s.TotalNodes = len(res.Nodes)
	for _, n := range res.Nodes {
		sections[n.Section] = true
		if len(n.Warnings) > 0 {
			s.NodeWarnings += len(n.Warnings)
		}
		switch n.NodeType {
		case NodeBab:
			s.Bab++
		case NodeBagian:
			s.Bagian++
		case NodeParagraf:
			s.Paragraf++
		case NodePasal:
			if n.Section == SectionBatangTubuh {
				s.Pasal++
			}
		case NodeAyat:
			if n.Section == SectionBatangTubuh {
				s.Ayat++
			}
		case NodeItem:
			s.ItemPreamble++
		}
	}
	s.SectionsFound = len(sections)
	return s
}

var reNumPart = regexp.MustCompile(`^(\d+)`)

// numPrefix mengambil bagian angka dari label (mis "27A" -> 27, "1a" -> 1).
func numPrefix(label string) (int, bool) {
	m := reNumPart.FindStringSubmatch(label)
	if m == nil {
		return 0, false
	}
	v, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return v, true
}

// checkPasalSequence mendeteksi celah pada penomoran pasal batang tubuh.
func checkPasalSequence(res Result) []Issue {
	var seq []int
	seen := map[int]bool{}
	for _, n := range res.Nodes {
		if n.Section == SectionBatangTubuh && n.NodeType == NodePasal && n.Pasal != nil {
			if v, ok := numPrefix(*n.Pasal); ok && !seen[v] {
				seen[v] = true
				seq = append(seq, v)
			}
		}
	}
	return gapIssues(seq, "PASAL_GAP", "Pasal")
}

// checkAyatSequence: per pasal, cek celah nomor ayat.
func checkAyatSequence(res Result) []Issue {
	byPasal := map[string][]int{}
	seen := map[string]bool{}
	var order []string
	for _, n := range res.Nodes {
		if n.Section == SectionBatangTubuh && n.NodeType == NodeAyat && n.Pasal != nil && n.Ayat != nil {
			key := *n.Pasal
			if v, ok := numPrefix(*n.Ayat); ok {
				sk := key + "|" + strconv.Itoa(v)
				if seen[sk] {
					continue
				}
				seen[sk] = true
				if _, exists := byPasal[key]; !exists {
					order = append(order, key)
				}
				byPasal[key] = append(byPasal[key], v)
			}
		}
	}
	var issues []Issue
	for _, key := range order {
		for _, is := range gapIssues(byPasal[key], "AYAT_GAP", "Ayat") {
			is.Message = fmt.Sprintf("Pasal %s: %s", key, is.Message)
			issues = append(issues, is)
		}
	}
	return issues
}

// checkPenjelasanRefs: pasal di penjelasan yang tak ada padanan di batang tubuh.
func checkPenjelasanRefs(res Result) []Issue {
	bt := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Section == SectionBatangTubuh && n.NodeType == NodePasal && n.Pasal != nil {
			bt[*n.Pasal] = true
		}
	}
	if len(bt) == 0 {
		return nil
	}
	var issues []Issue
	seen := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Section == SectionPenjelasanPasal && n.NodeType == NodePasal && n.Pasal != nil {
			if !bt[*n.Pasal] && !seen[*n.Pasal] {
				seen[*n.Pasal] = true
				issues = append(issues, Issue{SeverityNeedsReview, "PENJELASAN_ORPHAN",
					fmt.Sprintf("Penjelasan menyebut Pasal %s yang tidak ada di batang tubuh", *n.Pasal)})
			}
		}
	}
	return issues
}

// checkEmptyPasal: pasal batang tubuh tanpa teks, tanpa ayat, tanpa huruf anak.
func checkEmptyPasal(res Result) []Issue {
	// petakan pasal -> apakah punya anak (ayat/huruf) atau teks.
	type info struct {
		hasText, hasChild bool
	}
	m := map[string]*info{}
	var order []string
	for _, n := range res.Nodes {
		if n.Section != SectionBatangTubuh {
			continue
		}
		if n.NodeType == NodePasal && n.Pasal != nil {
			if _, ok := m[*n.Pasal]; !ok {
				m[*n.Pasal] = &info{}
				order = append(order, *n.Pasal)
			}
			if strings.TrimSpace(n.Text) != "" {
				m[*n.Pasal].hasText = true
			}
		}
		if n.NodeType == NodeAyat && n.Pasal != nil {
			if inf, ok := m[*n.Pasal]; ok {
				inf.hasChild = true
			}
		}
	}
	var issues []Issue
	for _, k := range order {
		inf := m[k]
		if !inf.hasText && !inf.hasChild {
			issues = append(issues, Issue{SeverityNeedsReview, "EMPTY_PASAL",
				fmt.Sprintf("Pasal %s tidak memiliki teks, ayat, maupun huruf (kemungkinan gagal parse)", k)})
		}
	}
	return issues
}

// ---- util deteksi celah ----

// gapIssues mendeteksi celah pada urutan angka menaik.
func gapIssues(seq []int, code, label string) []Issue {
	if len(seq) < 2 {
		return nil
	}
	// urutkan salinan untuk deteksi celah (urutan asli bisa saja benar).
	s := append([]int(nil), seq...)
	sort.Ints(s)
	var missing []int
	for i := 1; i < len(s); i++ {
		for v := s[i-1] + 1; v < s[i]; v++ {
			missing = append(missing, v)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []Issue{{SeverityNeedsReview, code,
		fmt.Sprintf("%s tidak berurutan, kemungkinan hilang: %s", label, intsToStr(missing))}}
}

func intsToStr(v []int) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ", ")
}

