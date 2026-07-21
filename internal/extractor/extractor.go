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
	DurationMS  int
	Notes       []string
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
	DPI          int
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

// ocrPage merender lalu meng-OCR SATU halaman.
func (e *Extractor) ocrPage(ctx context.Context, doc *raster.Doc, pageNum, total int) (PageResult, error) {
	c := e.cfg
	dpi := c.DPI
	if dpi == 0 {
		dpi = 200
	}
	maxTokens := c.OCRMaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultOCRMaxTokens
	}

	pg, err := doc.Render(pageNum, raster.Opts{DPI: dpi, AutoCrop: c.AutoCrop})
	if err != nil {
		return PageResult{}, fmt.Errorf("rasterisasi: %w", err)
	}

	if pg.InkRatio < blankInkRatio {
		logx.Skip("hal %d/%d — kosong, dilewati", pageNum, total)
		return PageResult{
			Page: pageNum, Total: total, W: pg.W, H: pg.H, IsEmpty: true, InkRatio: pg.InkRatio,
			Notes: []string{"halaman kosong (tanpa teks) — OCR dilewati"},
		}, nil
	}

	started := time.Now()
	dims := fmt.Sprintf("%dx%d px", pg.W, pg.H)
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
		DurationMS: int(time.Since(started).Milliseconds()), Notes: notes,
	}, nil
}
