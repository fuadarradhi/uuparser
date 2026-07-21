package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/fuadarradhi/uuparser/internal/config"
	"github.com/fuadarradhi/uuparser/internal/extractor"
	"github.com/fuadarradhi/uuparser/internal/localllm"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/parser"
	"github.com/fuadarradhi/uuparser/internal/store"
)

// Nilai tetap — lihat catatan di downloader_worker.go soal kenapa ini
// konstanta, bukan config.
const (
	ocrIdleInterval = 30 * time.Second
	ocrDPI          = 200
	ocrAutoCrop     = true
	ocrMaxTokens    = 2048
)

// ocrWorker adalah SATU goroutine saja: model yzma/llama.cpp dimuat sekali
// dan tidak aman dipakai konkuren dari banyak goroutine sekaligus.
func ocrWorker(ctx context.Context, cfg config.Config, st *store.Store, model *localllm.Client) {
	for {
		if ctx.Err() != nil {
			return
		}
		processed := drainOCR(ctx, st, model)
		if !processed {
			model.Release() // tak ada kerjaan: lepas memori sampai dokumen baru datang
			select {
			case <-ctx.Done():
				return
			case <-time.After(ocrIdleInterval):
			}
		}
	}
}

func drainOCR(ctx context.Context, st *store.Store, model *localllm.Client) (processedAny bool) {
	for {
		if ctx.Err() != nil {
			return processedAny
		}
		job, err := st.ClaimForOCR(ctx)
		if err == store.ErrNoWork {
			return processedAny
		}
		if err != nil {
			logx.Warn("ocr: klaim gagal: %v", err)
			return processedAny
		}
		processedAny = true
		processOneOCR(ctx, st, model, job)
	}
}

func processOneOCR(ctx context.Context, st *store.Store, model *localllm.Client, job store.OCRJob) {
	var headerRejectReason, headerExtractedInstansi, headerStructureType string
	var headerIsRegulation, headerPreUU122011 bool

	exCfg := extractor.Config{
		DPI: ocrDPI, AutoCrop: ocrAutoCrop,
		OCRClient: model, OCRMaxTokens: ocrMaxTokens,
		GateFunc: func(page1Text string) (bool, string) {
			h := parser.ExtractHeader(page1Text)
			headerIsRegulation = h.Found
			headerStructureType = h.StructureType
			headerExtractedInstansi = h.Instansi
			headerPreUU122011 = h.PreUU122011

			if !h.Found {
				headerRejectReason = "no_legal_signal"
				return false, headerRejectReason
			}
			// Kecocokan jurisdiksi: instansi di halaman 1 harus cocok PERSIS
			// dengan instansi pemilik source (lihat internal/parser/header.go
			// soal kenapa exact-match, bukan Contains). Dokumen pra-2011
			// tetap diterima OCR PENUH di sini supaya ada teks lengkap untuk
			// ditinjau manusia — ApplyHeaderResult di bawah yang memutuskan
			// status akhirnya jadi 'review_manual', bukan gate ini.
			if !headerPreUU122011 && !parser.MatchesJurisdiction(h.Instansi, job.SourceInstansi) {
				headerRejectReason = "wrong_jurisdiction"
				return false, headerRejectReason
			}
			return true, ""
		},
	}

	pages := dbPageStore{st: st, documentID: job.ID}
	ex := extractor.New(exCfg, pages)
	err := ex.Document(ctx, job.PDFPath)

	// Hasil header HANYA ditulis bila gate benar-benar sempat berjalan
	// (sukses atau ditolak gate). Pada kegagalan OCR transien (disk/model),
	// headerIsRegulation masih zero-value false — menulisnya akan MENOLAK
	// dokumen secara keliru dan permanen. (Temuan review eksternal, valid.)
	if err == nil || errors.Is(err, extractor.ErrRejected) {
		if applyErr := st.ApplyHeaderResult(ctx, job.ID, headerIsRegulation, headerRejectReason,
			headerStructureType, headerExtractedInstansi, headerPreUU122011); applyErr != nil {
			logx.Warn("ocr: simpan header check %s: %v", job.ID, applyErr)
		}
	}

	switch {
	case err == extractor.ErrRejected:
		logx.Info("ocr: %s ditolak gate (%s)", job.ID, headerRejectReason)
	case err != nil:
		logx.Fail(job.ID, "OCR gagal: %v", err)
		_ = st.MarkOCRFailed(ctx, job.ID, err.Error(), maxAttempts)
	default:
		if err := st.MarkOCRDone(ctx, job.ID); err != nil {
			logx.Warn("ocr: tandai selesai %s: %v", job.ID, err)
			return
		}
		logx.OK("ocr selesai · %s", job.ID)
	}
}
