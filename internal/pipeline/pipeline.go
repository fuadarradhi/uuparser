// Package pipeline menjalankan tiga worker independen (downloader, OCR,
// parser) yang berkoordinasi lewat Postgres — lihat CATATAN-MIGRASI.md untuk
// latar belakang arsitektur lengkap. main.go hanya memanggil Run.
package pipeline

import (
	"context"
	"sync"

	"github.com/fuadarradhi/uuparser/internal/config"
	"github.com/fuadarradhi/uuparser/internal/localllm"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/store"
)

// Run menjalankan ketiga worker sampai ctx dibatalkan (sinyal berhenti).
// Diblokir sampai semuanya keluar.
func Run(ctx context.Context, cfg config.Config, st *store.Store, model *localllm.Client) {
	var wg sync.WaitGroup
	wg.Add(3)

	go func() { defer wg.Done(); downloaderWorker(ctx, cfg, st) }()
	go func() { defer wg.Done(); ocrWorker(ctx, cfg, st, model) }()
	go func() { defer wg.Done(); parserWorker(ctx, st) }()

	wg.Wait()
	logx.Info("semua worker berhenti.")
}
