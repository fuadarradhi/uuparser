// Package logx menangani dua keluaran yang berbeda kebutuhannya.
//
// Konsol: ringkas, berwarna, dan menunjukkan kemajuan — untuk dipantau manusia.
// Warna dimatikan sendiri ketika keluaran bukan terminal (mis. ditangkap
// journald oleh systemd), sehingga berkas log tidak dipenuhi kode escape.
//
// Berkas galat: setiap kegagalan dicatat permanen ke log/error.log beserta
// waktu dan konteksnya. Ini penting bagi service systemd — keluaran konsol dapat
// hilang tergulung, sementara kegagalan pemrosesan dokumen perlu dapat ditelusuri
// kemudian.
package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// kode warna ANSI
const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cDim    = "\033[90m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cBlue   = "\033[34m"
	cCyan   = "\033[36m"
)

var (
	mu       sync.Mutex
	colorOn  bool
	errFile  *os.File
	errPath  string
	inProg   bool // sedang menampilkan baris kemajuan yang ditimpa
	isTTY    bool
	maxLogSz int64 = 5 << 20 // 5 MB sebelum digulung
)

func init() {
	isTTY = detectTTY()
	colorOn = isTTY && os.Getenv("NO_COLOR") == ""
}

func detectTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// Init menyiapkan berkas log galat di dalam dir. Aman dipanggil sekali di awal.
func Init(dir string) error {
	mu.Lock()
	defer mu.Unlock()
	if strings.TrimSpace(dir) == "" {
		dir = "log"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("menyiapkan folder log: %w", err)
	}
	errPath = filepath.Join(dir, "error.log")
	rotateIfLarge()
	f, err := os.OpenFile(errPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("membuka %s: %w", errPath, err)
	}
	errFile = f
	return nil
}

// Close menutup berkas log galat.
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if errFile != nil {
		errFile.Close()
		errFile = nil
	}
}

// ErrorLogPath mengembalikan lokasi berkas log galat.
func ErrorLogPath() string { return errPath }

// rotateIfLarge menggulung berkas log bila sudah besar, agar tidak tumbuh tanpa batas.
func rotateIfLarge() {
	fi, err := os.Stat(errPath)
	if err != nil || fi.Size() < maxLogSz {
		return
	}
	_ = os.Rename(errPath, errPath+".1")
}

func paint(color, s string) string {
	if !colorOn {
		return s
	}
	return color + s + cReset
}

// clearProgress menghapus baris kemajuan yang sedang ditimpa, agar baris
// berikutnya tidak menempel pada sisa teks sebelumnya.
func clearProgress() {
	if inProg && isTTY {
		fmt.Print("\r\033[K")
		inProg = false
	}
}

func out(s string) {
	clearProgress()
	fmt.Println(s)
}

// ---- keluaran tingkat tinggi ----

// Cycle menandai awal satu siklus.
func Cycle(n int, sources int) {
	mu.Lock()
	defer mu.Unlock()
	out("")
	out(paint(cBold+cCyan, fmt.Sprintf("═══ Siklus #%d — %s — %d sumber",
		n, time.Now().Format("2006-01-02 15:04:05"), sources)))
}

// Source menandai awal pemrosesan satu sumber JDIH.
func Source(code, endpoint string) {
	mu.Lock()
	defer mu.Unlock()
	out("")
	out(paint(cBold, "▸ "+code) + paint(cDim, "  "+endpoint))
}

// Doc menandai awal pemrosesan satu dokumen: [12/128] nama-dokumen
func Doc(i, total int, slug string) {
	mu.Lock()
	defer mu.Unlock()
	counter := fmt.Sprintf("[%d/%d]", i, total)
	out(paint(cBlue, counter) + " " + slug)
}

// Step mencetak satu langkah yang sedang berjalan, mis. "unduh" atau "parse".
func Step(name, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	out("   " + paint(cDim, pad(name)) + fmt.Sprintf(format, args...))
}

// Progress mencetak kemajuan yang menimpa dirinya sendiri di terminal, dan
// menjadi baris biasa bila keluaran bukan terminal (mis. journald).
func Progress(name string, cur, total int, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	detail := fmt.Sprintf(format, args...)
	pct := 0
	if total > 0 {
		pct = cur * 100 / total
	}
	line := fmt.Sprintf("   %s%s %s",
		paint(cDim, pad(name)),
		paint(cCyan, fmt.Sprintf("%d/%d (%d%%)", cur, total, pct)),
		paint(cDim, detail))
	if isTTY {
		fmt.Print("\r\033[K" + line)
		inProg = true
		return
	}
	fmt.Println(line)
}

// OK mencetak keberhasilan.
func OK(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	out("   " + paint(cGreen, "✓ ") + fmt.Sprintf(format, args...))
}

// Warn mencetak peringatan: pemrosesan berhasil tetapi ada yang perlu ditinjau.
func Warn(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	out("   " + paint(cYellow, "! ") + fmt.Sprintf(format, args...))
}

// Skip mencetak langkah yang dilewati.
func Skip(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	out("   " + paint(cDim, "· "+fmt.Sprintf(format, args...)))
}

// Info mencetak keterangan biasa.
func Info(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	out(paint(cDim, fmt.Sprintf(format, args...)))
}

// Fail mencetak kegagalan ke konsol DAN mencatatnya ke berkas log galat.
// Argumen ctx berisi konteks singkat (mis. sumber dan nama dokumen) agar catatan
// di berkas dapat dipahami tanpa membaca keluaran konsol.
func Fail(ctx, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	mu.Lock()
	defer mu.Unlock()
	out("   " + paint(cRed, "✗ ") + msg)
	writeErrLog(ctx, msg)
}

// Fatal mencetak galat yang menghentikan proses dan mencatatnya.
func Fatal(ctx, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	mu.Lock()
	defer mu.Unlock()
	out(paint(cRed+cBold, "GAGAL: ") + msg)
	writeErrLog(ctx, msg)
}

// Summary mencetak ringkasan akhir siklus, warnanya mengikuti ada tidaknya kegagalan.
func Summary(line string, hasFail bool) {
	mu.Lock()
	defer mu.Unlock()
	out("")
	if hasFail {
		out(paint(cYellow, "── "+line))
		return
	}
	out(paint(cGreen, "── "+line))
}

// writeErrLog menulis satu baris ke berkas log galat. Pemanggil sudah memegang kunci.
func writeErrLog(ctx, msg string) {
	if errFile == nil {
		return
	}
	stamp := time.Now().Format("2006-01-02 15:04:05")
	if strings.TrimSpace(ctx) != "" {
		fmt.Fprintf(errFile, "%s  [%s] %s\n", stamp, ctx, msg)
	} else {
		fmt.Fprintf(errFile, "%s  %s\n", stamp, msg)
	}
	_ = errFile.Sync() // service bisa mati sewaktu-waktu; jangan tunda ke buffer
}

// pad menyeragamkan lebar nama langkah agar kolom keluaran sejajar.
func pad(s string) string {
	const w = 9
	if len(s) >= w {
		return s + " "
	}
	return s + strings.Repeat(" ", w-len(s))
}
