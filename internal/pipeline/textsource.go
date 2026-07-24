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
// PDF (bukan model visi) untuk sisa halamannya, alih-alih OCR penuh — lalu
// MENJALANKAN keputusan itu sekaligus.
//
// Alur (permintaan user, 2026-07-24; diperluas 2026-07-24 dengan mesin
// kedua — lihat catatan MuPDF di bawah):
//  1. OCR halaman 1 saja (lewat jalur biasa — classify TETAP jalan seperti
//     semula, jadi dokumen yang ditolak/duplikat di halaman 1 berhenti
//     persis seperti sebelum fitur ini ada).
//  2. Bandingkan hasil OCR halaman 1 dengan lapisan teks PDF halaman yang
//     sama — dicoba DUA mesin SECARA BERURUTAN: pdftotext (poppler) dulu,
//     lalu MuPDF (lewat internal/raster) sebagai CADANGAN bila poppler
//     gagal/tak-cocok. [Ditambahkan 2026-07-24, permintaan user: "kesal
//     pdftotext kebanyakan gagal, jadinya semua ke OCR"] MuPDF adalah mesin
//     PDF yang TERPISAH SEPENUHNYA dari poppler (decoder font/ToUnicode
//     CMak beda — mirip beda Chromium/PDFium atau Firefox/PDF.js dari
//     poppler), jadi kadang berhasil tepat di kasus yang bikin pdftotext
//     gagal. TANPA dependensi/biner tambahan — MuPDF sudah ke-link lewat
//     go-fitz yang sama dipakai untuk merender halaman.
//  3. Salah satu cocok -> mesin YANG COCOK itu dipakai untuk SISA dokumen,
//     TANPA batas PageCountRange (permintaan eksplisit: mode lapisan-teks
//     mengabaikan PAGE_COUNT_RANGE, beda dari mode OCR yang memang
//     dibatasi untuk keperluan uji cepat).
//  4. Dua-duanya tidak cocok -> ulangi di halaman 2.
//  5. Tetap tidak cocok -> serahkan ke pemanggil untuk OCR biasa (dibatasi
//     PageCountRange bila diset) — TIDAK ADA yang diproses ulang, karena
//     halaman 1 dan 2 yang sudah di-OCR pada langkah probe di atas tetap
//     tersimpan (per-page resume yang sudah ada menganggapnya "sudah
//     selesai").
//
// Keputusan disimpan sekali di documents.text_source ("pdftotext" |
// "mupdf" | "ocr"): pada dokumen yang dilanjutkan (resume) setelah
// keputusan pernah dibuat, seluruh langkah di atas DILEWATI — langsung ke
// mode lapisan-teks (mesin yang sama seperti sebelumnya) atau langsung ke
// OCR biasa sesuai keputusan lama.
//
// Return:
//
//	handled = true  : dokumen SUDAH TUNTAS ditangani fungsi ini (baik lolos
//	                   ke mode lapisan-teks sampai MarkOCRDone, ATAU
//	                   dihentikan classify di tengah probe — cek
//	                   sink.stopped). Pemanggil TIDAK perlu memanggil
//	                   extractor.Document lagi.
//	useOCR  = true  : lanjutkan dengan jalur OCR biasa seperti sebelum fitur
//	                   ini ada (persis kode processDocument yang sudah ada).
func resolveTextSource(ctx context.Context, deps Deps, sink *docSink, job store.OCRJob, nAsli int) (handled, useOCR bool, err error) {
	// Fitur nonaktif -> selalu OCR, IDENTIK dengan perilaku sebelum fitur
	// ini ditambahkan. Sengaja TIDAK menyentuh text_source sama sekali di
	// jalur ini, supaya keadaan "fitur mati" tidak meninggalkan jejak apa
	// pun di database.
	//
	// CATATAN: TIDAK ADA lagi gerbang "pdftotext tak terpasang -> matikan
	// seluruh fitur" di sini (dulu ada) — MuPDF SELALU tersedia (lewat
	// go-fitz, tanpa biner eksternal), jadi absennya pdftotext saja bukan
	// alasan mematikan textcheck; poppler cuma dilewati, MuPDF tetap dicoba
	// (lihat tryTextLayerPage).
	if !deps.TextCheck {
		return false, true, nil
	}

	existing, err := deps.Store.GetTextSource(ctx, job.ID)
	if err != nil {
		return false, false, err
	}
	switch existing {
	case "ocr":
		return false, true, nil
	case "pdftotext", "mupdf":
		return true, false, runTextLayerMode(ctx, deps, sink, job, nAsli, existing)
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

		engine, cmp := tryTextLayerPage(ctx, job.PDFPath, probePage, ocrText)
		if engine != "" {
			if serr := deps.Store.SetTextSource(ctx, job.ID, engine); serr != nil {
				return false, false, serr
			}
			logx.OK("textcheck: lapisan teks PDF terpercaya via %s (cocok di hal %d) — sisa dokumen pakai %s, PAGE_COUNT_RANGE diabaikan",
				engine, probePage, engine)
			return true, false, runTextLayerMode(ctx, deps, sink, job, nAsli, engine)
		}
		logx.Info("textcheck: hal %d — tak ada mesin lapisan-teks yang cocok (kemiripan terbaik=%.3f digit_cocok=%v)",
			probePage, cmp.Similarity, cmp.DigitsMatch)
	}

	// Halaman 1 dan 2 sama-sama tidak cocok di KEDUA mesin (atau dokumen
	// sengaja diakhiri lebih pendek) -> OCR biasa, dibatasi PageCountRange
	// seperti sebelum fitur ini ada.
	if serr := deps.Store.SetTextSource(ctx, job.ID, "ocr"); serr != nil {
		logx.Warn("textcheck: tandai text_source=ocr: %v", serr)
	}
	return false, true, nil
}

// tryTextLayerPage mencoba MENGEKSTRAK & MEMBANDINGKAN teks halaman `page`
// terhadap ocrText, pdftotext (poppler) DULU lalu MuPDF sebagai cadangan —
// mengembalikan SEGERA begitu salah satu Trusted (lihat textcheck.Compare).
// engine="" berarti tidak ada satu pun yang dipercaya (atau dua-duanya
// gagal diekstrak sama sekali) — cmp yang dikembalikan adalah percobaan
// TERAKHIR yang berhasil diekstrak (buat log), zero-value bila kedua mesin
// gagal total.
func tryTextLayerPage(ctx context.Context, pdfPath string, page int, ocrText string) (engine string, cmp textcheck.CompareResult) {
	if textcheck.Available() {
		if t, err := textcheck.ExtractRange(ctx, pdfPath, page, page); err != nil {
			logx.Info("textcheck: pdftotext hal %d gagal (%v), coba mupdf", page, err)
		} else {
			cmp = textcheck.Compare(ocrText, t)
			if cmp.Trusted {
				return "pdftotext", cmp
			}
		}
	}

	if t, err := textcheck.ExtractRangeMuPDF(pdfPath, page, page); err != nil {
		logx.Info("textcheck: mupdf hal %d juga gagal (%v)", page, err)
	} else {
		mupdfCmp := textcheck.Compare(ocrText, t)
		if mupdfCmp.Trusted {
			return "mupdf", mupdfCmp
		}
		cmp = mupdfCmp // simpan punya mupdf buat log (dicoba paling akhir)
	}

	return "", cmp
}

// runTextLayerMode mengisi SELURUH halaman dokumen (1..nAsli) memakai
// lapisan teks PDF dari SATU mesin yang sudah terbukti cocok (engine:
// "pdftotext" | "mupdf" — lihat tryTextLayerPage), TANPA model visi dan
// TANPA batas PageCountRange — sesuai permintaan eksplisit user bahwa mode
// lapisan-teks mengabaikan PAGE_COUNT_RANGE.
//
// Dipanggil selalu dari halaman 1 (bukan hanya sisanya setelah probe):
// halaman yang sudah tersimpan dari tahap probe di resolveTextSource (atau
// dari penjalanan sebelumnya, saat dokumen ini dilanjutkan) otomatis
// dilewati lewat sink.HasPage — sama seperti extractor.Document menangani
// resume per-halaman.
func runTextLayerMode(ctx context.Context, deps Deps, sink *docSink, job store.OCRJob, nAsli int, engine string) error {
	var pages []string
	var err error
	switch engine {
	case "mupdf":
		pages, err = textcheck.ExtractPagesMuPDF(job.PDFPath, 1, nAsli)
	default: // "pdftotext"
		pages, err = textcheck.ExtractPages(ctx, job.PDFPath, 1, nAsli)
	}
	if err != nil {
		return fmt.Errorf("%s hal 1..%d: %w", engine, nAsli, err)
	}

	// Tag konsol tetap "PDF2Text" generik utk KEDUA mesin (permintaan user
	// sebelumnya: tag sesederhana mungkin) — mesin mana yang sebenarnya
	// dipakai tercatat di notes per-halaman (document_pages.notes) buat
	// yang perlu menelusuri lebih detail nanti.
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
		notes := []string{fmt.Sprintf("diambil dari lapisan teks PDF (%s) — tidak di-OCR; lihat documents.text_source", engine)}
		if isEmpty {
			notes = []string{"halaman kosong pada lapisan teks PDF"}
		}

		if _, err := sink.OnPage(ctx, extractor.PageResult{
			Page: page, Total: nAsli, Text: text, IsEmpty: isEmpty, Notes: notes,
		}); err != nil {
			return fmt.Errorf("halaman %d (%s): %w", page, engine, err)
		}
	}

	return deps.Store.MarkOCRDone(ctx, job.ID, nAsli)
}
