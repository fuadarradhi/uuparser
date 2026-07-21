// Package pipeline menjalankan tiga worker independen yang berkoordinasi
// lewat Postgres:
//
//	downloaderWorker — daftarkan tautan baru dari tiap sumber, lalu unduh PDF.
//	                    Tidak mempercayai metadata sumber sama sekali; yang
//	                    diambil hanya tautannya.
//	ocrWorker        — SATU goroutine: OCR halaman-per-halaman, tiap halaman
//	                    langsung diperbaiki model teks; halaman 1 menentukan
//	                    dokumen ini peraturan/duplikat/bukan. Kedua model
//	                    dimuat sekali dan dilepas bersamaan saat antrian habis.
//	parserWorker     — dokumen berstatus 'ocr_done' diurai menjadi nodes.
//
// main.go hanya menyiapkan dependensi lalu memanggil Run.
package pipeline

import (
	"context"
	"sync"
	"time"

	"github.com/fuadarradhi/uuparser/internal/localllm"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/prompts"
	"github.com/fuadarradhi/uuparser/internal/store"
)

// shutdownGrace membatasi lama menunggu worker menyelesaikan pekerjaan yang
// sedang berjalan setelah sinyal berhenti. OCR satu halaman bisa bermenit-
// menit; lewat batas ini Run menyerah secara eksplisit — progres tetap aman
// karena state per-halaman sudah tersimpan di Postgres.
const shutdownGrace = 30 * time.Second

// Deps mengumpulkan seluruh dependensi worker dalam satu tempat, sehingga
// menambah dependensi baru tidak mengubah tanda tangan tiap fungsi worker.
type Deps struct {
	Store   *store.Store
	Vision  *localllm.Client     // model OCR (visi)
	Text    *localllm.TextClient // model teks: klasifikasi + perbaikan
	Prompts prompts.Set
	DataDir string
	DPI     int
}

func Run(ctx context.Context, deps Deps) {
	var wg sync.WaitGroup
	wg.Add(3)

	go func() { defer wg.Done(); downloaderWorker(ctx, deps) }()
	go func() { defer wg.Done(); ocrWorker(ctx, deps) }()
	go func() { defer wg.Done(); parserWorker(ctx, deps) }()

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
