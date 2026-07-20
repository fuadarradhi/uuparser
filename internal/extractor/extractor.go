// Package extractor mengubah PDF menjadi teks per halaman: tiap halaman
// dirasterisasi lalu di-OCR, hasilnya ditulis ke ocr/<slug>/pageN.txt.
//
// Tidak ada tahap "perbaikan typo" oleh model bahasa. Model bahasa tidak dapat
// membedakan salah ketik dari istilah yang tidak dikenalnya — pada korpus hukum
// yang penuh ejaan lama, istilah daerah (Qanun, Reusam, Keuchik), dan nomenklatur
// lama, "perbaikan" semacam itu justru mengubah isi secara diam-diam. Kesalahan
// OCR yang benar-benar merusak struktur ditangani secara deterministik dan dapat
// diaudit di internal/parser/ocrfix.go.
//
// Deteksi "sudah selesai" dilakukan PER HALAMAN (bukan per folder): bila OCR baru
// sampai halaman 4, menjalankan ulang akan meneruskan dari halaman 5.
//
// GATE AWAL: sebelum meng-OCR seluruh halaman, hanya beberapa halaman pertama
// (ProbePages) yang di-OCR lalu diperiksa dengan parser.LooksLegal. Bila tidak
// menunjukkan ciri peraturan, sisa halaman TIDAK di-OCR (hemat untuk PDF tebal yang
// ternyata bukan peraturan), dan ditulis marker _rejected.json.

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fuadarradhi/uuparser/internal/fsutil"
	"github.com/fuadarradhi/uuparser/internal/localllm"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/parser"
	"github.com/fuadarradhi/uuparser/internal/raster"
	"github.com/fuadarradhi/uuparser/internal/state"
)

type Config struct {
	PDFDir string // sumber PDF (data/<sumber>/pdf)
	OCRDir string // keluaran OCR (data/<sumber>/ocr) — dibaca langsung oleh parser

	// PNGDir (opsional) menyimpan gambar halaman yang dikirim ke model OCR,
	// persis seperti yang diterima server. Berguna untuk menguji ulang secara
	// manual. Kosong = tidak disimpan.
	PNGDir string

	AutoCrop   bool // potong margin kosong: hemat piksel tanpa mengecilkan huruf
	DPI        int
	ProbePages int // halaman pertama yang di-OCR untuk gate kelayakan

	// BlankInkRatio: halaman dengan proporsi tinta di bawah nilai ini dianggap
	// kosong dan TIDAK dikirim ke model — menghemat waktu sekaligus menghindari
	// pengulangan degeneratif pada halaman hampa.
	BlankInkRatio float64

	OCRClient    *localllm.Client
	OCRPrompt    string
	OCRMaxTokens int

	FailDir     string // folder catatan kegagalan
	MaxAttempts int    // batas percobaan sebelum dokumen dilewati (0 = tanpa batas)
}

const (
	rejectedName  = "_rejected.json"
	pageNotesName = "_page_notes.json"
)

// rejectedInfo ditulis ke ocr/<slug>/_rejected.json bila dokumen dinilai bukan peraturan.
type rejectedInfo struct {
	Slug       string    `json:"slug"`
	Reason     string    `json:"reason"`
	ProbePages int       `json:"probe_pages"`
	At         time.Time `json:"at"`
	Sample     string    `json:"sample"` // cuplikan teks probe untuk audit manual
}

// DefaultOCRPrompt adalah prompt baku GLM-OCR. Model ini HANYA dilatih untuk
// beberapa prompt tertentu — "Text Recognition:", "Table Recognition:",
// "Figure Recognition:", "Formula Recognition:" — jadi kalimat instruksi bebas
// justru membuat keluarannya tak terduga. Untuk halaman peraturan, "Text
// Recognition:" adalah yang tepat.
const DefaultOCRPrompt = "Text Recognition:"

// Extractor memproses dokumen satu per satu memakai klien inferensi yang sudah
// disiapkan pemanggil.
type Extractor struct {
	cfg Config
}

// New menyiapkan Extractor dan memeriksa ketersediaan server (best-effort).
func New(_ context.Context, c Config) *Extractor {
	return &Extractor{cfg: c}
}

// ErrRejected dikembalikan bila dokumen ditolak gate kelayakan (bukan peraturan).
var ErrRejected = errRejected

// Document memproses SATU dokumen: OCR (dengan gate probe) lalu perbaikan teks.
// Idempoten per halaman; aman dipanggil berulang.
func (e *Extractor) Document(ctx context.Context, slug, pdfPath string) error {
	return e.extractOne(ctx, slug, pdfPath)
}

// Run memproses semua PDF di PDFDir (atau subset). Dipakai pada mode
// -download-first; mode biasa memanggil Document per dokumen.
func Run(ctx context.Context, c Config, slugsFilter map[string]bool) error {
	e := New(ctx, c)
	pdfs, err := ListPDFs(c.PDFDir)
	if err != nil {
		return err
	}
	for _, pdfPath := range pdfs {
		if err := ctx.Err(); err != nil {
			return err
		}
		slug := strings.TrimSuffix(filepath.Base(pdfPath), ".pdf")
		if len(slugsFilter) > 0 && !slugsFilter[slug] {
			continue
		}
		if c.FailDir != "" && state.ShouldSkip(c.FailDir, slug, c.MaxAttempts) {
			continue
		}
		err := e.Document(ctx, slug, pdfPath)
		// Kegagalan menyiapkan model bersifat lingkungan, bukan milik dokumen ini:
		// hentikan siklus daripada menghabiskan jatah percobaan seluruh dokumen.
		if localllm.IsLoadError(err) {
			return err
		}
		ReportResult(c, slug, err)
	}
	return nil
}

// ReportResult mencetak hasil satu dokumen dan mencatat kegagalannya.
// Mengembalikan true bila tahap ekstraksi berhasil.
func ReportResult(c Config, slug string, err error) bool {
	switch {
	case localllm.IsLoadError(err):
		// tidak dicatat sebagai kegagalan dokumen; pemanggil menghentikan siklus.
		logx.Fatal(slug, "model tidak dapat disiapkan: %v", err)
		return false
	case err == nil:
		if c.FailDir != "" {
			state.Clear(c.FailDir, slug)
		}
		logx.OK("OCR selesai")
		return true
	case err == errRejected:
		logx.Skip("bukan peraturan — ditolak di gate awal")
		return false
	default:
		n := 0
		if c.FailDir != "" {
			n = state.Record(c.FailDir, slug, "extract", err)
		}
		logx.Fail(slug, "OCR gagal (percobaan %d): %v", n, err)
		return false
	}
}

// errRejected menandai dokumen ditolak gate (bukan error kegagalan teknis).
var errRejected = fmt.Errorf("dokumen ditolak gate kelayakan")

func (e *Extractor) extractOne(ctx context.Context, slug, pdfPath string) error {
	c := e.cfg
	ocrDir := filepath.Join(c.OCRDir, slug)

	// sudah pernah ditolak? langsung skip (tanpa buka PDF).
	if fileExists(filepath.Join(ocrDir, rejectedName)) {
		return errRejected
	}

	probe := c.ProbePages
	if probe <= 0 {
		probe = 5
	}
	dpi := c.DPI
	if dpi == 0 {
		dpi = 200
	}

	doc, err := raster.Open(pdfPath)
	if err != nil {
		return err
	}
	defer doc.Close()
	n := doc.NumPages()
	if n == 0 {
		return fmt.Errorf("pdf tanpa halaman")
	}
	if err := os.MkdirAll(ocrDir, 0o755); err != nil {
		return err
	}

	// ---- OCR halaman probe (idempoten per halaman) ----
	probeN := probe
	if probeN > n {
		probeN = n
	}
	if err := e.ocrRange(ctx, doc, slug, ocrDir, 0, probeN, n, dpi); err != nil {
		return fmt.Errorf("ocr probe: %w", err)
	}

	// ---- GATE: apakah teks probe menunjukkan ciri peraturan? ----
	probeText, err := readPageRange(ocrDir, 1, probeN)
	if err != nil {
		return err
	}
	if !parser.LooksLegalProbe(probeText) {
		// tolak: jangan OCR sisa halaman. Tulis marker untuk audit & skip berikutnya.
		writeRejected(ocrDir, slug, probe, probeText)
		return errRejected
	}

	// ---- OCR sisa halaman (idempoten per halaman) ----
	if probeN < n {
		if err := e.ocrRange(ctx, doc, slug, ocrDir, probeN, n, n, dpi); err != nil {
			return fmt.Errorf("ocr lanjut: %w", err)
		}
	}

	return nil
}

// ocrRange meng-OCR halaman indeks [start,end). Melewati halaman yang txt-nya sudah ada.
func (e *Extractor) ocrRange(ctx context.Context, doc *raster.Doc, slug, ocrDir string, start, end, total, dpi int) error {
	c := e.cfg
	for i := start; i < end; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		out := filepath.Join(ocrDir, fmt.Sprintf("page%d.txt", i+1))
		if fileExists(out) {
			continue
		}
		pg, err := doc.Render(i+1, raster.Opts{DPI: dpi, AutoCrop: c.AutoCrop}) // halaman 1-based
		if err != nil {
			return fmt.Errorf("rasterisasi hal %d: %w", i+1, err)
		}

		// Halaman kosong: jangan panggil model sama sekali. Selain membuang waktu,
		// halaman hampa adalah pemicu utama pengulangan tanpa henti.
		if pg.InkRatio < blankRatio(c) {
			if err := fsutil.WriteFileAtomic(out, []byte(""), 0o644); err != nil {
				return err
			}
			notePage(ocrDir, i+1, "halaman kosong (tanpa teks) — OCR dilewati")
			logx.Skip("hal %d/%d — kosong, dilewati", i+1, total)
			continue
		}
		// simpan gambar yang benar-benar dikirim ke model, bila diminta.
		if c.PNGDir != "" {
			imgPath := filepath.Join(c.PNGDir, slug, fmt.Sprintf("page%d.png", i+1))
			if !fileExists(imgPath) {
				if err := fsutil.WriteFileAtomic(imgPath, pg.PNG, 0o644); err != nil {
					return fmt.Errorf("simpan png hal %d: %w", i+1, err)
				}
			}
		}
		started := time.Now()
		res, err := c.OCRClient.Vision(ctx, ocrPromptOf(c), pg.PNG,
			localllm.Params{MaxTokens: c.OCRMaxTokens})
		if err != nil {
			return fmt.Errorf("OCR hal %d: %w", i+1, err)
		}
		text := res.Text

		// Respons done=false berarti model dihentikan sebelum selesai: batas
		// num_predict tercapai, atau Ollama membatalkan karena pengulangan
		// (ollama/ollama#16892, #14117). Teks halaman kemungkinan terpotong.
		if res.Truncated {
			notePage(ocrDir, i+1,
				"keluaran OCR terpotong (batas token atau pembatalan Ollama) — teks halaman mungkin tidak lengkap")
			logx.Warn("hal %d/%d — keluaran terpotong", i+1, total)
		}
		saved := ""
		if pg.CroppedFrom > 0 {
			// pakai pecahan agar pemotongan besar tidak dibulatkan menjadi 100%
			cut := 100 - float64(pg.W*pg.H)*100/float64(pg.CroppedFrom)
			if cut >= 1 {
				saved = fmt.Sprintf(" (-%.1f%% piksel)", cut)
			}
		}
		logx.Progress("OCR", i+1, total, "%dx%d px%s · %s",
			pg.W, pg.H, saved, time.Since(started).Round(time.Second))
		// Hasil kosong hampir selalu berarti model gagal, bukan halaman benar-benar
		// kosong. Coba sekali lagi sebelum menerimanya.
		// Pangkas pengulangan degeneratif sebelum disimpan.
		if res := cleanOCRText(text); res.Removed > 0 || res.Degenerate {
			text = res.Text
			if res.Degenerate {
				notePage(ocrDir, i+1,
					"keluaran OCR didominasi pengulangan — kemungkinan model macet; perlu ditinjau")
				logx.Warn("hal %d/%d — keluaran berulang, dipangkas", i+1, total)
			}
		}

		if strings.TrimSpace(text) == "" {
			retry, rerr := c.OCRClient.Vision(ctx, ocrPromptOf(c), pg.PNG,
				localllm.Params{MaxTokens: c.OCRMaxTokens})
			if rerr != nil {
				return fmt.Errorf("OCR hal %d (ulang): %w", i+1, rerr)
			}
			text = retry.Text
			// Pangkas pengulangan degeneratif sebelum disimpan.
			if res := cleanOCRText(text); res.Removed > 0 || res.Degenerate {
				text = res.Text
				if res.Degenerate {
					notePage(ocrDir, i+1,
						"keluaran OCR didominasi pengulangan — kemungkinan model macet; perlu ditinjau")
					logx.Warn("hal %d/%d — keluaran berulang, dipangkas", i+1, total)
				}
			}

			if strings.TrimSpace(text) == "" {
				// Tetap tulis agar proses tidak mandek, tapi catat untuk ditinjau.
				notePage(ocrDir, i+1, "OCR menghasilkan teks kosong")
			}
		}
		if err := fsutil.WriteFileAtomic(out, []byte(text), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func writeRejected(ocrDir, slug string, probePages int, probeText []string) {
	sample := strings.Join(probeText, "\n")
	if len(sample) > 1500 {
		sample = sample[:1500]
	}
	info := rejectedInfo{
		Slug: slug, Reason: "tidak menunjukkan ciri dokumen perundang-undangan pada halaman probe",
		ProbePages: probePages, At: time.Now(), Sample: sample,
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	_ = fsutil.WriteFileAtomic(filepath.Join(ocrDir, rejectedName), b, 0o644)
}

// IsRejected melaporkan apakah slug sudah ditandai bukan-peraturan (dipakai orchestrator).
func IsRejected(ocrDir, slug string) bool {
	return fileExists(filepath.Join(ocrDir, slug, rejectedName))
}

// ---- util ----

func ListPDFs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".pdf") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

func listPages(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if !e.IsDir() && strings.HasPrefix(name, "page") && strings.HasSuffix(name, ".txt") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Slice(out, func(i, j int) bool { return pageNum(out[i]) < pageNum(out[j]) })
	return out, nil
}

// readPageRange membaca ocr/<>/pageA.txt .. pageB.txt (inklusif, 1-based) yang ada.
func readPageRange(dir string, a, b int) ([]string, error) {
	var out []string
	for i := a; i <= b; i++ {
		p := filepath.Join(dir, fmt.Sprintf("page%d.txt", i))
		data, err := os.ReadFile(p)
		if err != nil {
			continue // halaman mungkin belum ada; abaikan
		}
		out = append(out, string(data))
	}
	return out, nil
}

func pageNum(path string) int {
	base := strings.ToLower(filepath.Base(path))
	base = strings.TrimSuffix(strings.TrimPrefix(base, "page"), ".txt")
	n := 0
	fmt.Sscanf(base, "%d", &n)
	return n
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// notePage mencatat catatan mengenai satu halaman ke ocr/<slug>/_page_notes.json
// agar terlihat saat peninjauan dan muncul di JSON akhir.
func notePage(ocrDir string, page int, note string) {
	path := filepath.Join(ocrDir, pageNotesName)
	notes := map[string][]string{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &notes)
	}
	key := fmt.Sprintf("%d", page)
	for _, existing := range notes[key] {
		if existing == note {
			return // sudah tercatat
		}
	}
	notes[key] = append(notes[key], note) // satu halaman bisa punya lebih dari satu masalah
	if b, err := json.MarshalIndent(notes, "", "  "); err == nil {
		_ = fsutil.WriteFileAtomic(path, b, 0o644)
	}
}

// PageNotes mengembalikan catatan per halaman untuk sebuah slug (nomor halaman -> catatan).
func PageNotes(ocrDir, slug string) map[string][]string {
	b, err := os.ReadFile(filepath.Join(ocrDir, slug, pageNotesName))
	if err != nil {
		return nil
	}
	notes := map[string][]string{}
	if json.Unmarshal(b, &notes) != nil {
		return nil
	}
	return notes
}

// ocrPromptOf mengembalikan prompt OCR yang berlaku.
func ocrPromptOf(c Config) string {
	if strings.TrimSpace(c.OCRPrompt) != "" {
		return c.OCRPrompt
	}
	return DefaultOCRPrompt
}

func blankRatio(c Config) float64 {
	if c.BlankInkRatio > 0 {
		return c.BlankInkRatio
	}
	return 0.0004 // 0,04% piksel gelap
}
