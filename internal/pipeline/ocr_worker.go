package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/fuadarradhi/uuparser/internal/extractor"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/prompts"
	"github.com/fuadarradhi/uuparser/internal/store"
)

// ocr_worker.go menjalankan tahap kedua: OCR + perbaikan + klasifikasi.
//
// Urutannya SENGAJA berurutan per halaman (OCR hal-N -> perbaiki hal-N ->
// OCR hal-N+1 -> ...) sehingga kemajuan terpantau per halaman di UI dan
// dokumen yang gagal di halaman 1 langsung ditinggalkan tanpa membuang waktu
// meng-OCR sisanya.
//
// Kedua model (visi + teks) dimuat SEKALI dan tetap menempel selama masih ada
// antrian; keduanya dilepas bersamaan hanya ketika tidak ada lagi pekerjaan.
// Tidak ada bongkar-pasang model per halaman.

const (
	ocrIdleInterval = 30 * time.Second
	ocrAutoCrop     = true
	ocrMaxTokens    = 2048
)

func ocrWorker(ctx context.Context, deps Deps) {
	for {
		if ctx.Err() != nil {
			return
		}
		worked := drainOCR(ctx, deps)
		if !worked {
			// Antrian habis: lepaskan KEDUA model agar memori tidak tertahan
			// selama menunggu (penting di mesin 8 GB).
			deps.Vision.Release()
			deps.Text.Release()
			select {
			case <-ctx.Done():
				return
			case <-time.After(ocrIdleInterval):
			}
		}
	}
}

func drainOCR(ctx context.Context, deps Deps) (workedAny bool) {
	for {
		if ctx.Err() != nil {
			return workedAny
		}
		job, err := deps.Store.ClaimForOCR(ctx)
		if err == store.ErrNoWork {
			return workedAny
		}
		if err != nil {
			logx.Warn("ocr: klaim gagal: %v", err)
			return workedAny
		}
		workedAny = true
		processDocument(ctx, deps, job)
	}
}

// docSink menjalankan aturan per halaman: simpan OCR mentah, perbaiki dengan
// model teks, dan — khusus halaman 1 — klasifikasikan dokumen lalu putuskan
// apakah proses diteruskan.
type docSink struct {
	deps    Deps
	docID   string
	prompts prompts.Set
	stopped string // alasan berhenti, kosong bila lanjut
	fixed   int    // halaman yang sudah selesai diperbaiki model teks
	ocred   int    // halaman yang sudah selesai di-OCR
}

// OnProgress mencetak baris kemajuan [diperbaiki/di-OCR/total]. Dipanggil
// juga SEBELUM halaman pertama di-OCR, sehingga konsol langsung menunjukkan
// [0/0/N] alih-alih diam sampai halaman pertama selesai.
func (d *docSink) OnProgress(page, total int, detail string) {
	logx.Progress(d.fixed, d.ocred, total, "hal %d · %s", page, detail)
}

func (d *docSink) HasPage(ctx context.Context, page int) (bool, error) {
	return d.deps.Store.HasPage(ctx, d.docID, page)
}

func (d *docSink) OnPage(ctx context.Context, r extractor.PageResult) (bool, error) {
	// 1) Simpan hasil OCR MENTAH lebih dulu — apa pun yang terjadi setelah
	//    ini, teks asli sudah aman dan tidak akan pernah ditimpa.
	if err := d.deps.Store.SavePage(ctx, d.docID, r.Page, r.Text, r.IsEmpty, r.IsTruncated,
		r.InkRatio, r.CroppedPct, r.DurationMS, r.Notes); err != nil {
		return false, err
	}
	// Baris kemajuan SETELAH OCR, SEBELUM perbaikan: angka pertama (sudah
	// diperbaiki) memang tertinggal satu dari angka kedua (sudah di-OCR).
	d.ocred = r.Page
	logx.Progress(d.fixed, d.ocred, r.Total, "hal %d · %dx%d px%s · %s",
		r.Page, r.W, r.H, cropNote(r.CroppedPct), durText(r.DurationMS))

	if r.IsEmpty {
		d.fixed++ // halaman kosong dianggap selesai: tak ada yang perlu diperbaiki
		return false, nil
	}

	// 2) Perbaiki salah ketik/struktur. Kegagalan model TIDAK menggagalkan
	//    dokumen — teks mentah tetap dipakai (lihat fixPage).
	fixed, ok := fixPage(ctx, d.deps.Text, d.prompts.FixPage, r.Text)
	if ok {
		if err := d.deps.Store.SaveFixedText(ctx, d.docID, r.Page, fixed,
			countChangedOps(r.Text, fixed), d.prompts.FixPageHash); err != nil {
			return false, err
		}
	} else {
		logx.Warn("hal %d: perbaikan model dilewati — memakai teks OCR mentah", r.Page)
	}
	d.fixed++
	logx.Progress(d.fixed, d.ocred, r.Total, "hal %d · diperbaiki", r.Page)

	// 3) Halaman pertama menentukan nasib dokumen.
	if r.Page == 1 {
		text := r.Text
		if ok {
			text = fixed
		}
		return d.classify(ctx, text)
	}
	return false, nil
}

// classify membaca halaman 1 lewat model teks lalu memutuskan: tolak (bukan
// peraturan), tandai duplikat, atau lanjutkan ke halaman berikutnya.
func (d *docSink) classify(ctx context.Context, page1 string) (bool, error) {
	meta, mencabut, mengubah, err := classifyPage1(ctx, d.deps.Text, d.prompts.Classify, page1)
	if err != nil {
		// Model gagal membaca: JANGAN menolak dokumen (bisa jadi masalah
		// sesaat). Hentikan dokumen ini, biarkan status dikembalikan oleh
		// pemanggil supaya dicoba lagi nanti.
		return true, fmt.Errorf("klasifikasi halaman 1: %w", err)
	}

	if !meta.IsPeraturan {
		alasan := meta.Alasan
		if alasan == "" {
			alasan = "model menilai dokumen ini bukan produk hukum"
		}
		if err := d.deps.Store.RejectNotRegulation(ctx, d.docID, alasan); err != nil {
			return true, err
		}
		d.stopped = "bukan peraturan: " + alasan
		return true, nil
	}

	dup, err := d.deps.Store.ApplyMetaAndCheckDuplicate(ctx, d.docID, meta, canonicalKey(meta))
	if err != nil {
		return true, err
	}
	if dup {
		d.stopped = "duplikat peraturan yang sudah ada"
		return true, nil
	}

	// Relasi yang disebut di halaman 1 dicatat sebagai petunjuk awal;
	// relations.go tetap menjadi sumber utama saat parsing nanti.
	for _, v := range mencabut {
		_ = d.deps.Store.InsertRelation(ctx, d.docID, "mencabut", "", "", "", "", "", v, "perlu_review", v)
	}
	for _, v := range mengubah {
		_ = d.deps.Store.InsertRelation(ctx, d.docID, "mengubah", "", "", "", "", "", v, "perlu_review", v)
	}

	logx.Info("dokumen dikenali: %s %s Nomor %s Tahun %s — %s",
		meta.Jenis, meta.Instansi, meta.Nomor, meta.Tahun, meta.Tentang)
	return false, nil
}

func processDocument(ctx context.Context, deps Deps, job store.OCRJob) {
	sink := &docSink{deps: deps, docID: job.ID, prompts: deps.Prompts}

	ex := extractor.New(extractor.Config{
		DPI: deps.DPI, AutoCrop: ocrAutoCrop,
		OCRClient: deps.Vision, OCRMaxTokens: ocrMaxTokens,
	}, sink)

	total, stopped, err := ex.Document(ctx, job.PDFPath)
	switch {
	case err != nil:
		logx.Fail(job.ID, "proses gagal: %v", err)
		_ = deps.Store.MarkOCRFailed(ctx, job.ID, err.Error(), maxAttempts)
	case stopped:
		// Status sudah disetel oleh classify (rejected/duplicate).
		logx.Skip("dokumen dihentikan — %s", sink.stopped)
	default:
		if err := deps.Store.MarkOCRDone(ctx, job.ID, total); err != nil {
			logx.Warn("ocr: tandai selesai: %v", err)
			return
		}
		logx.OK("ocr+perbaikan selesai · %d halaman", total)
	}
}

// cropNote merangkum penghematan piksel hasil pemotongan margin, atau string
// kosong bila pemotongannya tidak berarti.
func cropNote(pct float64) string {
	if pct < 1 {
		return ""
	}
	return fmt.Sprintf(" · dipotong -%.0f%%", pct)
}

func durText(ms int) string {
	return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
}
