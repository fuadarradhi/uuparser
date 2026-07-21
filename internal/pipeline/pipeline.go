// Package pipeline menjalankan tiga worker independen (downloader, OCR,
// parser) yang berkoordinasi lewat Postgres — lihat CATATAN-MIGRASI.md untuk
// latar belakang arsitektur lengkap. main.go hanya memanggil Run.
package pipeline

import (
	"context"
	"sync"
	"time"

	"github.com/fuadarradhi/uuparser/internal/config"
	"github.com/fuadarradhi/uuparser/internal/localllm"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/store"
)

// shutdownGrace: batas tunggu worker menyelesaikan pekerjaan berjalan setelah
// sinyal berhenti. OCR satu halaman bisa makan waktu bermenit-menit; daripada
// menggantung tanpa batas (dan akhirnya di-SIGKILL systemd tanpa log apa pun),
// lewat batas ini Run menyerah secara eksplisit — progres tetap aman karena
// state per-halaman/per-dokumen sudah di Postgres.
const shutdownGrace = 30 * time.Second

// Run menjalankan ketiga worker sampai ctx dibatalkan (sinyal berhenti),
// lalu menunggu mereka selesai paling lama shutdownGrace.
func Run(ctx context.Context, cfg config.Config, st *store.Store, model *localllm.Client) {
	var wg sync.WaitGroup
	wg.Add(3)

	go func() { defer wg.Done(); downloaderWorker(ctx, cfg, st) }()
	go func() { defer wg.Done(); ocrWorker(ctx, cfg, st, model) }()
	go func() { defer wg.Done(); parserWorker(ctx, st) }()

	<-ctx.Done()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		logx.Info("semua worker berhenti.")
	case <-time.After(shutdownGrace):
		logx.Warn("worker belum selesai dalam %s — keluar paksa (progres aman di database)", shutdownGrace)
	}
}
