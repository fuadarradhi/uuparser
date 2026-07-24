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
// DUA PRINSIP yang menentukan bentuk berkas ini:
//
//  1. Model TIDAK PERNAH memerintah aplikasi. Ia hanya mengembalikan satu
//     objek JSON berskema tetap; keputusan terhadap basis data diambil kode Go
//     setelah nilainya divalidasi. Teks OCR berasal dari dokumen di internet,
//     jadi diperlakukan sebagai masukan tak tepercaya.
//
//  2. Model hanya MEMBACA; kode yang MENYIMPULKAN. Pemetaan jabatan ke badan
//     pemerintahan dan penurunan angka urut dari nomor peraturan bersifat
//     pasti, jadi dikerjakan di normalize.go — bukan diserahkan ke model kecil
//     yang bisa keliru dan sulit diaudit.
//
// Pertanyaan dipecah menjadi beberapa panggilan kecil (gerbang, identitas,
// penetapan). Satu prompt panjang yang meminta banyak hal sekaligus membuat
// model kecil kehilangan sebagian instruksi — gejalanya antara lain menyalin
// contoh di dalam prompt sebagai jawaban.

var (
	reJSONObject = regexp.MustCompile(`(?s)\{.*\}`)
	reTahun4     = regexp.MustCompile(`^(1[89]|20)[0-9]{2}$`)

	// rePlaceholder menangkap jawaban yang sebenarnya potongan prompt yang
	// tersalin, mis. "KEPUTUSAN ..." atau "<JENIS>". Nilai seperti ini WAJIB
	// dibuang: menyimpannya berarti mencatat isi prompt sebagai data.
	rePlaceholder = regexp.MustCompile(`\.\.\.|<[^>]*>`)
)

// askJSON menjalankan satu pertanyaan dan mengurai jawabannya sebagai JSON.
// raw (teks MENTAH sebelum diurai) dikembalikan terpisah — dipakai pemanggil
// untuk mode debug (lihat DEBUG_RESULT), supaya jawaban model yang
// sebenarnya (termasuk yang gagal diurai) tetap bisa ditinjau.
func askJSON(ctx context.Context, tc *localllm.TextClient, prompt, text string,
	p localllm.TextParams, out any) (raw string, err error) {
	res, err := tc.GenerateWith(ctx, prompt, text, p)
	if err != nil {
		return "", err
	}
	raw = strings.TrimSpace(res.Text)
	// Model kadang membungkus JSON dengan pagar markdown atau kalimat
	// pengantar meski sudah dilarang; ambil objek JSON pertama yang utuh.
	parseTarget := raw
	if m := reJSONObject.FindString(parseTarget); m != "" {
		parseTarget = m
	}
	if err := json.Unmarshal([]byte(parseTarget), out); err != nil {
		return raw, fmt.Errorf("jawaban bukan JSON yang sah: %w", err)
	}
	return raw, nil
}

// ---- Tahap 1: gerbang "ini produk hukum atau bukan" ----

type gateReply struct {
	ProdukHukum bool   `json:"produk_hukum"`
	Alasan      string `json:"alasan"`
}

// AskIsRegulation menanyakan SATU hal saja. Pertanyaan sempit jauh lebih
// jarang meleset pada model kecil daripada pertanyaan gabungan. raw adalah
// jawaban mentah model — lihat askJSON.
func AskIsRegulation(ctx context.Context, tc *localllm.TextClient, prompt, page string,
	p localllm.TextParams) (ok bool, alasan, raw string, err error) {
	var r gateReply
	raw, err = askJSON(ctx, tc, prompt, page, p, &r)
	if err != nil {
		return false, "", raw, err
	}
	return r.ProdukHukum, bersih(r.Alasan, 300), raw, nil
}

// ---- Tahap 2: identitas peraturan ----

type identityReply struct {
	Jenis            string `json:"jenis"`
	InstansiTertulis string `json:"instansi_tertulis"`
	Nomor            string `json:"nomor"`
	Tahun            string `json:"tahun"`
	Tentang          string `json:"tentang"`
}

// AskIdentity membaca identitas peraturan lalu MEMVALIDASI dan MENORMALKAN
// hasilnya. Model boleh salah; basis data tidak boleh ikut salah. raw adalah
// jawaban mentah model — lihat askJSON.
func AskIdentity(ctx context.Context, tc *localllm.TextClient, prompt, page string,
	p localllm.TextParams) (meta store.DocMeta, raw string, err error) {
	var r identityReply
	raw, err = askJSON(ctx, tc, prompt, page, p, &r)
	if err != nil {
		return store.DocMeta{}, raw, err
	}

	m := store.DocMeta{
		Jenis:            tolakPlaceholder(bersih(r.Jenis, 120)),
		InstansiTertulis: tolakPlaceholder(bersih(r.InstansiTertulis, 120)),
		Nomor:            tolakPlaceholder(bersih(r.Nomor, 60)),
		Tentang:          tolakPlaceholder(bersih(r.Tentang, 500)),
	}
	// Wilayah baku diturunkan dari yang tertulis — secara deterministik.
	m.Wilayah = NormalizeWilayah(m.InstansiTertulis)
	// Angka urut diturunkan dari nomor asli; nomor aslinya tetap utuh.
	m.NomorUrut = NomorUrut(m.Nomor)

	if t := bersih(r.Tahun, 4); reTahun4.MatchString(t) {
		m.Tahun = t
	}
	return m, raw, nil
}

// ---- Tahap 3: penetapan & pengundangan ----

type penetapanReply struct {
	DitetapkanDi        string `json:"ditetapkan_di"`
	DitetapkanTanggal   string `json:"ditetapkan_tanggal"`
	DitetapkanOleh      string `json:"ditetapkan_oleh"`
	DitetapkanOlehNama  string `json:"ditetapkan_oleh_nama"`
	DiundangkanDi       string `json:"diundangkan_di"`
	DiundangkanTanggal  string `json:"diundangkan_tanggal"`
	DiundangkanOleh     string `json:"diundangkan_oleh"`
	DiundangkanOlehNama string `json:"diundangkan_oleh_nama"`
}

// AskPenetapan membaca bagian penutup dokumen. Dipanggil HANYA bila parser
// menemukan penandanya tetapi tidak dapat menguraikannya sendiri — lihat
// pipeline/trigger.go. raw adalah jawaban mentah model — lihat askJSON.
func AskPenetapan(ctx context.Context, tc *localllm.TextClient, prompt, text string,
	p localllm.TextParams) (hasil store.Penetapan, raw string, err error) {
	var r penetapanReply
	raw, err = askJSON(ctx, tc, prompt, text, p, &r)
	if err != nil {
		return store.Penetapan{}, raw, err
	}
	return store.Penetapan{
		DitetapkanDi:        tolakPlaceholder(bersih(r.DitetapkanDi, 120)),
		DitetapkanTanggal:   tolakPlaceholder(bersih(r.DitetapkanTanggal, 60)),
		DitetapkanOleh:      tolakPlaceholder(bersih(r.DitetapkanOleh, 120)),
		DitetapkanOlehNama:  tolakPlaceholder(bersih(r.DitetapkanOlehNama, 120)),
		DiundangkanDi:       tolakPlaceholder(bersih(r.DiundangkanDi, 120)),
		DiundangkanTanggal:  tolakPlaceholder(bersih(r.DiundangkanTanggal, 60)),
		DiundangkanOleh:     tolakPlaceholder(bersih(r.DiundangkanOleh, 120)),
		DiundangkanOlehNama: tolakPlaceholder(bersih(r.DiundangkanOlehNama, 120)),
	}, raw, nil
}

// ---- Tinjauan: dipanggil HANYA saat parser mencurigai hasilnya sendiri ----

type tinjauReply struct {
	Bermasalah bool   `json:"bermasalah"`
	Penjelasan string `json:"penjelasan"`
}

// AskTinjauan (2026-07-23) adalah mekanisme berbeda dari AskIsRegulation/
// AskIdentity/AskPenetapan di atas: ketiganya dipanggil untuk MENGISI data
// yang belum ada (identitas, penetapan). AskTinjauan dipanggil untuk MENILAI
// ULANG data yang SUDAH ADA tapi parser sendiri mencurigainya — sinyalnya
// parser.AnchorLeakNodes (lihat diagnose.go), dipicu ketika sebuah node
// hasil parse memuat kata kunci penanda section lain (LAMPIRAN/MEMUTUSKAN/
// dst) DI TENGAH teksnya, yang biasanya berarti dua bagian dokumen tercampur
// karena batasnya gagal terdeteksi — persis kelas bug yang ditemukan
// 2026-07-23 (Memperhatikan tersedot ke Mengingat, Lampiran tersedot ke
// penutup) TAPI TIDAK memicu warning struktural apa pun sebelumnya.
//
// PRINSIP TETAP SAMA — malah lebih ketat: jawabannya TIDAK PERNAH mengubah
// node/nilai apa pun di database. Ia murni CATATAN TAMBAHAN yang ditempel
// ke extraction_notes dokumen untuk dibaca manusia (lihat parser_worker.go)
// — beda dari AskIdentity/AskPenetapan yang jawabannya (sesudah divalidasi)
// memang disimpan sebagai data. Di sini bahkan KODE PUN tidak menyimpulkan
// apa-apa dari jawabannya, apalagi menimpa hasil parse — hanya meneruskan
// apa kata model, apa adanya, sebagai bahan tinjauan manusia.
//
// [2026-07-24] Fungsi yang SAMA ini dipakai untuk DUA pemicu berbeda —
// hanya prompt & teks yang dikirim yang beda, bentuk jawaban
// {bermasalah, penjelasan} identik di keduanya: (1) tinjau.md untuk
// parser.AnchorLeakNodes di atas, (2) orphan.md untuk
// parser.OrphanWarningNodes (teks yatim yang gagal dicocokkan ke struktur
// apa pun). Sempat ada pemicu ketiga (document_review.md — ringkasan
// jumlah Bab/Pasal/dst per dokumen) tapi DIHAPUS lagi (2026-07-24,
// permintaan user): itu murni perbandingan ANGKA, tidak butuh model sama
// sekali — mubazir. Kalau nanti ada kebutuhan sanity-check angka
// semacam itu, itu jadi Issue baru di parser.Diagnose (Go biasa), bukan
// panggilan model.
func AskTinjauan(ctx context.Context, tc *localllm.TextClient, prompt, teksNode string,
	p localllm.TextParams) (bermasalah bool, penjelasan, raw string, err error) {
	var r tinjauReply
	raw, err = askJSON(ctx, tc, prompt, teksNode, p, &r)
	if err != nil {
		return false, "", raw, err
	}
	return r.Bermasalah, bersih(r.Penjelasan, 500), raw, nil
}

// ---- kunci identitas ----

// canonicalKey menyusun kunci identitas peraturan untuk deteksi duplikat.
// Memakai nomor ASLI, bukan angka urutnya: dua keputusan berbeda dapat
// berbagi angka pertama yang sama ("300.2/ 69 /2026" dan "300.2/ 70 /2026").
//
// Mengembalikan string kosong bila identitas belum lengkap — lebih baik
// memproses dokumen dua kali daripada membuang dokumen sah karena
// metadatanya tidak terbaca.
func canonicalKey(m store.DocMeta) string {
	j := rapikan(m.Jenis)
	w := rapikan(m.Wilayah)
	n := rapikan(m.Nomor)
	if j == "" || w == "" || n == "" || m.Tahun == "" {
		return ""
	}
	return strings.Join([]string{j, w, n, m.Tahun}, "|")
}

// ---- pembersih nilai ----

func bersih(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// tolakPlaceholder membuang jawaban yang sebenarnya potongan prompt yang
// tersalin. Model kecil kerap melakukannya, dan hasilnya menyesatkan karena
// tampak seperti data yang sah ("KEPUTUSAN ...").
func tolakPlaceholder(s string) string {
	if s == "" {
		return ""
	}
	if rePlaceholder.MatchString(s) {
		return ""
	}
	return s
}
