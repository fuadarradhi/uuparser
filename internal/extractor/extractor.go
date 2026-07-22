// Package extractor mengubah PDF menjadi teks per halaman. Tiap halaman
// dirasterisasi, di-OCR, lalu diserahkan ke PageSink milik pemanggil —
// TIDAK ADA berkas perantara di disk (satu-satunya berkas yang hidup di disk
// adalah PDF sumbernya, yang tetap berguna untuk pratinjau di UI).
//
// Package ini sengaja tidak tahu apa pun soal Postgres, model teks, maupun
// aturan bisnis "peraturan atau bukan". Ia hanya menjalankan urutan:
//
//	untuk tiap halaman: render -> OCR -> serahkan ke PageSink
//
// dan berhenti bila PageSink menyuruh berhenti. Keputusan (klasifikasi
// halaman 1, perbaikan teks, penyimpanan) seluruhnya milik pemanggil —
// lihat internal/pipeline.
package extractor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fuadarradhi/uuparser/internal/localllm"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/raster"
)

// PageResult adalah hasil OCR satu halaman beserta ukuran teknisnya.
type PageResult struct {
	Page        int
	Total       int // jumlah halaman dokumen — dipakai pemanggil untuk baris kemajuan
	W, H        int // dimensi gambar yang benar-benar dikirim ke model
	Text        string
	IsEmpty     bool
	IsTruncated bool
	InkRatio    float64
	CroppedPct  float64
	// DPI adalah resolusi render yang SUNGGUH dipakai untuk halaman ini —
	// diputuskan sekali di halaman 1 (lihat renderAdaptif), diwariskan ke
	// halaman-halaman berikutnya.
	DPI int
	// BlurScoreProbe hanya terisi (>0) pada halaman yang MEMUTUSKAN DPI-nya
	// sendiri (biasanya halaman 1) — nol pada halaman yang mewarisi
	// keputusan halaman itu, supaya pemanggil tahu skor mana yang benar-benar
	// dipakai mengambil keputusan (bukan skor yang tidak pernah dihitung).
	BlurScoreProbe float64
	// PNG adalah gambar PERSIS yang dikirim ke model OCR — hanya diisi bila
	// pemanggil memerlukannya (mis. mode debug: lihat pipeline.DebugResult).
	// Dikosongkan kembali (nil) sesegera mungkin oleh pemanggil yang tak
	// membutuhkannya, supaya tidak menahan memori PNG banyak halaman
	// sekaligus.
	PNG        []byte
	DurationMS int
	Notes      []string
}

// PageSink menerima hasil tiap halaman, satu per satu, berurutan.
//
// Nilai balik `stop` memungkinkan pemanggil menghentikan dokumen di tengah
// jalan — dipakai untuk berhenti setelah halaman 1 bila dokumen ternyata
// bukan peraturan atau duplikat, sehingga sisa halaman tidak di-OCR
// percuma. `HasPage` memungkinkan melanjutkan dokumen yang terhenti
// (resume per-halaman) tanpa mengulang halaman yang sudah tersimpan.
type PageSink interface {
	HasPage(ctx context.Context, page int) (bool, error)
	OnPage(ctx context.Context, r PageResult) (stop bool, err error)

	// OnProgress dipanggil BERKALI-KALI selama satu halaman dikerjakan,
	// termasuk SEBELUM OCR halaman itu dimulai. Tanpa ini konsol diam
	// beberapa menit pada halaman pertama sehingga proses tampak macet:
	// penyandian gambar adalah bagian paling lambat dan terjadi sebelum
	// token pertama keluar.
	OnProgress(page, total int, detail string)
}

// Config: hanya hal yang memang perlu ditentukan pemanggil.
type Config struct {
	// Render adaptif — TAPI HANYA DIPUTUSKAN SEKALI PER DOKUMEN (di halaman
	// pertama yang benar-benar diproses, lihat Document/renderAdaptif),
	// bukan per halaman: DPIJelas dipakai sebagai probe; kalau blurScore-nya
	// sudah cukup tajam (>= AmbangJelas), dipakai langsung tanpa render
	// ulang. Kalau tidak, dirender ULANG SEKALI LAGI di DPISedang (skor >=
	// AmbangSedang) atau DPIBlur (skor lebih rendah lagi). Halaman-halaman
	// berikutnya lalu memakai DPI yang sama — TIDAK diukur ulang.
	DPIJelas     int
	DPISedang    int
	DPIBlur      int
	AmbangJelas  float64
	AmbangSedang float64

	// DPIPaksa, bila > 0, MELEWATI seluruh logika probe/adaptif di atas dan
	// memakai DPI ini langsung untuk setiap halaman. Dipakai saat
	// melanjutkan dokumen yang terhenti SETELAH halaman 1 selesai: DPI
	// halaman 1 (tersimpan di DB) diteruskan ke sini, supaya halaman
	// berikutnya tetap konsisten dengan halaman 1 — bukan menghitung ulang
	// seolah halaman itu "halaman pertama".
	DPIPaksa int

	AutoCrop     bool
	OCRClient    *localllm.Client
	OCRMaxTokens int
}

// DefaultOCRPrompt adalah SATU-SATUNYA prompt yang didukung GLM-OCR untuk teks
// biasa; kalimat instruksi bebas membuat keluarannya tak terduga. Karena itu
// ia konstanta, bukan pengaturan.
const DefaultOCRPrompt = "Text Recognition:"

// blankInkRatio: halaman dengan proporsi tinta di bawah ini dianggap kosong
// dan tidak dikirim ke model — menghemat waktu sekaligus menghindari
// pengulangan degeneratif pada halaman hampa.
const blankInkRatio = 0.0004

const defaultOCRMaxTokens = 2048

type Extractor struct {
	cfg  Config
	sink PageSink

	// dpiKeputusan menyimpan DPI yang sudah diputuskan untuk dokumen yang
	// SEDANG diproses dalam satu pemanggilan Document() — diisi sekali di
	// halaman pertama yang diproses, dipakai apa adanya untuk sisa halaman.
	// 0 berarti belum diputuskan.
	dpiKeputusan int
}

func New(cfg Config, sink PageSink) *Extractor {
	return &Extractor{cfg: cfg, sink: sink}
}

// PageCount melaporkan jumlah halaman sebuah PDF tanpa memprosesnya.
//
// Dipakai saat melanjutkan dokumen: baris kemajuan membutuhkan angka total
// SEBELUM halaman pertama diproses, sedangkan angka itu biasanya baru
// diketahui dari hasil pemrosesan. Tanpa ini, persentase pada tahap
// perbaikan tertunda selalu tampil 0%.
func PageCount(pdfPath string) (int, error) {
	doc, err := raster.Open(pdfPath)
	if err != nil {
		return 0, err
	}
	defer doc.Close()
	return doc.NumPages(), nil
}

// Document memproses satu PDF halaman demi halaman. Mengembalikan jumlah
// halaman dokumen dan apakah proses dihentikan lebih awal oleh PageSink.
func (e *Extractor) Document(ctx context.Context, pdfPath string) (totalPages int, stopped bool, err error) {
	doc, err := raster.Open(pdfPath)
	if err != nil {
		return 0, false, err
	}
	defer doc.Close()

	n := doc.NumPages()
	if n == 0 {
		return 0, false, fmt.Errorf("pdf tanpa halaman")
	}

	for i := 1; i <= n; i++ {
		if err := ctx.Err(); err != nil {
			return n, false, err
		}
		has, err := e.sink.HasPage(ctx, i)
		if err != nil {
			return n, false, err
		}
		if has {
			continue
		}
		e.sink.OnProgress(i, n, "menyiapkan halaman")
		res, err := e.ocrPage(ctx, doc, i, n)
		if err != nil {
			return n, false, fmt.Errorf("halaman %d: %w", i, err)
		}
		stop, err := e.sink.OnPage(ctx, res)
		if err != nil {
			return n, false, err
		}
		if stop {
			return n, true, nil
		}
	}
	return n, false, nil
}

// renderAdaptif merender halaman dengan DPI yang dipilih otomatis lewat skor
// ketajaman (raster.Page.BlurScore, varians Laplacian) — TAPI HANYA SEKALI
// PER DOKUMEN, di halaman pertama yang benar-benar diproses. Halaman
// berikutnya memakai e.dpiKeputusan apa adanya, tanpa probe/render ulang
// sama sekali.
//
// c.DPIPaksa, bila diisi pemanggil (lihat Config), melewati SEMUA logika di
// bawah dan langsung dipakai sebagai e.dpiKeputusan — dipakai saat
// melanjutkan dokumen yang halaman 1-nya sudah diproses di penjalanan
// sebelumnya.
//
// skorProbe hanya bernilai (>0) pada RENDER YANG MEMUTUSKAN — nol pada
// halaman-halaman berikutnya yang cuma mewarisi keputusan itu, supaya
// pemanggil tahu skor mana yang benar-benar dipakai mengambil keputusan.
// SELALU dari render di DPIJelas (bukan dari render akhir bila terjadi
// render ulang): mencampur skor dari DPI berbeda akan menyesatkan kalibrasi,
// karena varians Laplacian ikut membesar sekadar karena resolusinya naik,
// bukan karena halamannya makin tajam.
func (e *Extractor) renderAdaptif(doc *raster.Doc, pageNum int) (pg raster.Page, dpiPakai int, skorProbe float64, err error) {
	c := e.cfg

	// Sudah diputuskan (baik oleh probe di halaman sebelumnya, atau
	// dipaksakan pemanggil lewat DPIPaksa) — pakai apa adanya.
	if e.dpiKeputusan > 0 {
		pg, err = doc.Render(pageNum, raster.Opts{DPI: e.dpiKeputusan, AutoCrop: c.AutoCrop})
		return pg, e.dpiKeputusan, 0, err
	}
	if c.DPIPaksa > 0 {
		e.dpiKeputusan = c.DPIPaksa
		pg, err = doc.Render(pageNum, raster.Opts{DPI: e.dpiKeputusan, AutoCrop: c.AutoCrop})
		return pg, e.dpiKeputusan, 0, err
	}

	dpiJelas, dpiSedang, dpiBlur := c.DPIJelas, c.DPISedang, c.DPIBlur
	if dpiJelas <= 0 {
		dpiJelas = 100
	}
	if dpiSedang <= 0 {
		dpiSedang = 150
	}
	if dpiBlur <= 0 {
		dpiBlur = 200
	}

	probe, err := doc.Render(pageNum, raster.Opts{DPI: dpiJelas, AutoCrop: c.AutoCrop})
	if err != nil {
		return raster.Page{}, 0, 0, err
	}
	skorProbe = probe.BlurScore

	switch {
	case skorProbe >= c.AmbangJelas:
		e.dpiKeputusan = dpiJelas
		return probe, dpiJelas, skorProbe, nil
	case skorProbe >= c.AmbangSedang:
		e.dpiKeputusan = dpiSedang
	default:
		e.dpiKeputusan = dpiBlur
	}
	pg, err = doc.Render(pageNum, raster.Opts{DPI: e.dpiKeputusan, AutoCrop: c.AutoCrop})
	return pg, e.dpiKeputusan, skorProbe, err
}

// ocrPage merender lalu meng-OCR SATU halaman.
func (e *Extractor) ocrPage(ctx context.Context, doc *raster.Doc, pageNum, total int) (PageResult, error) {
	c := e.cfg
	maxTokens := c.OCRMaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultOCRMaxTokens
	}

	pg, dpiPakai, skorProbe, err := e.renderAdaptif(doc, pageNum)
	if err != nil {
		return PageResult{}, fmt.Errorf("rasterisasi: %w", err)
	}
	// Ke log info.log — dipakai mengalibrasi AmbangJelas/AmbangSedang: lihat
	// sebaran skor mentahnya di korpus nyata sebelum menyesuaikan ambang di
	// .env. skorProbe nol berarti halaman ini mewarisi keputusan halaman
	// sebelumnya (tidak diukur ulang).
	if skorProbe > 0 {
		logx.Info("hal %d/%d — blur_score=%.0f -> DPI=%d", pageNum, total, skorProbe, dpiPakai)
	}

	if pg.InkRatio < blankInkRatio {
		logx.Skip("hal %d/%d — kosong, dilewati", pageNum, total)
		return PageResult{
			Page: pageNum, Total: total, W: pg.W, H: pg.H, IsEmpty: true, InkRatio: pg.InkRatio,
			DPI: dpiPakai, BlurScoreProbe: skorProbe, PNG: pg.PNG,
			Notes: []string{"halaman kosong (tanpa teks) — OCR dilewati"},
		}, nil
	}

	started := time.Now()
	// DPI TAMPIL DI KONSOL (permintaan user, 2026-07-22) — sebelumnya cuma
	// masuk log/info.log. Dimensi & DPI sama-sama berguna dipantau langsung
	// selagi proses jalan, bukan cuma ditinjau belakangan dari berkas log.
	dims := fmt.Sprintf("DPI %d · %dx%d px", dpiPakai, pg.W, pg.H)
	params := localllm.Params{
		MaxTokens: maxTokens,
		OnStage: func(stage string) {
			e.sink.OnProgress(pageNum, total, dims+" · "+stage)
		},
		OnToken: func(n int) {
			// Tampilkan berkala saja: memperbarui tiap token justru
			// membanjiri terminal dan memperlambat proses.
			if n%32 == 0 {
				e.sink.OnProgress(pageNum, total,
					fmt.Sprintf("%s · %d token · %s", dims, n, time.Since(started).Round(time.Second)))
			}
		},
	}
	res, err := c.OCRClient.Vision(ctx, DefaultOCRPrompt, pg.PNG, params)
	if err != nil {
		return PageResult{}, fmt.Errorf("OCR: %w", err)
	}
	text := res.Text
	truncated := res.Truncated
	var notes []string
	if truncated {
		notes = append(notes, "keluaran OCR terpotong (batas token) — teks halaman mungkin tidak lengkap")
		logx.Warn("hal %d/%d — keluaran terpotong", pageNum, total)
	}

	croppedPct := 0.0
	if pg.CroppedFrom > 0 {
		croppedPct = 100 - float64(pg.W*pg.H)*100/float64(pg.CroppedFrom)
	}

	if cr := cleanOCRText(text); cr.Removed > 0 || cr.Degenerate {
		text = cr.Text
		if cr.Degenerate {
			notes = append(notes, "keluaran OCR didominasi pengulangan — perlu ditinjau")
			logx.Warn("hal %d/%d — keluaran berulang, dipangkas", pageNum, total)
		}
	}

	if strings.TrimSpace(text) == "" {
		if err := ctx.Err(); err != nil { // jangan mulai percobaan ulang saat shutdown
			return PageResult{}, err
		}
		retry, rerr := c.OCRClient.Vision(ctx, DefaultOCRPrompt, pg.PNG, params)
		if rerr != nil {
			return PageResult{}, fmt.Errorf("OCR (ulang): %w", rerr)
		}
		text = retry.Text
		if retry.Truncated {
			truncated = true
			notes = append(notes, "keluaran OCR terpotong pada percobaan kedua")
		}
		if cr := cleanOCRText(text); cr.Removed > 0 || cr.Degenerate {
			text = cr.Text
		}
		if strings.TrimSpace(text) == "" {
			notes = append(notes, "OCR menghasilkan teks kosong (2 percobaan)")
		}
	}

	return PageResult{
		Page: pageNum, Total: total, W: pg.W, H: pg.H,
		Text: text, IsTruncated: truncated,
		InkRatio: pg.InkRatio, CroppedPct: croppedPct,
		DPI: dpiPakai, BlurScoreProbe: skorProbe, PNG: pg.PNG,
		DurationMS: int(time.Since(started).Milliseconds()), Notes: notes,
	}, nil
}
