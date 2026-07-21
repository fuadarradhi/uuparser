package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fuadarradhi/uuparser/internal/extractor"
	"github.com/fuadarradhi/uuparser/internal/localllm"
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
	total   int    // jumlah halaman dokumen (0 bila belum diketahui)

	// classified menandai bahwa pemeriksaan "ini peraturan atau bukan" sudah
	// dijalankan untuk dokumen ini, sehingga tidak diulang tiap halaman.
	classified bool
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
	d.total = r.Total
	logx.Progress(d.fixed, d.ocred, r.Total, "hal %d · %dx%d px%s · %s",
		r.Page, r.W, r.H, cropNote(r.CroppedPct), durText(r.DurationMS))

	if r.IsEmpty {
		// Halaman kosong tidak bisa diklasifikasi dan tidak perlu diperbaiki.
		// Klasifikasi TIDAK dilewatkan begitu saja: ia menunggu halaman
		// berisi pertama (lihat blok klasifikasi di bawah). Tanpa itu,
		// dokumen yang halaman pertamanya kosong karena artefak pindaian
		// akan diproses sampai selesai tanpa pernah diperiksa.
		d.fixed++
		return false, nil
	}

	// 2) Perbaiki salah ketik/struktur. Kegagalan model TIDAK menggagalkan
	//    dokumen — teks mentah tetap dipakai (lihat fixPage).
	//
	// Pada mode hemat memori, model visi dilepas lebih dulu supaya kedua
	// model tidak pernah menempati memori bersamaan; model visi akan dimuat
	// ulang sendiri saat halaman berikutnya di-OCR.
	if d.deps.LowMemory {
		d.deps.Vision.Release()
	}
	fixed, ok, err := fixPage(ctx, d.deps.Text, d.prompts.FixPage, r.Text,
		d.textProgress(r.Page, r.Total, "perbaikan"))
	if d.deps.LowMemory {
		d.deps.Text.Release()
	}
	if err != nil {
		// Galat model/pembatalan: JANGAN tandai halaman ini selesai. Dokumen
		// dikembalikan ke antrian oleh pemanggil dan dilanjutkan nanti dari
		// halaman ini juga (teks OCR-nya sudah tersimpan, tidak diulang).
		return false, fmt.Errorf("perbaikan halaman %d: %w", r.Page, err)
	}
	if ok {
		if err := d.deps.Store.SaveFixedText(ctx, d.docID, r.Page, fixed,
			countChangedOps(r.Text, fixed), d.prompts.FixPageHash); err != nil {
			return false, err
		}
	} else {
		logx.Warn("hal %d: keluaran model tidak layak — memakai teks OCR mentah", r.Page)
	}
	d.fixed++
	logx.Progress(d.fixed, d.ocred, r.Total, "hal %d · diperbaiki", r.Page)

	// 3) Halaman BERISI PERTAMA menentukan nasib dokumen. Biasanya halaman 1;
	//    bila halaman 1 kosong, pemeriksaan bergeser ke halaman berisi
	//    berikutnya alih-alih hilang sama sekali.
	if !d.classified {
		if done, derr := d.deps.Store.IsClassified(ctx, d.docID); derr == nil && done {
			d.classified = true
			return false, nil
		}
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
	if d.deps.LowMemory {
		d.deps.Vision.Release()
	}
	meta, mencabut, mengubah, err := classifyPage1(ctx, d.deps.Text, d.prompts.Classify, page1,
		d.textProgress(1, d.total, "klasifikasi"))
	if d.deps.LowMemory {
		d.deps.Text.Release()
	}
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

	d.classified = true
	logx.Info("dokumen dikenali: %s %s Nomor %s Tahun %s — %s",
		meta.Jenis, meta.Instansi, meta.Nomor, meta.Tahun, meta.Tentang)
	return false, nil
}

func processDocument(ctx context.Context, deps Deps, job store.OCRJob) {
	sink := &docSink{deps: deps, docID: job.ID, prompts: deps.Prompts}

	// Jumlah halaman diambil lebih dulu supaya baris kemajuan sudah punya
	// angka total sejak tahap melanjutkan, bukan menunggu halaman pertama
	// diproses (yang membuat persentase tampil 0% terus).
	if n, perr := extractor.PageCount(job.PDFPath); perr == nil {
		sink.total = n
	}

	// Rapikan sisa pekerjaan dari penjalanan sebelumnya SEBELUM meng-OCR
	// halaman baru: memulihkan hitungan kemajuan, menyelesaikan perbaikan
	// yang tertunda, dan — yang paling penting — memastikan dokumen sudah
	// diklasifikasi.
	stop, err := sink.resume(ctx)
	if err != nil {
		finishInterrupted(deps, job.ID, "melanjutkan dokumen", err)
		return
	}
	if stop {
		logx.Skip("dokumen dihentikan — %s", sink.stopped)
		return
	}

	ex := extractor.New(extractor.Config{
		DPI: deps.DPI, AutoCrop: ocrAutoCrop,
		OCRClient: deps.Vision, OCRMaxTokens: ocrMaxTokens,
	}, sink)

	total, stopped, err := ex.Document(ctx, job.PDFPath)
	switch {
	case err != nil:
		finishInterrupted(deps, job.ID, "proses dokumen", err)
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

// finishInterrupted memutuskan apakah sebuah galat merupakan kegagalan DATA
// (dokumen memang bermasalah — hitung percobaan) atau gangguan SEMENTARA
// (pembatalan, model gagal dimuat, database sesaat tak terjangkau).
//
// Pembedaan ini penting: menekan Ctrl+C beberapa kali seharusnya tidak boleh
// membuat dokumen yang sehat berakhir berstatus 'failed' dan tak pernah
// diproses lagi.
func finishInterrupted(deps Deps, docID, what string, err error) {
	if isTransient(err) {
		// Kembalikan ke antrian apa adanya; dilanjutkan pada penjalanan
		// berikutnya, dari halaman tempatnya berhenti.
		if rerr := deps.Store.RequeueDocument(docID, "downloaded"); rerr != nil {
			logx.Warn("mengembalikan dokumen ke antrian: %v", rerr)
		}
		logx.Skip("%s dihentikan (%v) — dokumen dikembalikan ke antrian, akan dilanjutkan", what, err)
		return
	}
	logx.Fail(docID, "%s gagal: %v", what, err)
	_ = deps.Store.MarkOCRFailed(context.Background(), docID, err.Error(), maxAttempts)
}

// isTransient membedakan gangguan sementara dari kerusakan data.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return localllm.IsLoadError(err)
}

// resume merapikan sisa pekerjaan dokumen yang sebelumnya terhenti. Urutannya
// disengaja:
//
//  1. Pulihkan hitungan kemajuan dari basis data, supaya baris kemajuan
//     meneruskan angka sebelumnya alih-alih mulai dari [0/0/N] padahal
//     sebagian halaman sudah selesai.
//  2. Selesaikan perbaikan yang tertunda (halaman punya teks OCR tetapi
//     belum diperbaiki).
//  3. Pastikan dokumen SUDAH DIKLASIFIKASI. Ini yang paling penting:
//     pemeriksaan "ini peraturan atau bukan" hanya berjalan saat halaman 1
//     diproses. Bila halaman 1 sudah ada dari penjalanan sebelumnya, tahap
//     OCR melewatinya — sehingga tanpa langkah ini dokumen diteruskan sampai
//     selesai TANPA PERNAH diperiksa, dan dokumen bukan-peraturan pun lolos
//     ke tahap parse.
//
// Mengembalikan stop=true bila klasifikasi memutuskan dokumen ini tidak
// dilanjutkan (bukan peraturan, atau duplikat).
func (d *docSink) resume(ctx context.Context) (stop bool, err error) {
	if ocred, fixed, cerr := d.deps.Store.CountPagesDone(ctx, d.docID); cerr == nil {
		d.ocred, d.fixed = ocred, fixed
	}

	if err := d.catchUpFixes(ctx); err != nil {
		return false, err
	}

	done, err := d.deps.Store.IsClassified(ctx, d.docID)
	if err != nil {
		return false, err
	}
	if done {
		d.classified = true
		return false, nil
	}

	// Klasifikasi dari halaman BERISI pertama yang sudah tersimpan — tanpa
	// OCR ulang. Bila belum ada satu pun halaman berisi, klasifikasi berjalan
	// seperti biasa saat halaman pertama di-OCR nanti.
	_, text, ok, err := d.deps.Store.FirstNonEmptyPage(ctx, d.docID)
	if err != nil || !ok {
		return false, err
	}
	return d.classify(ctx, text)
}

// catchUpFixes memperbaiki halaman yang sudah di-OCR tetapi belum diperbaiki.
func (d *docSink) catchUpFixes(ctx context.Context) error {
	pending, err := d.deps.Store.ListPagesNeedingFix(ctx, d.docID)
	if err != nil {
		return err
	}
	for _, p := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		logx.Progress(d.fixed, d.ocred, d.total, "hal %d · melanjutkan perbaikan tertunda", p.PageNumber)

		if d.deps.LowMemory {
			d.deps.Vision.Release()
		}
		fixed, ok, ferr := fixPage(ctx, d.deps.Text, d.prompts.FixPage, p.OCRText,
			d.textProgress(p.PageNumber, d.total, "perbaikan tertunda"))
		if d.deps.LowMemory {
			d.deps.Text.Release()
		}
		if ferr != nil {
			return fmt.Errorf("perbaikan tertunda halaman %d: %w", p.PageNumber, ferr)
		}
		if !ok {
			logx.Warn("hal %d: keluaran model tidak layak — memakai teks OCR mentah", p.PageNumber)
			continue
		}
		if err := d.deps.Store.SaveFixedText(ctx, d.docID, p.PageNumber, fixed,
			countChangedOps(p.OCRText, fixed), d.prompts.FixPageHash); err != nil {
			return err
		}
		d.fixed++
	}
	return nil
}

// textProgress membuat pelapor kemajuan untuk pekerjaan model teks, sehingga
// konsol tetap bergerak selama tahap yang paling lama diam.
func (d *docSink) textProgress(page, total int, what string) localllm.TextParams {
	return localllm.TextParams{
		OnStage: func(stage string) {
			logx.Progress(d.fixed, d.ocred, total, "hal %d · %s: %s", page, what, stage)
		},
		OnToken: func(n int) {
			if n%32 == 0 {
				logx.Progress(d.fixed, d.ocred, total, "hal %d · %s: %d token", page, what, n)
			}
		},
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
