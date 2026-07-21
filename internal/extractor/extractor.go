// Package extractor mengubah PDF menjadi teks per halaman: tiap halaman
// dirasterisasi lalu di-OCR, hasilnya diserahkan ke PageStore pemanggil
// (Postgres, lihat internal/store) — TIDAK ADA file perantara di disk sama
// sekali (kecuali PDF sumbernya sendiri, yang tetap berguna untuk pratinjau).
//
// Tidak ada tahap "perbaikan typo" oleh model bahasa. Model bahasa tidak dapat
// membedakan salah ketik dari istilah yang tidak dikenalnya — pada korpus hukum
// yang penuh ejaan lama, istilah daerah (Qanun, Reusam, Keuchik), dan nomenklatur
// lama, "perbaikan" semacam itu justru mengubah isi secara diam-diam. Kesalahan
// OCR yang benar-benar merusak struktur ditangani secara deterministik dan dapat
// diaudit di internal/parser/ocrfix.go dan internal/parser/fuzzyfix.go.
//
// Deteksi "sudah selesai" dilakukan PER HALAMAN lewat PageStore.HasPage (bukan
// file-existence): bila OCR baru sampai halaman 4, menjalankan ulang akan
// meneruskan dari halaman 5.
//
// GATE: HANYA halaman 1 yang diperiksa (bukan beberapa halaman seperti versi
// lama) — identitas peraturan wajib ada di halaman 1 per Lampiran II UU
// 12/2011, lihat internal/parser/header.go. Bila gagal gate, sisa halaman
// TIDAK di-OCR sama sekali.
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

// PageStore adalah satu-satunya jembatan extractor ke penyimpanan permanen.
// Diimplementasikan oleh pemanggil (main.go, membungkus internal/store) —
// package ini sendiri TIDAK tahu dan tidak peduli itu Postgres atau apa pun,
// supaya tetap bisa diuji dengan implementasi in-memory tanpa DB sungguhan.
type PageStore interface {
	// HasPage melapor apakah halaman ini sudah punya hasil OCR tersimpan
	// (dasar resume per-halaman, menggantikan cek file-existence lama).
	HasPage(ctx context.Context, page int) (bool, error)
	// SavePage menyimpan hasil satu halaman. notes adalah catatan tinjauan
	// (halaman kosong, keluaran terpotong, dst) — disimpan sebagai daftar,
	// bisa lebih dari satu per halaman.
	SavePage(ctx context.Context, page int, text string, isEmpty, isTruncated bool, notes []string) error
	// ReadPages membaca halaman [a,b] (inklusif, 1-based) yang SUDAH
	// tersimpan; halaman yang belum ada dilewati begitu saja (bukan error).
	ReadPages(ctx context.Context, a, b int) ([]string, error)
}

// Config berisi parameter yang BENAR-BENAR perlu diubah pemanggil. Nilai yang
// sudah punya jawaban jelas (prompt OCR, ambang halaman-kosong) adalah
// KONSTANTA di bawah, bukan field — supaya tidak ada tombol yang bisa
// "diutak-atik jadi rusak" tanpa alasan (permintaan eksplisit user 2026-07-20).
type Config struct {
	AutoCrop bool
	DPI      int

	OCRClient    *localllm.Client
	OCRMaxTokens int

	// GateFunc memutuskan apakah dokumen ini layak diteruskan berdasarkan
	// teks HALAMAN 1 SAJA. WAJIB diisi pemanggil — lihat
	// internal/parser/header.go untuk pemeriksaan jenis/instansi/nomor/
	// tahun + kecocokan jurisdiksi yang dipakai main.go.
	GateFunc func(page1Text string) (accept bool, reason string)
}

// DefaultOCRPrompt adalah SATU-SATUNYA prompt yang didukung GLM-OCR untuk teks
// biasa. Model ini hanya dilatih untuk beberapa prompt tertentu — kalimat
// instruksi bebas membuat keluarannya tak terduga — jadi ini konstanta, bukan
// pengaturan.
const DefaultOCRPrompt = "Text Recognition:"

// blankInkRatio: halaman dengan proporsi tinta di bawah ini dianggap kosong
// dan TIDAK dikirim ke model — menghemat waktu sekaligus menghindari
// pengulangan degeneratif pada halaman hampa. Nilai ini hasil pengujian
// (lihat riwayat proyek), bukan sesuatu yang perlu di-tuning per instalasi.
const blankInkRatio = 0.0004

// ErrRejected dikembalikan bila dokumen ditolak gate kelayakan (bukan peraturan
// atau bukan produk jurisdiksi sumber ini).
var ErrRejected = fmt.Errorf("dokumen ditolak gate kelayakan")

// Extractor memproses SATU dokumen memakai klien inferensi yang sudah
// disiapkan pemanggil dan PageStore tujuan penyimpanan.
type Extractor struct {
	cfg   Config
	pages PageStore
}

func New(cfg Config, pages PageStore) *Extractor {
	return &Extractor{cfg: cfg, pages: pages}
}

// Document meng-OCR satu dokumen: halaman 1 dulu (lalu gate), baru sisanya
// bila lolos. Idempoten per halaman (PageStore.HasPage) — aman dipanggil
// berulang bila proses terhenti di tengah jalan.
func (e *Extractor) Document(ctx context.Context, pdfPath string) error {
	doc, err := raster.Open(pdfPath)
	if err != nil {
		return err
	}
	defer doc.Close()
	n := doc.NumPages()
	if n == 0 {
		return fmt.Errorf("pdf tanpa halaman")
	}

	has1, err := e.pages.HasPage(ctx, 1)
	if err != nil {
		return err
	}
	if !has1 {
		if err := e.ocrPage(ctx, doc, 1, n); err != nil {
			return fmt.Errorf("ocr halaman 1: %w", err)
		}
	}

	page1, err := e.pages.ReadPages(ctx, 1, 1)
	if err != nil {
		return err
	}
	var page1Text string
	if len(page1) > 0 {
		page1Text = page1[0]
	}
	if accept, _ := e.cfg.GateFunc(page1Text); !accept {
		return ErrRejected
	}

	for i := 2; i <= n; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		has, err := e.pages.HasPage(ctx, i)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if err := e.ocrPage(ctx, doc, i, n); err != nil {
			return fmt.Errorf("ocr lanjut hal %d: %w", i, err)
		}
	}
	return nil
}

// ocrPage merender lalu meng-OCR SATU halaman, menyimpan hasilnya lewat
// PageStore. Menyertakan penanganan halaman-kosong, keluaran-terpotong,
// pengulangan-degeneratif, dan satu percobaan ulang bila hasil kosong —
// semua perilaku ini sama seperti versi berbasis-file sebelumnya, hanya
// tujuan penyimpanannya yang berubah.
func (e *Extractor) ocrPage(ctx context.Context, doc *raster.Doc, pageNum, total int) error {
	c := e.cfg
	dpi := c.DPI
	if dpi == 0 {
		dpi = 200
	}

	pg, err := doc.Render(pageNum, raster.Opts{DPI: dpi, AutoCrop: c.AutoCrop})
	if err != nil {
		return fmt.Errorf("rasterisasi: %w", err)
	}

	if pg.InkRatio < blankInkRatio {
		logx.Skip("hal %d/%d — kosong, dilewati", pageNum, total)
		return e.pages.SavePage(ctx, pageNum, "", true, false,
			[]string{"halaman kosong (tanpa teks) — OCR dilewati"})
	}

	started := time.Now()
	res, err := c.OCRClient.Vision(ctx, DefaultOCRPrompt, pg.PNG, localllm.Params{MaxTokens: c.OCRMaxTokens})
	if err != nil {
		return fmt.Errorf("OCR: %w", err)
	}
	text := res.Text
	truncated := res.Truncated
	var notes []string
	if truncated {
		notes = append(notes, "keluaran OCR terpotong (batas token atau pembatalan) — teks halaman mungkin tidak lengkap")
		logx.Warn("hal %d/%d — keluaran terpotong", pageNum, total)
	}

	saved := ""
	if pg.CroppedFrom > 0 {
		cut := 100 - float64(pg.W*pg.H)*100/float64(pg.CroppedFrom)
		if cut >= 1 {
			saved = fmt.Sprintf(" (-%.1f%% piksel)", cut)
		}
	}
	logx.Progress("OCR", pageNum, total, "%dx%d px%s · %s",
		pg.W, pg.H, saved, time.Since(started).Round(time.Second))

	if cr := cleanOCRText(text); cr.Removed > 0 || cr.Degenerate {
		text = cr.Text
		if cr.Degenerate {
			notes = append(notes, "keluaran OCR didominasi pengulangan — kemungkinan model macet; perlu ditinjau")
			logx.Warn("hal %d/%d — keluaran berulang, dipangkas", pageNum, total)
		}
	}

	if strings.TrimSpace(text) == "" {
		retry, rerr := c.OCRClient.Vision(ctx, DefaultOCRPrompt, pg.PNG, localllm.Params{MaxTokens: c.OCRMaxTokens})
		if rerr != nil {
			return fmt.Errorf("OCR (ulang): %w", rerr)
		}
		text = retry.Text
		if retry.Truncated {
			truncated = true
			notes = append(notes, "keluaran OCR terpotong pada percobaan kedua")
			logx.Warn("hal %d/%d — keluaran terpotong (percobaan 2)", pageNum, total)
		}
		if cr := cleanOCRText(text); cr.Removed > 0 || cr.Degenerate {
			text = cr.Text
			if cr.Degenerate {
				notes = append(notes, "keluaran OCR didominasi pengulangan (percobaan 2)")
				logx.Warn("hal %d/%d — keluaran berulang, dipangkas (percobaan 2)", pageNum, total)
			}
		}
		if strings.TrimSpace(text) == "" {
			notes = append(notes, "OCR menghasilkan teks kosong (2 percobaan)")
		}
	}

	return e.pages.SavePage(ctx, pageNum, text, false, truncated, notes)
}
