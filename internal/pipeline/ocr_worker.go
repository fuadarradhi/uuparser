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
	logx.Progress(d.ocred, total, "hal %d · %s", page, detail)
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
	logx.Progress(d.ocred, r.Total, "hal %d · %dx%d px%s · %s",
		r.Page, r.W, r.H, cropNote(r.CroppedPct), durText(r.DurationMS))

	if r.IsEmpty {
		// Halaman kosong tidak bisa diklasifikasi. Klasifikasi TIDAK
		// dilewatkan begitu saja: ia menunggu halaman berisi pertama (lihat
		// blok di bawah). Tanpa itu, dokumen yang halaman pertamanya kosong
		// karena artefak pindaian akan diproses sampai selesai tanpa pernah
		// diperiksa.
		return false, nil
	}

	// Bagian penutup: bila penandanya ada, uraikan. Model teks hanya
	// dipanggil untuk bagian yang penandanya ada tetapi tak dapat diuraikan
	// pola baku — lihat trigger.go.
	if err := d.uraiPenetapan(ctx, r); err != nil {
		return false, err
	}

	// 3) Halaman BERISI PERTAMA menentukan nasib dokumen. Biasanya halaman 1;
	//    bila halaman 1 kosong, pemeriksaan bergeser ke halaman berisi
	//    berikutnya alih-alih hilang sama sekali.
	if !d.classified {
		if done, derr := d.deps.Store.IsClassified(ctx, d.docID); derr == nil && done {
			d.classified = true
			return false, nil
		}
		return d.classify(ctx, r.Text)
	}
	return false, nil
}

// classify membaca halaman 1 lewat model teks lalu memutuskan: tolak (bukan
// peraturan), tandai duplikat, atau lanjutkan ke halaman berikutnya.
func (d *docSink) classify(ctx context.Context, halaman string) (bool, error) {
	// Tahap 1 — gerbang. Pertanyaan tunggal yang sempit: produk hukum atau
	// bukan. Dipisah dari pembacaan identitas karena model kecil lebih andal
	// menjawab satu hal daripada banyak hal sekaligus.
	if d.deps.LowMemory {
		d.deps.Vision.Release()
	}
	produkHukum, alasan, err := AskIsRegulation(ctx, d.deps.Text, d.prompts.Gate, halaman,
		d.textProgress(d.ocred, d.total, "memeriksa jenis dokumen"))
	if err != nil {
		if d.deps.LowMemory {
			d.deps.Text.Release()
		}
		return true, fmt.Errorf("pemeriksaan produk hukum: %w", err)
	}

	if !produkHukum {
		if d.deps.LowMemory {
			d.deps.Text.Release()
		}
		if alasan == "" {
			alasan = "model menilai dokumen ini bukan produk hukum"
		}
		if err := d.deps.Store.RejectNotRegulation(ctx, d.docID, alasan); err != nil {
			return true, err
		}
		d.stopped = "bukan peraturan: " + alasan
		return true, nil
	}

	// Tahap 2 — identitas. Model hanya menyalin apa yang tertulis; pemetaan
	// instansi dan penurunan angka urut dikerjakan kode (lihat normalize.go).
	meta, err := AskIdentity(ctx, d.deps.Text, d.prompts.Identity, halaman,
		d.textProgress(d.ocred, d.total, "membaca identitas"))
	if d.deps.LowMemory {
		d.deps.Text.Release()
	}
	if err != nil {
		return true, fmt.Errorf("membaca identitas: %w", err)
	}
	meta.IsPeraturan = true

	dup, err := d.deps.Store.ApplyMetaAndCheckDuplicate(ctx, d.docID, meta, canonicalKey(meta))
	if err != nil {
		return true, err
	}
	if dup {
		d.stopped = "duplikat peraturan yang sudah ada"
		return true, nil
	}

	d.classified = true
	logx.Info("dokumen dikenali: %s %s Nomor %s Tahun %s — %s",
		meta.Jenis, meta.Instansi, meta.Nomor, meta.Tahun, meta.Tentang)
	if meta.InstansiTertulis != meta.Instansi {
		logx.Info("instansi dipetakan: %q -> %q", meta.InstansiTertulis, meta.Instansi)
	}
	return false, nil
}

// uraiPenetapan menangani bagian penutup dokumen.
//
// Model teks hanya dipanggil bila penandanya ADA tetapi isinya TIDAK dapat
// diuraikan pola baku — pemakaian yang sangat jarang, sehingga biayanya kecil
// sementara cakupannya tetap luas untuk dokumen yang formatnya menyimpang.
func (d *docSink) uraiPenetapan(ctx context.Context, r extractor.PageResult) error {
	h := UraiPenetapan(r.Text)
	if !h.AdaPenanda {
		return nil
	}

	p := store.Penetapan{
		DitetapkanDi: h.DitetapkanDi, DitetapkanTanggal: h.DitetapkanTanggal,
		DitetapkanOleh: h.DitetapkanOleh, DiundangkanDi: h.DiundangkanDi,
		DiundangkanTanggal: h.DiundangkanTanggal, DiundangkanOleh: h.DiundangkanOleh,
	}

	if h.PerluModel {
		logx.Info("hal %d: bagian penetapan tidak sesuai pola baku — model teks dipanggil", r.Page)
		if d.deps.LowMemory {
			d.deps.Vision.Release()
		}
		mp, err := AskPenetapan(ctx, d.deps.Text, d.prompts.Penetapan, r.Text,
			d.textProgress(r.Page, r.Total, "membaca bagian penetapan"))
		if d.deps.LowMemory {
			d.deps.Text.Release()
		}
		if err != nil {
			return fmt.Errorf("membaca bagian penetapan halaman %d: %w", r.Page, err)
		}
		// Hasil deterministik didahulukan; model hanya mengisi yang kosong.
		p = gabungPenetapan(p, mp)
	}

	return d.deps.Store.SavePenetapan(ctx, d.docID, p)
}

// gabungPenetapan memakai nilai dari penguraian pola baku bila ada, dan nilai
// dari model hanya untuk bagian yang masih kosong. Urutan ini disengaja:
// aturan yang pasti selalu lebih dipercaya daripada pembacaan model.
func gabungPenetapan(pasti, model store.Penetapan) store.Penetapan {
	pilih := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	return store.Penetapan{
		DitetapkanDi:       pilih(pasti.DitetapkanDi, model.DitetapkanDi),
		DitetapkanTanggal:  pilih(pasti.DitetapkanTanggal, model.DitetapkanTanggal),
		DitetapkanOleh:     pilih(pasti.DitetapkanOleh, model.DitetapkanOleh),
		DiundangkanDi:      pilih(pasti.DiundangkanDi, model.DiundangkanDi),
		DiundangkanTanggal: pilih(pasti.DiundangkanTanggal, model.DiundangkanTanggal),
		DiundangkanOleh:    pilih(pasti.DiundangkanOleh, model.DiundangkanOleh),
	}
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
	if ocred, _, cerr := d.deps.Store.CountPagesDone(ctx, d.docID); cerr == nil {
		d.ocred = ocred
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


// textProgress membuat pelapor kemajuan untuk pekerjaan model teks, sehingga
// konsol tetap bergerak selama tahap yang paling lama diam.
func (d *docSink) textProgress(page, total int, what string) localllm.TextParams {
	return localllm.TextParams{
		OnStage: func(stage string) {
			logx.Progress(d.ocred, total, "hal %d · %s: %s", page, what, stage)
		},
		OnToken: func(n int) {
			if n%32 == 0 {
				logx.Progress(d.ocred, total, "hal %d · %s: %d token", page, what, n)
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
