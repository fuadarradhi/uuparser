package pipeline

import (
	"context"
	"path/filepath"
	"time"

	"github.com/fuadarradhi/uuparser/internal/downloader"
	"github.com/fuadarradhi/uuparser/internal/fsutil"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/store"
)

// Nilai tetap — bukan pengaturan, karena jawabannya sudah pasti.
const (
	downloaderRegisterInterval = 2 * time.Minute
	downloaderDelay            = 1500 * time.Millisecond
	downloaderMaxRetries       = 3
	downloaderHTTPTimeout      = 120 * time.Second

	// maxAttempts berlaku untuk unduh DAN OCR: setelah sekian kegagalan
	// dokumen berstatus 'failed' dan tidak dicoba lagi sampai di-reset
	// manual lewat SQL.
	maxAttempts = 3
)

// downloaderWorker HANYA mengumpulkan tautan dan mengunduh berkasnya. Ia
// tidak menyimpan judul/jenis/tahun dari sumber sama sekali — seluruh
// identitas peraturan dibaca belakangan dari halaman pertama berkas oleh
// model teks (keputusan 2026-07-21: jangan percaya metadata sumber).
func downloaderWorker(ctx context.Context, deps Deps) {
	for {
		if ctx.Err() != nil {
			return
		}
		registerNewURLs(ctx, deps)
		drainDownloads(ctx, deps)

		select {
		case <-ctx.Done():
			return
		case <-time.After(downloaderRegisterInterval):
		}
	}
}

func dlConfig(endpoint string) downloader.Config {
	return downloader.Config{
		Endpoint: endpoint, Delay: downloaderDelay,
		MaxRetries: downloaderMaxRetries, HTTPTimeout: downloaderHTTPTimeout,
	}
}

func registerNewURLs(ctx context.Context, deps Deps) {
	rows, err := deps.Store.ListSources(ctx)
	if err != nil {
		logx.Warn("downloader: daftar sumber: %v", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		src, err := downloader.NewSource(downloader.SourceRow{
			Code: row.Code, EndpointURL: row.EndpointURL, SourceType: row.SourceType,
			SourceConfigRaw: row.SourceConfigRaw,
		}, dlConfig(row.EndpointURL))
		if err != nil {
			logx.Warn("downloader: %v", err)
			continue
		}
		docs, err := src.ListDocuments(ctx)
		if err != nil {
			logx.Warn("downloader: sumber %s tak terjangkau (%v) — dicoba lagi nanti", row.Code, err)
			continue
		}
		baru := 0
		lewati := 0
		for _, d := range docs {
			// MinTahun menyaring SEBELUM didaftarkan — dokumen yang lolos
			// tetap dipakai sort_tahun-nya untuk urutan seperti biasa.
			// Dokumen TANPA sort_tahun (nil) tidak disaring: lebih baik
			// diproses daripada hilang karena metadata sumber tak
			// menyediakannya. Lihat config.Config.MinTahun.
			if deps.MinTahun > 0 && d.SortTahun != nil && *d.SortTahun < deps.MinTahun {
				lewati++
				continue
			}
			isNew, err := deps.Store.RegisterURL(ctx, row.ID, d.FileURL, d.SortTahun, d.SortNomor)
			if err != nil {
				logx.Warn("downloader: daftar tautan: %v", err)
				continue
			}
			if isNew {
				baru++
			}
		}
		// Sengaja tanpa log per-sumber saat tidak ada tautan baru: unduhan
		// bukan tahap yang perlu dipantau di konsol (permintaan user).
		if baru > 0 {
			logx.Info("%s: %d tautan baru", row.Code, baru)
		}
		if lewati > 0 {
			logx.Info("%s: %d tautan dilewati (tahun < MIN_TAHUN)", row.Code, lewati)
		}
	}
}

func drainDownloads(ctx context.Context, deps Deps) {
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := deps.Store.ClaimForDownload(ctx)
		if err == store.ErrNoWork {
			return
		}
		if err != nil {
			logx.Warn("downloader: klaim gagal: %v", err)
			return
		}
		processOneDownload(ctx, deps, job)
	}
}

func processOneDownload(ctx context.Context, deps Deps, job store.DownloadJob) {
	// Endpoint sumber tidak diperlukan untuk mengunduh berkas: tautannya
	// sudah lengkap. Konfigurasi HTTP generik sudah cukup.
	body, err := downloader.DownloadPDF(ctx, dlConfig(""), job.DownloadURL)
	if err != nil {
		// Pembatalan (Ctrl+C) bukan kegagalan dokumen: kembalikan ke antrian
		// tanpa menambah penghitung percobaan, agar beberapa kali Ctrl+C
		// tidak membuat dokumen sehat berakhir 'failed'.
		if isTransient(err) {
			_ = deps.Store.RequeueDocument(job.ID, "pending")
			return
		}
		logx.Fail(job.DownloadURL, "unduh gagal: %v", err)
		_ = deps.Store.MarkDownloadFailed(context.Background(), job.ID, err.Error(), maxAttempts)
		return
	}

	// Nama berkas diturunkan dari hash isinya dan disimpan di SATU folder:
	// dua sumber yang menyajikan berkas identik menunjuk ke berkas fisik
	// yang sama, tanpa duplikasi di disk.
	sha := sha256Hex(body)
	dest := filepath.Join(deps.DataDir, "pdf", downloader.FileName(sha))
	if err := fsutil.WriteFileAtomic(dest, body, 0o644); err != nil {
		logx.Fail(job.DownloadURL, "simpan PDF gagal: %v", err)
		_ = deps.Store.MarkDownloadFailed(context.Background(), job.ID, err.Error(), maxAttempts)
		return
	}

	dup, err := deps.Store.MarkDownloaded(ctx, job.ID, dest, sha, int64(len(body)))
	if err != nil {
		logx.Warn("downloader: tandai selesai: %v", err)
		return
	}
	_ = dup // unduhan tidak dilaporkan ke konsol; statusnya terlihat di database
	time.Sleep(downloaderDelay)
}
