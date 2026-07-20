// Command uuparser adalah service yang menjaga agar setiap peraturan baru pada
// endpoint JDIH /integrasi otomatis terunduh, di-OCR, dirapikan, lalu di-parse
// menjadi JSON siap simpan ke database.
//
// Mendukung BANYAK SUMBER sekaligus (jdih.acehprov.go.id, jdih.acehbesarkab.go.id,
// dst). Data tiap sumber dipisah ke foldernya sendiri agar tidak ada nama berkas
// yang bertabrakan dan jumlah berkas per folder tetap wajar:
//
//	data/<sumber>/pdf/<slug>.pdf           (ada berkas = selesai)
//	data/<sumber>/ocr/<slug>/pageN.txt     (OCR, per halaman)
//	data/<sumber>/json/<slug>.json         (ada json = selesai)
//	data/<sumber>/failed/<slug>.json       (catatan gagal + jumlah percobaan)
//	data/<sumber>/integrasi.json           (metadata mentah)
//	data/_last_run.json                    (ringkasan siklus terakhir)
//
// Setelah semua sumber diperiksa, service tidur INTERVAL (baku 60 menit) lalu
// mengulang, sehingga dokumen baru langsung tertangkap.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/fuadarradhi/uuparser/internal/config"
	"github.com/fuadarradhi/uuparser/internal/downloader"
	"github.com/fuadarradhi/uuparser/internal/extractor"
	"github.com/fuadarradhi/uuparser/internal/fsutil"
	"github.com/fuadarradhi/uuparser/internal/localllm"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/parser"
	"github.com/fuadarradhi/uuparser/internal/raster"
	"github.com/fuadarradhi/uuparser/internal/state"
)

// daftar endpoint bawaan; dapat diganti lewat -endpoints.
const defaultEndpoints = "http://jdih.acehprov.go.id/integrasi"

// finalOutput adalah isi data/<sumber>/json/<slug>.json — keluaran siap simpan ke DB.
type finalOutput struct {
	Source          sourceMeta        `json:"source"`
	Report          parser.Report     `json:"report"`
	Relations       []parser.Relation `json:"relations,omitempty"`
	ExtractionNotes []string          `json:"extraction_notes,omitempty"`
	Result          parser.Result     `json:"result"`
}

type sourceMeta struct {
	Sumber       string `json:"sumber"` // kode sumber, mis. "acehprov"
	Endpoint     string `json:"endpoint"`
	IDData       string `json:"id_data"`
	Judul        string `json:"judul"`
	Jenis        string `json:"jenis"`
	Tahun        string `json:"tahun"`
	TeuBadan     string `json:"teu_badan"`
	FileDownload string `json:"file_download"`
	URLDownload  string `json:"url_download"`
	URLDetail    string `json:"url_detail"`
	Slug         string `json:"slug"`
	ParsedAt     string `json:"parsed_at"`
}

// cycleStats ringkasan hasil satu sumber dalam satu siklus.
type cycleStats struct {
	Sumber      string `json:"sumber"`
	Endpoint    string `json:"endpoint"`
	Records     int    `json:"records"`
	Downloaded  int    `json:"downloaded"`
	DownSkipped int    `json:"download_skipped"`
	DownFailed  int    `json:"download_failed"`
	ParseOK     int    `json:"parse_ok"`
	ParseWarn   int    `json:"parse_warning"`
	ParseFail   int    `json:"parse_fail"`
	AlreadyDone int    `json:"already_done"`
	Rejected    int    `json:"rejected_not_legal"`
	NotReady    int    `json:"not_ready"`
	GaveUp      int    `json:"gave_up"` // melewati batas percobaan
	Error       string `json:"error,omitempty"`
}

type runSummary struct {
	StartedAt string       `json:"started_at"`
	Duration  string       `json:"duration"`
	Cycle     int          `json:"cycle"`
	Sources   []cycleStats `json:"sources"`
}

func main() {
	var (
		envPath   = flag.String("env", ".env", "berkas konfigurasi")
		once      = flag.Bool("once", false, "jalankan satu siklus lalu keluar")
		renderPDF = flag.String("render", "", "mode mandiri: render PDF ini menjadi PNG lalu keluar")
		renderOut = flag.String("render-out", "render", "folder keluaran untuk -render")

		// Alat uji sesaat — sengaja TIDAK di .env agar nilai uji coba tidak
		// tertinggal dan diam-diam membatasi service produksi.
		limit  = flag.Int("limit", 0, "batasi N dokumen pertama per sumber (0 = semua)")
		idsCSV = flag.String("ids", "", "hanya proses idData ini, dipisah koma")
	)
	flag.Parse()

	cfg, err := config.Load(*envPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error konfigurasi:", err) // log belum siap di titik ini
		os.Exit(1)
	}
	if err := logx.Init(cfg.LogDir); err != nil {
		fmt.Fprintln(os.Stderr, "error log:", err)
		os.Exit(1)
	}
	defer logx.Close()
	logx.Info("log galat: %s", logx.ErrorLogPath())

	model, err := newModel(cfg)
	if err != nil {
		logx.Fatal("", "menyiapkan model: %v", err)
		os.Exit(1)
	}
	defer model.Release()

	cfg.Limit = *limit
	for _, id := range strings.Split(*idsCSV, ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.OnlyID = append(cfg.OnlyID, id)
		}
	}

	// Mode mandiri: render PDF menjadi PNG lalu keluar.
	if strings.TrimSpace(*renderPDF) != "" {
		if err := renderOnly(*renderPDF, *renderOut, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		logx.Info("\nsinyal berhenti diterima — progres di disk aman")
		cancel()
	}()

	for cycle := 1; ; cycle++ {
		start := time.Now()
		logx.Cycle(cycle, len(cfg.Endpoints))

		var all []cycleStats
		for _, ep := range cfg.Endpoints {
			if ctx.Err() != nil {
				break
			}
			all = append(all, runSource(ctx, cfg, model, ep))
		}
		writeSummary(cfg.DataDir, runSummary{
			StartedAt: start.Format(time.RFC3339),
			Duration:  time.Since(start).Round(time.Second).String(),
			Cycle:     cycle, Sources: all,
		})
		printTotals(all, time.Since(start))

		if ctx.Err() != nil {
			logx.Info("dihentikan.")
			return
		}
		if *once {
			return
		}
		// Tidak ada lagi yang perlu di-OCR sampai siklus berikutnya: lepaskan
		// model agar memori tidak tertahan selama menunggu. Model dimuat ulang
		// sendiri begitu ada halaman baru.
		model.Release()

		logx.Info("menunggu %s sebelum siklus berikutnya...", cfg.Interval)
		select {
		case <-ctx.Done():
			logx.Info("dihentikan.")
			return
		case <-time.After(cfg.Interval):
		}
	}
}

// runSource menjalankan satu putaran penuh untuk satu endpoint sumber.
func runSource(ctx context.Context, cfg config.Config, model *localllm.Client, endpoint string) cycleStats {
	code := downloader.SourceCode(endpoint)
	p := layout(cfg.DataDir, code)
	stats := cycleStats{Sumber: code, Endpoint: endpoint}
	logx.Source(code, endpoint)

	dlCfg := downloader.Config{
		Endpoint: endpoint, PDFDir: p.pdf, MetaPath: p.meta,
		Delay: cfg.DelayMS, MaxRetries: 3, HTTPTimeout: 120 * time.Second,
		Limit: cfg.Limit, OnlyID: cfg.OnlyID,
		FailDir: p.failed, MaxAttempts: cfg.MaxAttempts,
	}

	records, err := downloader.Fetch(ctx, dlCfg)
	if err != nil {
		logx.Warn("endpoint tidak terjangkau (%v) — melanjutkan dengan berkas yang ada", err)
		stats.Error = err.Error()
		records = loadCachedRecords(p.meta)
	}
	stats.Records = len(records)

	selected := downloader.Select(records, dlCfg)
	recBySlug := map[string]downloader.Record{}
	slugs := map[string]bool{}
	for _, r := range selected {
		s := downloader.Slug(r)
		recBySlug[s] = r
		slugs[s] = true
	}
	for _, s := range slugsFromDisk(p.pdf) { // PDF lama yang belum tuntas diproses
		slugs[s] = true
	}
	if len(slugs) == 0 {
		logx.Info("tidak ada dokumen untuk diproses")
		return stats
	}

	exCfg := extractor.Config{
		PDFDir: p.pdf, OCRDir: p.ocr,
		PNGDir: pngDirIf(cfg.SavePNG, p.png),
		DPI:    cfg.DPI, AutoCrop: cfg.AutoCrop,
		ProbePages: cfg.ProbePages, BlankInkRatio: cfg.BlankInk,
		OCRClient: model,
		OCRPrompt: cfg.OCRPrompt, OCRMaxTokens: cfg.OCRMaxTokens,
		FailDir: p.failed, MaxAttempts: cfg.MaxAttempts,
	}

	if cfg.DownloadFirst {
		// Mode lama: seluruh PDF diunduh lebih dulu, baru OCR seluruhnya.
		if len(selected) > 0 {
			d, sk, f, err := downloader.DownloadAll(ctx, dlCfg, selected)
			stats.Downloaded, stats.DownSkipped, stats.DownFailed = d, sk, f
			if err != nil {
				stats.Error = err.Error()
				if ctx.Err() == nil {
					logx.Fatal(code, "tahap unduh dihentikan: %v", err)
				}
				return stats
			}
			logx.Info("unduh: %d baru, %d dilewati, %d gagal", d, sk, f)
		}
		if ctx.Err() != nil {
			return stats
		}
		if err := extractor.Run(ctx, exCfg, slugs); err != nil {
			if ctx.Err() == nil {
				stats.Error = err.Error()
				logx.Fatal(code, "tahap OCR dihentikan: %v", err)
			}
			return stats
		}
		parseAll(ctx, cfg, p, code, endpoint, slugs, recBySlug, &stats)
		return stats
	}

	// Mode baku: satu dokumen diselesaikan penuh (unduh → OCR → perbaiki → parse)
	// sebelum lanjut ke dokumen berikutnya, sehingga JSON pertama muncul cepat
	// tanpa menunggu seluruh unduhan selesai.
	ex := extractor.New(ctx, exCfg)
	if err := os.MkdirAll(p.json, 0o755); err != nil {
		stats.Error = err.Error()
		return stats
	}

	all := sortedKeys(slugs)
	for idx, slug := range all {
		if ctx.Err() != nil {
			return stats
		}
		rec, known := recBySlug[slug]
		logx.Doc(idx+1, len(all), slug)

		// sudah selesai sepenuhnya?
		if fsutil.Exists(filepath.Join(p.json, slug+".json")) {
			stats.AlreadyDone++
			continue
		}
		if extractor.IsRejected(p.ocr, slug) {
			stats.Rejected++
			continue
		}
		if state.ShouldSkip(p.failed, slug, cfg.MaxAttempts) {
			stats.GaveUp++
			continue
		}

		// 1) pastikan PDF ada
		pdfPath := filepath.Join(p.pdf, slug+".pdf")
		if known {
			path, didDownload, err := downloader.Ensure(ctx, dlCfg, rec)
			if err != nil {
				stats.DownFailed++
				logx.Fail(slug, "unduh gagal: %v", err)
				continue
			}
			pdfPath = path
			if didDownload {
				stats.Downloaded++
				logx.Step("unduh", "selesai")
			} else {
				stats.DownSkipped++
			}
		} else if !fsutil.Exists(pdfPath) {
			stats.NotReady++
			continue
		}
		if ctx.Err() != nil {
			return stats
		}

		// 2) OCR
		if err := ex.Document(ctx, slug, pdfPath); err != nil {
			if ctx.Err() != nil {
				return stats
			}
			// Model gagal disiapkan: masalah lingkungan, hentikan sumber ini.
			if localllm.IsLoadError(err) {
				stats.Error = err.Error()
				logx.Fatal(slug, "model tidak dapat disiapkan: %v", err)
				return stats
			}
			if extractor.ReportResult(exCfg, slug, err); err == extractor.ErrRejected {
				stats.Rejected++
			}
			continue
		}
		extractor.ReportResult(exCfg, slug, nil)

		// 3) parse dokumen ini
		parseOne(cfg, p, code, endpoint, slug, recBySlug[slug], &stats)
	}

	logx.Info("%s: %d OK · %d peringatan · %d gagal · %d sudah ada · %d ditolak · %d belum siap · %d menyerah",
		code, stats.ParseOK, stats.ParseWarn, stats.ParseFail, stats.AlreadyDone,
		stats.Rejected, stats.NotReady, stats.GaveUp)
	return stats
}

// parseOne menjalankan parser untuk satu dokumen yang teks-nya sudah siap.
func parseOne(cfg config.Config, p paths, code, endpoint, slug string,
	rec downloader.Record, stats *cycleStats) {

	pages, err := readPages(filepath.Join(p.ocr, slug))
	if err != nil || len(pages) == 0 {
		stats.NotReady++
		return
	}
	rep, res, perr := parser.DiagnoseParse(pages)
	if perr != nil {
		n := state.Record(p.failed, slug, "parse", perr)
		logx.Fail(slug, "parse gagal (percobaan %d): %v", n, perr)
		stats.ParseFail++
		return
	}
	out := finalOutput{
		Source:          toMeta(rec, code, endpoint, slug),
		Report:          rep,
		Relations:       parser.ExtractRelations(res),
		ExtractionNotes: extractionNotes(p.ocr, slug),
		Result:          res,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		stats.ParseFail++
		return
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(p.json, slug+".json"), b, 0o644); err != nil {
		stats.ParseFail++
		return
	}
	state.Clear(p.failed, slug)

	switch rep.Status {
	case parser.StatusSuccess:
		stats.ParseOK++
		logx.OK("parse · pasal=%d ayat=%d relasi=%d",
			rep.Stats.Pasal, rep.Stats.Ayat, len(out.Relations))
	case parser.StatusWarning:
		stats.ParseWarn++
		logx.Warn("parse · %d temuan · relasi=%d", len(rep.Issues), len(out.Relations))
	default:
		stats.ParseFail++
		logx.Fail(slug, "parse menghasilkan status FAIL")
	}
}

func parseAll(ctx context.Context, cfg config.Config, p paths, code, endpoint string,
	slugs map[string]bool, recBySlug map[string]downloader.Record, stats *cycleStats) {

	if err := os.MkdirAll(p.json, 0o755); err != nil {
		stats.Error = err.Error()
		return
	}
	for _, slug := range sortedKeys(slugs) {
		if ctx.Err() != nil {
			return
		}
		jsonPath := filepath.Join(p.json, slug+".json")
		if fsutil.Exists(jsonPath) {
			stats.AlreadyDone++
			continue
		}
		if extractor.IsRejected(p.ocr, slug) {
			stats.Rejected++
			continue
		}
		if state.ShouldSkip(p.failed, slug, cfg.MaxAttempts) {
			stats.GaveUp++
			continue
		}
		pages, err := readPages(filepath.Join(p.ocr, slug))
		if err != nil || len(pages) == 0 {
			stats.NotReady++
			continue
		}

		rep, res, perr := parser.DiagnoseParse(pages)
		if perr != nil {
			// teks ada tapi tidak dikenali sebagai peraturan: catat, jangan ulang selamanya.
			n := state.Record(p.failed, slug, "parse", perr)
			logx.Fail(slug, "parse gagal (percobaan %d): %v", n, perr)
			stats.ParseFail++
			continue
		}

		out := finalOutput{
			Source:          toMeta(recBySlug[slug], code, endpoint, slug),
			Report:          rep,
			Relations:       parser.ExtractRelations(res),
			ExtractionNotes: extractionNotes(p.ocr, slug),
			Result:          res,
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			stats.ParseFail++
			continue
		}
		if err := fsutil.WriteFileAtomic(jsonPath, b, 0o644); err != nil {
			stats.ParseFail++
			continue
		}
		state.Clear(p.failed, slug)

		switch rep.Status {
		case parser.StatusSuccess:
			stats.ParseOK++
			logx.OK("parse · pasal=%d ayat=%d relasi=%d",
				rep.Stats.Pasal, rep.Stats.Ayat, len(out.Relations))
		case parser.StatusWarning:
			stats.ParseWarn++
			logx.Warn("parse · %d temuan · relasi=%d", len(rep.Issues), len(out.Relations))
		default:
			stats.ParseFail++
			logx.Fail(slug, "parse menghasilkan status FAIL")
		}
	}
	logx.Info("parse: %d OK · %d peringatan · %d gagal · %d sudah ada · %d ditolak · %d belum siap · %d menyerah",
		stats.ParseOK, stats.ParseWarn, stats.ParseFail, stats.AlreadyDone,
		stats.Rejected, stats.NotReady, stats.GaveUp)
}

// extractionNotes mengubah catatan per halaman menjadi daftar yang terbaca,
// terurut menurut nomor halaman.
func extractionNotes(ocrDir, slug string) []string {
	raw := extractor.PageNotes(ocrDir, slug)
	if len(raw) == 0 {
		return nil
	}
	pages := make([]int, 0, len(raw))
	for k := range raw {
		n := 0
		fmt.Sscanf(k, "%d", &n)
		pages = append(pages, n)
	}
	sort.Ints(pages)
	notes := make([]string, 0, len(pages))
	for _, p := range pages {
		for _, n := range raw[fmt.Sprint(p)] {
			notes = append(notes, fmt.Sprintf("halaman %d: %s", p, n))
		}
	}
	return notes
}

// ---- tata letak folder per sumber ----

type paths struct{ pdf, ocr, json, failed, png, meta string }

func layout(root, source string) paths {
	base := filepath.Join(root, source)
	return paths{
		pdf:    filepath.Join(base, "pdf"),
		ocr:    filepath.Join(base, "ocr"),
		json:   filepath.Join(base, "json"),
		failed: filepath.Join(base, "failed"),
		png:    filepath.Join(base, "png"),
		meta:   filepath.Join(base, "integrasi.json"),
	}
}

// ---- ringkasan ----

func writeSummary(root string, s runSummary) {
	if b, err := json.MarshalIndent(s, "", "  "); err == nil {
		_ = fsutil.WriteFileAtomic(filepath.Join(root, "_last_run.json"), b, 0o644)
	}
}

func printTotals(all []cycleStats, dur time.Duration) {
	var ok, warn, fail, dl, rej, gave int
	var aborted int
	for _, s := range all {
		if s.Error != "" {
			aborted++
		}
		ok += s.ParseOK
		warn += s.ParseWarn
		fail += s.ParseFail
		dl += s.Downloaded
		rej += s.Rejected
		gave += s.GaveUp
	}
	line := fmt.Sprintf("Siklus selesai dalam %s · %d unduh baru · %d OK · %d peringatan · %d gagal · %d ditolak · %d menyerah",
		dur.Round(time.Second), dl, ok, warn, fail, rej, gave)
	if aborted > 0 {
		line += fmt.Sprintf(" · %d sumber terhenti", aborted)
	}
	logx.Summary(line, fail > 0 || gave > 0 || aborted > 0)
}

// ---- util ----

func loadCachedRecords(metaPath string) []downloader.Record {
	b, err := os.ReadFile(metaPath)
	if err != nil {
		return nil
	}
	var recs []downloader.Record
	if json.Unmarshal(b, &recs) != nil {
		return nil
	}
	return recs
}

func toMeta(r downloader.Record, code, endpoint, slug string) sourceMeta {
	return sourceMeta{
		Sumber: code, Endpoint: endpoint,
		IDData: r.IDData, Judul: r.Judul, Jenis: r.Jenis, Tahun: r.TahunPengundangan,
		TeuBadan: r.TeuBadan, FileDownload: r.FileDownload, URLDownload: r.URLDownload,
		URLDetail: r.URLDetailPeraturan, Slug: slug,
		ParsedAt: time.Now().Format(time.RFC3339),
	}
}

func slugsFromDisk(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		} else if strings.HasSuffix(strings.ToLower(e.Name()), ".pdf") {
			out = append(out, strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
		}
	}
	return out
}

func readPages(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type pf struct {
		n int
		p string
	}
	var files []pf
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if e.IsDir() || !strings.HasPrefix(name, "page") || !strings.HasSuffix(name, ".txt") {
			continue
		}
		num := strings.TrimSuffix(strings.TrimPrefix(name, "page"), ".txt")
		n := 0
		fmt.Sscanf(num, "%d", &n)
		files = append(files, pf{n, filepath.Join(dir, e.Name())})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].n < files[j].n })
	var pages []string
	for _, f := range files {
		b, err := os.ReadFile(f.p)
		if err != nil {
			return nil, err
		}
		pages = append(pages, string(b))
	}
	return pages, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func joinInts(v []int) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = fmt.Sprint(x)
	}
	return strings.Join(parts, ", ")
}

// pngDirIf mengembalikan folder PNG hanya bila penyimpanan diaktifkan.
func pngDirIf(enabled bool, dir string) string {
	if enabled {
		return dir
	}
	return ""
}

// renderOnly merender seluruh halaman satu PDF menjadi PNG memakai pengaturan
// -dpi dan -max-px yang sama dengan pipeline, lalu berhenti. Gambar yang
// dihasilkan identik dengan yang dikirim ke model, sehingga dapat diuji langsung:
//
//	ollama run glm-ocr Text Recognition: ./render/<nama>/page1.png
func renderOnly(pdfPath, outRoot string, cfg config.Config) error {
	doc, err := raster.Open(pdfPath)
	if err != nil {
		return err
	}
	defer doc.Close()

	stem := strings.TrimSuffix(filepath.Base(pdfPath), filepath.Ext(pdfPath))
	outDir := filepath.Join(outRoot, stem)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	n := doc.NumPages()
	fmt.Printf("render %s — %d halaman, DPI=%d AUTO_CROP=%v\n", pdfPath, n, cfg.DPI, cfg.AutoCrop)

	for i := 1; i <= n; i++ {
		start := time.Now()
		pg, err := doc.Render(i, raster.Opts{DPI: cfg.DPI, AutoCrop: cfg.AutoCrop})
		if err != nil {
			return fmt.Errorf("hal %d: %w", i, err)
		}
		dst := filepath.Join(outDir, fmt.Sprintf("page%d.png", i))
		if err := fsutil.WriteFileAtomic(dst, pg.PNG, 0o644); err != nil {
			return err
		}
		blank := ""
		if pg.InkRatio < cfg.BlankInk {
			blank = "  (dinilai kosong — akan dilewati saat OCR)"
		}
		fmt.Printf("  %s — %dx%d px, %.0f KB, tinta %.3f%%, %s%s\n",
			dst, pg.W, pg.H, float64(len(pg.PNG))/1024, pg.InkRatio*100,
			time.Since(start).Round(time.Millisecond), blank)
	}
	sample := filepath.Join(outDir, "page1.png")
	if !filepath.IsAbs(sample) {
		sample = "./" + sample
	}
	fmt.Printf("\nuji satu halaman lewat CLI:\n  ollama run glm-ocr Text Recognition: %s\n", sample)
	return nil
}

// newModel menyiapkan klien model. Model belum dimuat di sini — pemuatan terjadi
// saat halaman pertama benar-benar perlu di-OCR, lalu dipakai untuk seluruh
// dokumen dalam siklus itu.
func newModel(cfg config.Config) (*localllm.Client, error) {
	return localllm.New(localllm.Config{
		ModelPath:  cfg.ModelPath,
		MMProjPath: cfg.MMProjPath,
		LibPath:    cfg.LibPath,
		Verbose:    cfg.Verbose,
	})
}
