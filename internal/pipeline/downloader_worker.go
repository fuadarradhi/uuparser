package pipeline

import (
	"context"
	"path/filepath"
	"time"

	"github.com/fuadarradhi/uuparser/internal/config"
	"github.com/fuadarradhi/uuparser/internal/downloader"
	"github.com/fuadarradhi/uuparser/internal/fsutil"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/store"
)

// Nilai tetap — bukan config, karena jawabannya sudah pasti (permintaan user
// 2026-07-20: jangan bikin tombol untuk sesuatu yang tidak akan pernah diubah).
const (
	downloaderRegisterInterval = 2 * time.Minute // seberapa sering cek dokumen baru per sumber
	downloaderDelay            = 1500 * time.Millisecond
	downloaderMaxRetries       = 3
	downloaderHTTPTimeout      = 120 * time.Second
	downloaderMaxAttempts      = 3
)

// downloaderWorker mendaftarkan dokumen baru dari tiap sumber lalu mengunduh
// PDF untuk yang berstatus 'pending'. Berjalan independen dari OCR/parser —
// PDF baru selalu siap sebelum OCR sempat memintanya.
func downloaderWorker(ctx context.Context, cfg config.Config, st *store.Store) {
	for {
		if ctx.Err() != nil {
			return
		}
		registerNewDocuments(ctx, st)
		drainDownloads(ctx, cfg, st)

		select {
		case <-ctx.Done():
			return
		case <-time.After(downloaderRegisterInterval):
		}
	}
}

func dlConfig() downloader.Config {
	return downloader.Config{
		Delay: downloaderDelay, MaxRetries: downloaderMaxRetries, HTTPTimeout: downloaderHTTPTimeout,
	}
}

func registerNewDocuments(ctx context.Context, st *store.Store) {
	rows, err := st.ListSources(ctx)
	if err != nil {
		logx.Warn("downloader: daftar sumber: %v", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		cfg := dlConfig()
		cfg.Endpoint = row.EndpointURL
		src, err := downloader.NewSource(downloader.SourceRow{
			Code: row.Code, EndpointURL: row.EndpointURL, SourceType: row.SourceType,
			SourceConfigRaw: row.SourceConfigRaw,
		}, cfg)
		if err != nil {
			logx.Warn("downloader: %v", err)
			continue
		}
		docs, err := src.ListDocuments(ctx)
		if err != nil {
			logx.Warn("downloader: sumber %s tak terjangkau (%v) — dicoba lagi nanti", row.Code, err)
			continue
		}
		n := 0
		for _, d := range docs {
			slug := downloader.Slug(downloader.Record{IDData: d.IDData, FileDownload: d.Meta["file_download"]})
			if err := st.UpsertDocumentMeta(ctx, row.IDStr, d.IDData, d.Judul, slug, d.FileURL); err != nil {
				logx.Warn("downloader: daftar %s: %v", d.IDData, err)
				continue
			}
			n++
		}
		logx.Info("downloader: %s — %d dokumen diperiksa dari sumber", row.Code, n)
	}
}

func drainDownloads(ctx context.Context, cfg config.Config, st *store.Store) {
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := st.ClaimForDownload(ctx)
		if err == store.ErrNoWork {
			return
		}
		if err != nil {
			logx.Warn("downloader: klaim gagal: %v", err)
			return
		}
		processOneDownload(ctx, cfg, st, job)
	}
}

func processOneDownload(ctx context.Context, cfg config.Config, st *store.Store, job store.DownloadJob) {
	row, err := st.GetSource(ctx, job.SourceID)
	if err != nil {
		logx.Warn("downloader: sumber %s tak ditemukan: %v", job.SourceID, err)
		_ = st.MarkDownloadFailed(ctx, job.ID, err.Error(), downloaderMaxAttempts)
		return
	}
	dc := dlConfig()
	dc.Endpoint = row.EndpointURL
	src, err := downloader.NewSource(downloader.SourceRow{
		Code: row.Code, EndpointURL: row.EndpointURL, SourceType: row.SourceType,
	}, dc)
	if err != nil {
		_ = st.MarkDownloadFailed(ctx, job.ID, err.Error(), downloaderMaxAttempts)
		return
	}

	body, err := src.FetchPDF(ctx, downloader.RemoteDoc{FileURL: job.PDFURL})
	if err != nil {
		logx.Fail(job.Slug, "unduh gagal: %v", err)
		_ = st.MarkDownloadFailed(ctx, job.ID, err.Error(), downloaderMaxAttempts)
		return
	}

	// Satu-satunya berkas yang tetap hidup di disk: PDF-nya sendiri (untuk
	// pratinjau). Semua yang lain (metadata, teks OCR, hasil parse) langsung
	// ke Postgres, tidak lewat perantara file.
	pdfDir := filepath.Join(cfg.DataDir, row.Code, "pdf")
	dest := filepath.Join(pdfDir, job.Slug+".pdf")
	if err := fsutil.WriteFileAtomic(dest, body, 0o644); err != nil {
		logx.Fail(job.Slug, "simpan PDF gagal: %v", err)
		_ = st.MarkDownloadFailed(ctx, job.ID, err.Error(), downloaderMaxAttempts)
		return
	}

	if err := st.MarkDownloaded(ctx, job.ID, dest, sha256Hex(body)); err != nil {
		logx.Warn("downloader: tandai selesai %s: %v", job.Slug, err)
		return
	}
	logx.Step("unduh", "%s", job.Slug)
	time.Sleep(downloaderDelay)
}
