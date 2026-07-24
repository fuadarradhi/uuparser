package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/fuadarradhi/uuparser/internal/extractor"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/store"
	"github.com/fuadarradhi/uuparser/internal/textcheck"
)

// resolveTextSource memutuskan APAKAH dokumen ini boleh memakai lapisan teks
// PDF (`pdftotext`) untuk sisa halamannya, alih-alih OCR penuh — lalu
// MENJALANKAN keputusan itu sekaligus.
//
// Alur (permintaan user, 2026-07-24):
//  1. OCR halaman 1 saja (lewat jalur biasa — classify TETAP jalan seperti
//     semula, jadi dokumen yang ditolak/duplikat di halaman 1 berhenti
//     persis seperti sebelum fitur ini ada).
//  2. Bandingkan hasil OCR halaman 1 dengan `pdftotext` halaman 1 yang sama
//     (internal/textcheck.Compare).
//  3. Cocok -> pdftotext dipakai untuk SISA dokumen, TANPA batas MaxPage
//     (permintaan eksplisit: mode poppler mengabaikan MAX_PAGE, beda dari
//     mode OCR yang memang dibatasi untuk keperluan uji cepat).
//  4. Tidak cocok -> ulangi di halaman 2.
//  5. Tetap tidak cocok -> serahkan ke pemanggil untuk OCR biasa (dibatasi
//     MaxPage bila diset) — TIDAK ADA yang diproses ulang, karena halaman 1
//     dan 2 yang sudah di-OCR pada langkah probe di atas tetap tersimpan
//     (per-page resume yang sudah ada menganggapnya "sudah selesai").
//
// Keputusan disimpan sekali di documents.text_source: pada dokumen yang
// dilanjutkan (resume) setelah keputusan pernah dibuat, seluruh langkah di
// atas DILEWATI — langsung ke pdftotext-mode atau langsung ke OCR biasa
// sesuai keputusan lama.
//
// Return:
//
//	handled = true  : dokumen SUDAH TUNTAS ditangani fungsi ini (baik lolos
//	                   ke pdftotext-mode sampai MarkOCRDone, ATAU dihentikan
//	                   classify di tengah probe — cek sink.stopped). Pemanggil
//	                   TIDAK perlu memanggil extractor.Document lagi.
//	useOCR  = true  : lanjutkan dengan jalur OCR biasa seperti sebelum fitur
//	                   ini ada (persis kode processDocument yang sudah ada).
func resolveTextSource(ctx context.Context, deps Deps, sink *docSink, job store.OCRJob, nAsli int) (handled, useOCR bool, err error) {
	// Fitur nonaktif atau pdftotext tak terpasang -> selalu OCR, IDENTIK
	// dengan perilaku sebelum fitur ini ditambahkan. Sengaja TIDAK menyentuh
	// text_source sama sekali di jalur ini, supaya keadaan "fitur mati"
	// tidak meninggalkan jejak apa pun di database.
	if !deps.TextCheck {
		return false, true, nil
	}
	if !textcheck.Available() {
		logx.Warn("textcheck: biner pdftotext tak ditemukan di PATH — TEXT_CHECK diabaikan, OCR penuh (install poppler-utils untuk mengaktifkan)")
		return false, true, nil
	}

	existing, err := deps.Store.GetTextSource(ctx, job.ID)
	if err != nil {
		return false, false, err
	}
	switch existing {
	case "ocr":
		return false, true, nil
	case "pdftotext":
		return true, false, runPdftotextMode(ctx, deps, sink, job, nAsli)
	}

	// Belum diputuskan: coba halaman 1, lalu halaman 2.
	for _, probePage := range []int{1, 2} {
		if probePage > nAsli {
			break
		}

		// dpiPaksa dibaca ULANG tiap iterasi, persis pola di processDocument:
		// begitu halaman 1 sudah tersimpan (dari iterasi sebelumnya),
		// halaman 2 HARUS mewarisi DPI yang sama, bukan menghitung ulang
		// skor ketajaman seolah halaman 2 adalah "halaman pertama".
		dpiPaksa, derr := deps.Store.DPIPage1(ctx, job.ID)
		if derr != nil {
			logx.Warn("textcheck: baca DPI halaman 1: %v", derr)
		}

		ex := extractor.New(extractor.Config{
			DPIJelas: deps.DPIJelas, DPISedang: deps.DPISedang, DPIBlur: deps.DPIBlur,
			AmbangJelas: deps.AmbangJelas, AmbangSedang: deps.AmbangSedang,
			DPIPaksa:  dpiPaksa,
			AutoCrop:  ocrAutoCrop,
			OCRClient: deps.Vision, OCRMaxTokens: ocrMaxTokens,
			MaxPage: probePage,
		}, sink)

		_, stopped, derr := ex.Document(ctx, job.PDFPath)
		if derr != nil {
			return false, false, derr
		}
		if stopped {
			if sink.tierSwitch {
				// BUKAN classify yang menghentikan — cuma tier hemat
				// (PENJELASAN/LAMPIRAN) yang kebetulan terdeteksi pada
				// probe 1-2 halaman ini (kasus yang praktis mustahil untuk
				// dokumen legal sungguhan, lihat catatan di OnPage). Reset
				// flag lalu lanjutkan probe seperti biasa — sink.tier
				// sendiri TETAP tersimpan, dipakai nanti oleh jalur OCR
				// utama.
				sink.tierSwitch = false
			} else {
				// classify sudah menghentikan dokumen (bukan peraturan/
				// duplikat) — sink.stopped sudah terisi, pemanggil
				// menanganinya persis seperti jalur OCR biasa.
				return true, false, nil
			}
		}

		ocrText, gerr := deps.Store.GetPageText(ctx, job.ID, probePage)
		if gerr != nil {
			return false, false, gerr
		}
		if strings.TrimSpace(ocrText) == "" {
			// Halaman kosong (mis. sampul polos) tak bisa jadi dasar
			// perbandingan yang berarti — lanjut coba halaman berikutnya.
			continue
		}

		pdfText, perr := textcheck.ExtractRange(ctx, job.PDFPath, probePage, probePage)
		if perr != nil {
			logx.Warn("textcheck: pdftotext hal %d gagal (%v) — OCR penuh", probePage, perr)
			if serr := deps.Store.SetTextSource(ctx, job.ID, "ocr"); serr != nil {
				logx.Warn("textcheck: tandai text_source=ocr: %v", serr)
			}
			return false, true, nil
		}

		cmp := textcheck.Compare(ocrText, pdfText)
		logx.Info("textcheck: hal %d — kemiripan=%.3f digit_cocok=%v trusted=%v",
			probePage, cmp.Similarity, cmp.DigitsMatch, cmp.Trusted)

		if cmp.Trusted {
			if serr := deps.Store.SetTextSource(ctx, job.ID, "pdftotext"); serr != nil {
				return false, false, serr
			}
			logx.OK("textcheck: lapisan teks PDF terpercaya (cocok di hal %d) — sisa dokumen pakai pdftotext, MAX_PAGE diabaikan", probePage)
			return true, false, runPdftotextMode(ctx, deps, sink, job, nAsli)
		}
	}

	// Halaman 1 dan 2 sama-sama tidak cocok (atau dokumen sengaja diakhiri
	// lebih pendek) -> OCR biasa, dibatasi MaxPage seperti sebelum fitur ini
	// ada.
	if serr := deps.Store.SetTextSource(ctx, job.ID, "ocr"); serr != nil {
		logx.Warn("textcheck: tandai text_source=ocr: %v", serr)
	}
	return false, true, nil
}

// runPdftotextMode mengisi SELURUH halaman dokumen (1..nAsli) memakai teks
// lapisan PDF, TANPA model visi dan TANPA batas MaxPage — sesuai permintaan
// eksplisit user bahwa mode poppler mengabaikan MAX_PAGE.
//
// Dipanggil selalu dari halaman 1 (bukan hanya sisanya setelah probe):
// halaman yang sudah tersimpan dari tahap probe di resolveTextSource (atau
// dari penjalanan sebelumnya, saat dokumen ini dilanjutkan) otomatis
// dilewati lewat sink.HasPage — sama seperti extractor.Document menangani
// resume per-halaman.
func runPdftotextMode(ctx context.Context, deps Deps, sink *docSink, job store.OCRJob, nAsli int) error {
	pages, err := textcheck.ExtractPages(ctx, job.PDFPath, 1, nAsli)
	if err != nil {
		return fmt.Errorf("pdftotext hal 1..%d: %w", nAsli, err)
	}

	for i, text := range pages {
		page := i + 1
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

		sink.OnProgress(page, nAsli, "PDF2Text · diambil dari lapisan teks PDF")

		isEmpty := strings.TrimSpace(text) == ""
		notes := []string{"diambil dari lapisan teks PDF (pdftotext) — tidak di-OCR; lihat documents.text_source"}
		if isEmpty {
			notes = []string{"halaman kosong pada lapisan teks PDF"}
		}

		if _, err := sink.OnPage(ctx, extractor.PageResult{
			Page: page, Total: nAsli, Text: text, IsEmpty: isEmpty, Notes: notes,
		}); err != nil {
			return fmt.Errorf("halaman %d (pdftotext): %w", page, err)
		}
	}

	return deps.Store.MarkOCRDone(ctx, job.ID, nAsli)
}
