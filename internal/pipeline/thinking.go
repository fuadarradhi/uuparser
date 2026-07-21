package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/fuadarradhi/uuparser/internal/localllm"
	"github.com/fuadarradhi/uuparser/internal/store"
)

// thinking.go membungkus pemakaian model teks.
//
// PRINSIP: model TIDAK PERNAH memerintah aplikasi. Ia hanya mengembalikan
// (a) satu objek JSON berskema tetap untuk klasifikasi, atau (b) teks halaman
// yang sudah diperbaiki. Keputusan apa pun terhadap basis data — menolak,
// menandai duplikat, menyimpan metadata — diambil oleh kode Go di sini
// setelah nilai balikan divalidasi. Teks OCR berasal dari dokumen di
// internet, jadi ia diperlakukan sebagai masukan tak tepercaya, bukan
// perintah.

// classifyReply adalah bentuk jawaban yang diharapkan dari prompts/classify.md.
type classifyReply struct {
	IsPeraturan bool     `json:"is_peraturan"`
	Alasan      string   `json:"alasan"`
	Jenis       string   `json:"jenis"`
	Instansi    string   `json:"instansi"`
	Nomor       string   `json:"nomor"`
	Tahun       string   `json:"tahun"`
	Tentang     string   `json:"tentang"`
	Struktur    string   `json:"struktur"`
	Mencabut    []string `json:"mencabut"`
	Mengubah    []string `json:"mengubah"`
}

var (
	reJSONObject = regexp.MustCompile(`(?s)\{.*\}`)
	reTahun4     = regexp.MustCompile(`^(1[89]|20)[0-9]{2}$`)
	reNomorOK    = regexp.MustCompile(`^[0-9]{1,4}[A-Za-z]?$`)
)

// classifyPage1 meminta model membaca halaman pertama, lalu MEMVALIDASI
// jawabannya sebelum dipakai. Nilai yang tidak masuk akal dibuang (dikosongkan)
// alih-alih dipercaya — model boleh salah, database tidak boleh ikut salah.
func classifyPage1(ctx context.Context, tc *localllm.TextClient, systemPrompt, page1 string) (store.DocMeta, []string, []string, error) {
	res, err := tc.Generate(ctx, systemPrompt, page1)
	if err != nil {
		return store.DocMeta{}, nil, nil, err
	}

	raw := strings.TrimSpace(res.Text)
	// Model kadang membungkus JSON dengan pagar markdown atau kalimat
	// pengantar meski sudah dilarang; ambil objek JSON pertama yang utuh.
	if m := reJSONObject.FindString(raw); m != "" {
		raw = m
	}

	var rep classifyReply
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		return store.DocMeta{}, nil, nil, fmt.Errorf("jawaban klasifikasi bukan JSON yang sah: %w", err)
	}

	meta := store.DocMeta{
		IsPeraturan: rep.IsPeraturan,
		Alasan:      strings.TrimSpace(rep.Alasan),
		Jenis:       cleanField(rep.Jenis, 120),
		Instansi:    cleanField(rep.Instansi, 120),
		Tentang:     cleanField(rep.Tentang, 500),
	}

	// Validasi ketat untuk field yang dipakai sebagai kunci identitas.
	if n := cleanField(rep.Nomor, 8); reNomorOK.MatchString(n) {
		meta.Nomor = n
	}
	if t := cleanField(rep.Tahun, 4); reTahun4.MatchString(t) {
		meta.Tahun = t
	}
	switch strings.ToLower(strings.TrimSpace(rep.Struktur)) {
	case "pasal_ayat", "diktum":
		meta.Struktur = strings.ToLower(strings.TrimSpace(rep.Struktur))
	default:
		meta.Struktur = "unknown"
	}

	return meta, trimList(rep.Mencabut), trimList(rep.Mengubah), nil
}

// canonicalKey menyusun kunci identitas peraturan untuk deteksi duplikat.
// Mengembalikan string kosong bila identitas belum lengkap — lebih baik
// memproses dokumen dua kali daripada membuang dokumen sah karena
// metadatanya tidak terbaca.
func canonicalKey(m store.DocMeta) string {
	j := normKey(m.Jenis)
	i := normKey(m.Instansi)
	if j == "" || i == "" || m.Nomor == "" || m.Tahun == "" {
		return ""
	}
	return strings.Join([]string{j, i, m.Nomor, m.Tahun}, "|")
}

func normKey(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ")
	// Ejaan lama disamakan supaya dokumen yang sama tidak lolos sebagai
	// dua peraturan berbeda hanya karena beda ejaan.
	s = strings.ReplaceAll(s, "PROPINSI", "PROVINSI")
	return s
}

func cleanField(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}

func trimList(in []string) []string {
	var out []string
	for _, v := range in {
		if v = cleanField(v, 300); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// fixPage meminta model memperbaiki salah ketik/struktur satu halaman.
//
// Bila model gagal, mengembalikan teks kosong, atau hasilnya menyusut drastis
// (indikasi model meringkas alih-alih memperbaiki — dilarang oleh prompt),
// hasilnya DIBUANG dan teks mentah yang dipakai. Lebih baik menyisakan salah
// ketik daripada kehilangan isi peraturan.
func fixPage(ctx context.Context, tc *localllm.TextClient, systemPrompt, ocrText string) (fixed string, ok bool) {
	if strings.TrimSpace(ocrText) == "" {
		return "", false
	}
	res, err := tc.Generate(ctx, systemPrompt, ocrText)
	if err != nil {
		return "", false
	}
	out := strings.TrimSpace(res.Text)
	out = stripCodeFence(out)
	if out == "" {
		return "", false
	}
	// Ambang 60%: perbaikan salah ketik hampir tidak mengubah panjang teks;
	// penyusutan besar berarti model meringkas atau memotong.
	if len(out) < len(strings.TrimSpace(ocrText))*6/10 {
		return "", false
	}
	return out, true
}

func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

// countChangedOps menghitung berapa banyak potongan teks yang berbeda antara
// hasil OCR mentah dan hasil perbaikan — dipakai UI untuk menunjukkan
// "model mengubah N bagian di halaman ini". Diff yang sesungguhnya (untuk
// visualisasi) dihitung saat dibutuhkan dari kedua kolom, tidak disimpan.
func countChangedOps(a, b string) int {
	fa, fb := strings.Fields(a), strings.Fields(b)
	// Perbandingan kata-per-kata sederhana lewat LCS panjang; cukup untuk
	// angka indikatif, bukan untuk rendering.
	m, n := len(fa), len(fb)
	if m == 0 || n == 0 {
		return maxInt(m, n)
	}
	prev := make([]int, n+1)
	cur := make([]int, n+1)
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if fa[i-1] == fb[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] >= cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	lcs := prev[n]
	return (m - lcs) + (n - lcs)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
