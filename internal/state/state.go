// Package state melacak kegagalan per dokumen agar dokumen rusak tidak dicoba
// ulang selamanya oleh service yang berjalan tiap jam.
//
// Setiap kegagalan menambah penghitung di failed/<slug>.json. Setelah mencapai
// MaxAttempts, dokumen dilewati pada siklus berikutnya. Menghapus berkas itu
// membuat dokumen dicoba lagi dari awal (jalur pemulihan manual).
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/fuadarradhi/uuparser/internal/fsutil"
)

// Failure catatan kegagalan sebuah dokumen.
type Failure struct {
	Slug      string    `json:"slug"`
	Stage     string    `json:"stage"` // download | ocr | fix | parse
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error"`
	FirstAt   time.Time `json:"first_at"`
	LastAt    time.Time `json:"last_at"`
}

func path(failDir, slug string) string { return filepath.Join(failDir, slug+".json") }

// Load membaca catatan kegagalan; nil bila belum ada.
func Load(failDir, slug string) *Failure {
	b, err := os.ReadFile(path(failDir, slug))
	if err != nil {
		return nil
	}
	var f Failure
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	return &f
}

// Attempts mengembalikan jumlah percobaan gagal yang tercatat.
func Attempts(failDir, slug string) int {
	if f := Load(failDir, slug); f != nil {
		return f.Attempts
	}
	return 0
}

// ShouldSkip melaporkan apakah dokumen sudah melewati batas percobaan.
// maxAttempts <= 0 berarti tanpa batas (tidak pernah dilewati).
func ShouldSkip(failDir, slug string, maxAttempts int) bool {
	if maxAttempts <= 0 {
		return false
	}
	return Attempts(failDir, slug) >= maxAttempts
}

// Record menambah penghitung kegagalan dan mengembalikan jumlah percobaan terbaru.
func Record(failDir, slug, stage string, cause error) int {
	f := Load(failDir, slug)
	now := time.Now()
	if f == nil {
		f = &Failure{Slug: slug, FirstAt: now}
	}
	f.Stage = stage
	f.Attempts++
	f.LastAt = now
	if cause != nil {
		f.LastError = cause.Error()
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err == nil {
		_ = fsutil.WriteFileAtomic(path(failDir, slug), b, 0o644)
	}
	return f.Attempts
}

// Clear menghapus catatan kegagalan (dipanggil setelah tahap berhasil).
func Clear(failDir, slug string) {
	_ = os.Remove(path(failDir, slug))
}
