// Package logx memisahkan dua hal yang selama ini bercampur dan saling
// merusak: KEMAJUAN yang ditimpa di tempat, dan CATATAN yang harus tersimpan.
//
// Aturannya tegas:
//
//	Konsol  -> HANYA baris kemajuan (ditimpa di tempat memakai \r).
//	Berkas  -> semua catatan lain: info.log dan error.log.
//
// Alasannya: baris kemajuan hidup di satu baris yang terus ditimpa. Begitu ada
// keluaran lain ke konsol, baris itu harus dihapus dulu — sehingga kemajuan
// terus-menerus lenyap dan konsol menjadi campur aduk antara kemajuan dan
// catatan. Memisahkan keduanya membuat konsol selalu menampilkan SATU baris
// kemajuan yang stabil, sementara catatan tetap lengkap dan dapat ditelusuri
// di berkas.
//
// Pengecualian tunggal: Fatal dan Banner tetap tampil di konsol. Kegagalan
// yang menghentikan aplikasi harus terlihat — kalau tidak, aplikasi seolah
// diam tanpa sebab.
package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxLogSz = 5 << 20 // 5 MB, lalu digulung dengan nama bercap waktu

	cReset = "\033[0m"
	cDim   = "\033[2m"
	cCyan  = "\033[36m"
	cRed   = "\033[31m"
)

var (
	mu       sync.Mutex
	errFile  *os.File
	infoFile *os.File
	errPath  string
	infoPath string

	isTTY   bool
	colorOn bool
	inProg  bool
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

// Init membuka kedua berkas catatan.
func Init(dir string) error {
	mu.Lock()
	defer mu.Unlock()
	if strings.TrimSpace(dir) == "" {
		dir = "log"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("menyiapkan folder log: %w", err)
	}

	var err error
	errPath = filepath.Join(dir, "error.log")
	if errFile, err = openLog(errPath); err != nil {
		return err
	}
	infoPath = filepath.Join(dir, "info.log")
	if infoFile, err = openLog(infoPath); err != nil {
		return err
	}
	return nil
}

func openLog(path string) (*os.File, error) {
	rotateIfLarge(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("membuka %s: %w", path, err)
	}
	return f, nil
}

// rotateIfLarge menggulung berkas memakai CAP WAKTU, bukan akhiran ".1" tetap
// yang menimpa catatan sebelumnya — justru catatan yang paling dibutuhkan
// saat menelusuri kegagalan beruntun.
func rotateIfLarge(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < maxLogSz {
		return
	}
	_ = os.Rename(path, fmt.Sprintf("%s.%s", path, time.Now().Format("20060102-150405")))
}

func Close() {
	FinishProgress()
	mu.Lock()
	defer mu.Unlock()
	for _, f := range []**os.File{&errFile, &infoFile} {
		if *f != nil {
			(*f).Close()
			*f = nil
		}
	}
}

func ErrorLogPath() string { return errPath }
func InfoLogPath() string  { return infoPath }

// ---- berkas ----

// write menulis satu baris bercap waktu, lalu MEMAKSA-SIMPAN ke disk.
// Pemaksaan itu perlu karena service dapat berhenti kapan saja; catatan yang
// masih mengambang di penyangga akan hilang persis pada saat paling
// dibutuhkan.
func write(f *os.File, level, msg string) {
	if f == nil {
		return
	}
	line := fmt.Sprintf("%s  %-5s  %s\n", time.Now().Format("2006-01-02 15:04:05"), level, msg)
	_, _ = f.WriteString(line)
	_ = f.Sync()
}

func toInfo(level, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	write(infoFile, level, fmt.Sprintf(format, args...))
}

func toError(level, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	write(errFile, level, msg)
	write(infoFile, level, msg) // info.log memuat runtutan lengkap
}

// ---- konsol ----

func paint(color, s string) string {
	if !colorOn {
		return s
	}
	return color + s + cReset
}

// Progress adalah SATU-SATUNYA keluaran rutin ke konsol.
//
// Format: [sudah diperbaiki / sudah di-OCR / total halaman] keterangan  persen
//
//	[0/1/12] hal 1 · perbaikan: 128 token   8%
//
// Angka pertama memang tertinggal satu langkah dari angka kedua: halaman
// di-OCR dulu, baru diperbaiki. Bila angka pertama berhenti bergerak
// sementara angka kedua terus naik, yang bermasalah tahap perbaikan.
func Progress(fixed, ocred, total int, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()

	detail := fmt.Sprintf(format, args...)
	pct := 0
	if total > 0 {
		pct = fixed * 100 / total
	}
	line := fmt.Sprintf("   %s %s  %s",
		paint(cCyan, fmt.Sprintf("[%d/%d/%d]", fixed, ocred, total)),
		paint(cDim, detail),
		paint(cCyan, fmt.Sprintf("%d%%", pct)))

	if isTTY {
		fmt.Print("\r\033[K" + line)
		inProg = true
		return
	}
	// Bukan terminal (mis. journald): tulis sebagai baris biasa.
	fmt.Println(line)
}

// FinishProgress mengakhiri baris kemajuan dengan baris baru agar tidak
// tertimpa prompt shell saat program berhenti.
func FinishProgress() {
	mu.Lock()
	defer mu.Unlock()
	if inProg && isTTY {
		fmt.Println()
		inProg = false
	}
}

// Banner mencetak keterangan singkat ke konsol. Dipakai HANYA saat mulai
// (mis. memberi tahu lokasi berkas catatan), bukan di dalam perulangan kerja.
func Banner(format string, args ...any) {
	mu.Lock()
	line := fmt.Sprintf(format, args...)
	if inProg && isTTY {
		fmt.Print("\r\033[K")
		inProg = false
	}
	fmt.Println(paint(cDim, line))
	write(infoFile, "INFO", line)
	mu.Unlock()
}

// ---- catatan (berkas saja) ----

// Info, Step, OK, Skip: keterangan biasa. TIDAK tampil di konsol supaya baris
// kemajuan tetap utuh; semuanya tersimpan di info.log.
func Info(format string, args ...any) { toInfo("INFO", format, args...) }
func OK(format string, args ...any)   { toInfo("OK", format, args...) }
func Skip(format string, args ...any) { toInfo("SKIP", format, args...) }

func Step(name, format string, args ...any) {
	toInfo("STEP", name+": "+fmt.Sprintf(format, args...))
}

// Warn: sesuatu yang perlu ditinjau tetapi tidak menghentikan pekerjaan.
func Warn(format string, args ...any) { toError("WARN", format, args...) }

// Fail: satu pekerjaan gagal. Pekerjaan lain tetap berjalan.
func Fail(ctx, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if ctx != "" {
		msg = "[" + ctx + "] " + msg
	}
	toError("FAIL", "%s", msg)
}

// Fatal: kegagalan yang menghentikan aplikasi — SATU-SATUNYA catatan galat
// yang juga tampil di konsol. Tanpa ini aplikasi seolah berhenti tanpa sebab.
func Fatal(ctx, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if ctx != "" {
		msg = "[" + ctx + "] " + msg
	}
	toError("FATAL", "%s", msg)

	mu.Lock()
	defer mu.Unlock()
	if inProg && isTTY {
		fmt.Print("\r\033[K")
		inProg = false
	}
	fmt.Fprintln(os.Stderr, paint(cRed, "✗ "+msg))
}
