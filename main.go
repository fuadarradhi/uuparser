// Command uuparser adalah service yang menjaga agar setiap peraturan baru pada
// sumber JDIH terdaftar otomatis terunduh, di-OCR, lalu di-parse menjadi baris
// siap ditinjau/diedit di database Postgres.
//
// Tidak ada flag CLI: konfigurasi hanya lewat .env (DATABASE_URL, MODEL_PATH,
// MMPROJ_PATH, LIB_PATH, LOG_DIR, DATA_DIR, VERBOSE — lihat internal/config).
// Aksi admin sesaat (reset/approve/reject dokumen) dilakukan lewat SQL
// langsung — lihat CATATAN-MIGRASI.md bagian "Perubahan status manual".
//
// Arsitektur tiga-worker (downloader/OCR/parser independen, berkoordinasi
// lewat Postgres) ada di internal/pipeline; main.go hanya menyiapkan
// dependency (config, log, DB, model) lalu memanggil pipeline.Run.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/fuadarradhi/uuparser/internal/config"
	"github.com/fuadarradhi/uuparser/internal/localllm"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/pipeline"
	"github.com/fuadarradhi/uuparser/internal/store"
)

func main() {
	cfg, err := config.Load(".env")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error konfigurasi:", err)
		os.Exit(1)
	}
	if err := logx.Init(cfg.LogDir); err != nil {
		fmt.Fprintln(os.Stderr, "error log:", err)
		os.Exit(1)
	}
	defer logx.Close()
	logx.Info("log galat: %s", logx.ErrorLogPath())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		logx.Info("sinyal berhenti diterima — progres di database aman")
		cancel()
	}()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logx.Fatal("", "membuka database: %v", err)
		os.Exit(1)
	}
	defer st.Close()

	model, err := localllm.New(localllm.Config{
		ModelPath: cfg.ModelPath, MMProjPath: cfg.MMProjPath,
		LibPath: cfg.LibPath, Verbose: cfg.Verbose,
	})
	if err != nil {
		logx.Fatal("", "menyiapkan model: %v", err)
		os.Exit(1)
	}
	defer model.Release()

	pipeline.Run(ctx, cfg, st, model)
}
