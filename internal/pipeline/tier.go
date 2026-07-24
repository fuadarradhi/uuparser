package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/fuadarradhi/uuparser/internal/extractor"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/raster"
	"github.com/fuadarradhi/uuparser/internal/store"
	"github.com/fuadarradhi/uuparser/internal/textcheck"
)

// blankInkRatioTier: sama seperti angka blank-page milik extractor (tidak
// diekspor dari sana) — dipakai di sini supaya halaman kosong pada tier
// hemat tidak dipaksa lewat Tesseract/GLM-OCR percuma. Duplikasi satu
// konstanta kecil ini dianggap lebih murah daripada mengekspor internal
// extractor cuma untuk ini.
const blankInkRatioTier = 0.0004

// tierPDFTextMinChars: pdftotext dianggap BERHASIL untuk tier hemat bila
// panjang teksnya (sudah di-TrimSpace) setidaknya sekian karakter. SENGAJA
// TIDAK divalidasi lebih jauh lewat textcheck.Compare (beda filosofi dari
// resolveTextSource): di sana ketepatan halaman batang tubuh harus
// dijamin sama-persis dengan OCR; di sini datanya secara eksplisit
// dianggap sekunder (permintaan user) — cukup "ada teksnya", bukan
// "teksnya terverifikasi akurat".
const tierPDFTextMinChars = 3

// runTieredMode melanjutkan dokumen SETELAH docSink.OnPage mendeteksi masuk
// ke bagian PENJELASAN atau LAMPIRAN (lihat docSink.tier/tierSwitch di
// ocr_worker.go) — dipanggil processDocument menggantikan pemanggilan
// ex.Document() biasa untuk SISA halaman dokumen.
//
// Per halaman, urutan percobaan:
//  1. pdftotext (paling murah, tanpa render sama sekali).
//  2. Gagal/terlalu pendek -> render halaman (CPU saja) untuk memeriksa
//     apakah memang kosong.
//  3. Tidak kosong -> tier "penjelasan": Tesseract. Tier "lampiran":
//     GLM-OCR penuh (lewat Extractor.OCRSinglePage) — LAMPIRAN sering
//     berisi tabel/peta/struktur organisasi yang tetap butuh model visi,
//     Tesseract SENGAJA tidak dipakai untuk tier ini.
//
// docSink.tier bisa naik dari "penjelasan" ke "lampiran" DI TENGAH loop ini
// (dibaca ulang tiap iterasi lewat sink.tier) — LAMPIRAN selalu di akhir
// dokumen, jadi transisinya cuma satu arah.
func runTieredMode(ctx context.Context, deps Deps, sink *docSink, job store.OCRJob, nAsli int) error {
	dpiPaksa, derr := deps.Store.DPIPage1(ctx, job.ID)
	if derr != nil {
		logx.Warn("tier: baca DPI halaman 1: %v", derr)
	}

	for page := sink.ocred + 1; page <= nAsli; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		has, herr := sink.HasPage(ctx, page)
		if herr != nil {
			return herr
		}
		if has {
			continue
		}

		tier := sink.tier

		text, notes, terr := ocrTieredPage(ctx, deps, sink, job.PDFPath, page, nAsli, tier, dpiPaksa)
		if terr != nil {
			return fmt.Errorf("halaman %d (tier %s): %w", page, tier, terr)
		}

		isEmpty := strings.TrimSpace(text) == ""
		if _, err := sink.OnPage(ctx, extractor.PageResult{
			Page: page, Total: nAsli, Text: text, IsEmpty: isEmpty, Notes: notes,
		}); err != nil {
			return fmt.Errorf("halaman %d: %w", page, err)
		}
		// Nilai balik `stop` dari OnPage sengaja diabaikan di sini: itu
		// cuma sinyal PERUBAHAN tier (mis. penjelasan -> lampiran di
		// tengah loop ini), sudah tertangkap lewat sink.tier yang dibaca
		// ulang di awal iterasi berikutnya — bukan penghentian dokumen
		// sungguhan.
	}

	return deps.Store.MarkOCRDone(ctx, job.ID, nAsli)
}

// ocrTieredPage memutuskan & menjalankan strategi SATU halaman sesuai tier.
//
// sink diteruskan HANYA supaya Extractor punya PageSink untuk baris
// kemajuan (OnProgress) selama panggilan GLM-OCR fallback berlangsung —
// OnPage/HasPage milik sink TIDAK disentuh dari sisi Extractor di sini
// (pemanggilnya, runTieredMode, yang memanggil sink.OnPage sendiri).
func ocrTieredPage(ctx context.Context, deps Deps, sink *docSink, pdfPath string, page, total int, tier string, dpiPaksa int) (text string, notes []string, err error) {
	if textcheck.Available() {
		sink.OnProgress(page, total, "PDF2Text · mencoba lapisan teks PDF")
		if t, perr := textcheck.ExtractRange(ctx, pdfPath, page, page); perr == nil &&
			len(strings.TrimSpace(t)) >= tierPDFTextMinChars {
			return t, []string{fmt.Sprintf("tier %s: diambil dari lapisan teks PDF (pdftotext)", tier)}, nil
		}
	}

	// pdftotext gagal/tak tersedia/terlalu pendek — render dulu utk
	// memastikan halaman ini memang berisi (murah, CPU saja, tanpa model)
	// sebelum memanggil Tesseract/GLM-OCR.
	//
	// CATATAN performa: ini artinya PDF dibuka lagi di sini walau
	// OCRSinglePage (jalur GLM-OCR di bawah) akan membukanya SEKALI LAGI
	// untuk render adaptifnya sendiri — dua kali buka utk halaman yang
	// jatuh ke GLM-OCR. Diterima sebagai penyederhanaan (bukan biaya
	// besar dibanding inferensi model) daripada mengekspor lebih banyak
	// internal extractor demi ini saja.
	doc, derr := raster.Open(pdfPath)
	if derr != nil {
		return "", nil, fmt.Errorf("rasterisasi: %w", derr)
	}
	defer doc.Close()

	dpi := dpiPaksa
	if dpi <= 0 {
		// Titik tengah yang wajar bila DPI halaman 1 belum diketahui:
		// halaman tier ini tidak lagi mengikuti probe ketajaman
		// DPIJelas/DPISedang/DPIBlur milik halaman batang tubuh — data
		// sekunder, tidak sepenting itu untuk dikalibrasi presisi.
		dpi = 150
	}
	pg, rerr := doc.Render(page, raster.Opts{DPI: dpi, AutoCrop: true})
	if rerr != nil {
		return "", nil, fmt.Errorf("render hal %d: %w", page, rerr)
	}
	if pg.InkRatio < blankInkRatioTier {
		return "", []string{"halaman kosong (tanpa teks) — dilewati"}, nil
	}

	glmFallback := func(alasan string) (string, []string, error) {
		ex := extractor.New(extractor.Config{
			DPIPaksa: dpi, AutoCrop: true,
			OCRClient: deps.Vision, OCRMaxTokens: ocrMaxTokens,
		}, sink)
		res, oerr := ex.OCRSinglePage(ctx, pdfPath, page, total)
		if oerr != nil {
			return "", nil, fmt.Errorf("GLM-OCR: %w", oerr)
		}
		return res.Text, append([]string{alasan}, res.Notes...), nil
	}

	switch tier {
	case "lampiran":
		// Tabel/peta/struktur organisasi butuh model visi — Tesseract
		// SENGAJA tidak dipakai untuk tier ini.
		return glmFallback("tier lampiran: pdftotext gagal, dipakai GLM-OCR (tabel/peta butuh model visi)")

	default: // "penjelasan"
		if !textcheck.TesseractAvailable() {
			logx.Warn("tier: tesseract tak terpasang — hal %d (penjelasan) jatuh ke GLM-OCR sebagai jaring pengaman", page)
			return glmFallback("tier penjelasan: pdftotext gagal & tesseract tak terpasang, dipakai GLM-OCR (jaring pengaman)")
		}
		sink.OnProgress(page, total, fmt.Sprintf("Tesseract · DPI %d · %dx%d px", dpi, pg.W, pg.H))
		t, terr := textcheck.RunTesseract(ctx, pg.PNG, deps.TesseractLang)
		if terr != nil {
			return "", nil, fmt.Errorf("tesseract: %w", terr)
		}
		return t, []string{"tier penjelasan: pdftotext gagal, dipakai Tesseract"}, nil
	}
}
