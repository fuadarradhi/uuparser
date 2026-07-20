// Package downloader menangani tahap 1: menarik metadata dari endpoint JDIH
// /integrasi dan mengunduh tiap PDF ke folder pdf/. "Sudah selesai" ditentukan
// semata dari keberadaan file (jika pdf/<slug>.pdf ada, tidak diunduh ulang).
package downloader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fuadarradhi/uuparser/internal/fsutil"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/state"
)

// Record adalah satu entri metadata dari /integrasi (field mengikuti endpoint).
type Record struct {
	IDData             string `json:"idData"`
	Judul              string `json:"judul"`
	Jenis              string `json:"jenis"`
	NoPeraturan        string `json:"noPeraturan"`
	TeuBadan           string `json:"teuBadan"`
	TahunPengundangan  string `json:"tahun_pengundangan"`
	Status             string `json:"status"`
	Bahasa             string `json:"bahasa"`
	FileDownload       string `json:"fileDownload"`
	URLDownload        string `json:"urlDownload"`
	URLDetailPeraturan string `json:"urlDetailPeraturan"`
}

// Config untuk downloader.
type Config struct {
	Endpoint    string
	PDFDir      string // folder tujuan PDF (mis. data/pdf)
	MetaPath    string // tempat menyimpan integrasi.json mentah (mis. data/integrasi.json)
	Delay       time.Duration
	MaxRetries  int
	HTTPTimeout time.Duration

	Limit  int      // 0 = semua; >0 = N record pertama (mode uji)
	OnlyID []string // bila diisi, hanya idData ini

	FailDir     string // folder catatan kegagalan (mis. data/<sumber>/failed)
	MaxAttempts int    // batas percobaan sebelum dokumen dilewati (0 = tanpa batas)
}

var (
	reUnsafe   = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)
	reMultiUnd = regexp.MustCompile(`_{2,}`)
	reEdge     = regexp.MustCompile(`(^[._-]+)|([._-]+$)`)
	pdfMagic   = []byte("%PDF")
	reHrefPDF  = regexp.MustCompile(`(?i)href="([^"]+\.pdf[^"]*)"`)
)

// Slug mengembalikan identitas stabil & aman untuk sebuah record: stem fileDownload.
// Dipakai konsisten oleh semua tahap (nama folder ocr/json).
func Slug(r Record) string {
	name := strings.TrimSpace(r.FileDownload)
	if name == "" {
		return "doc_" + sanitize(r.IDData)
	}
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	return sanitize(stem)
}

// SourceCode menurunkan kode sumber ringkas dari URL endpoint, dipakai sebagai
// nama folder agar data tiap situs JDIH terpisah:
//
//	http://jdih.acehprov.go.id/integrasi        -> acehprov
//	http://jdih.acehbesarkab.go.id/integrasi    -> acehbesarkab
//	http://jdih.bandaacehkota.go.id/integrasi   -> bandaacehkota
func SourceCode(endpoint string) string {
	host := endpoint
	port := ""
	if u, err := url.Parse(endpoint); err == nil && u.Hostname() != "" {
		host = u.Hostname()
		// sertakan port non-standar agar dua endpoint pada host sama tidak
		// berbagi folder yang sama.
		if p := u.Port(); p != "" && p != "80" && p != "443" {
			port = "_" + p
		}
	}
	host = strings.ToLower(host)
	host = strings.TrimPrefix(host, "www.")
	host = strings.TrimPrefix(host, "jdih.")
	for _, suf := range []string{".go.id", ".ac.id", ".co.id", ".or.id", ".id", ".com", ".net", ".org"} {
		if strings.HasSuffix(host, suf) {
			host = strings.TrimSuffix(host, suf)
			break
		}
	}
	host = strings.ReplaceAll(host, ".", "_")
	if strings.TrimSpace(host) == "" {
		host = "sumber"
	}
	return sanitize(host + port)
}

func sanitize(s string) string {
	s = strings.TrimSpace(s)
	s = reUnsafe.ReplaceAllString(s, "_")
	s = reMultiUnd.ReplaceAllString(s, "_")
	s = reEdge.ReplaceAllString(s, "")
	if len(s) > 120 {
		s = reEdge.ReplaceAllString(s[:120], "")
	}
	if s == "" {
		s = "dokumen"
	}
	return s
}

// PDFPath mengembalikan path PDF untuk sebuah record.
func (c Config) PDFPath(r Record) string { return filepath.Join(c.PDFDir, Slug(r)+".pdf") }

// Fetch menarik dan mengurai daftar record dari endpoint, sekaligus menyimpan mentahnya.
func Fetch(ctx context.Context, c Config) ([]Record, error) {
	body, _, err := httpGet(ctx, c, c.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("fetch integrasi: %w", err)
	}
	var records []Record
	if err := json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("parse integrasi (%d byte): %w", len(body), err)
	}
	if c.MetaPath != "" {
		_ = os.MkdirAll(filepath.Dir(c.MetaPath), 0o755)
		_ = fsutil.WriteFileAtomic(c.MetaPath, body, 0o644)
	}
	return records, nil
}

// Select memfilter record sesuai Limit / OnlyID (terurut idData numerik).
func Select(records []Record, c Config) []Record {
	sortByID(records)
	only := map[string]bool{}
	for _, id := range c.OnlyID {
		only[id] = true
	}
	var out []Record
	for _, r := range records {
		if len(only) > 0 && !only[r.IDData] {
			continue
		}
		out = append(out, r)
		if c.Limit > 0 && len(only) == 0 && len(out) >= c.Limit {
			break
		}
	}
	return out
}

// DownloadAll mengunduh PDF untuk record terpilih; melewati yang filenya sudah ada.
// Mengembalikan jumlah terunduh & jumlah dilewati.
func DownloadAll(ctx context.Context, c Config, records []Record) (downloaded, skipped, failed int, err error) {
	if err := os.MkdirAll(c.PDFDir, 0o755); err != nil {
		return 0, 0, 0, err
	}
	for _, r := range records {
		if err := ctx.Err(); err != nil {
			return downloaded, skipped, failed, err
		}
		slug := Slug(r)
		dest := c.PDFPath(r)
		if isValidPDF(dest) {
			skipped++
			continue
		}
		// dokumen yang berulang kali gagal tidak dicoba lagi tiap siklus.
		if c.FailDir != "" && state.ShouldSkip(c.FailDir, slug, c.MaxAttempts) {
			skipped++
			continue
		}
		if e := downloadOne(ctx, c, r, dest); e != nil {
			failed++
			n := 0
			if c.FailDir != "" {
				n = state.Record(c.FailDir, slug, "download", e)
			}
			logx.Fail(slug, "unduh gagal (percobaan %d): %v", n, e)
			continue
		}
		if c.FailDir != "" {
			state.Clear(c.FailDir, slug)
		}
		downloaded++
		logx.Step("unduh", "%s", slug)
		if c.Delay > 0 {
			time.Sleep(c.Delay)
		}
	}
	return downloaded, skipped, failed, nil
}

func downloadOne(ctx context.Context, c Config, r Record, dest string) error {
	url := strings.TrimSpace(r.URLDownload)
	if url == "" {
		return fmt.Errorf("urlDownload kosong")
	}
	body, ctype, err := httpGet(ctx, c, url)
	if err != nil {
		return err
	}
	if isPDF(ctype, body) {
		return fsutil.WriteFileAtomic(dest, body, 0o644)
	}
	// fallback: server mengembalikan HTML viewer -> cari tautan .pdf.
	if looksHTML(ctype, body) {
		if link := findPDFLink(body, url); link != "" {
			body2, ctype2, err := httpGet(ctx, c, link)
			if err != nil {
				return fmt.Errorf("mengikuti tautan PDF: %w", err)
			}
			if isPDF(ctype2, body2) {
				return fsutil.WriteFileAtomic(dest, body2, 0o644)
			}
			return fmt.Errorf("tautan %s bukan PDF (ctype=%s)", link, ctype2)
		}
		return fmt.Errorf("HTML tanpa tautan .pdf (ctype=%s)", ctype)
	}
	if bytes.HasPrefix(bytes.TrimSpace(body), pdfMagic) {
		return fsutil.WriteFileAtomic(dest, body, 0o644)
	}
	return fmt.Errorf("bukan PDF (ctype=%s, %d byte)", ctype, len(body))
}

// ---- http & deteksi konten ----

func httpGet(ctx context.Context, c Config, url string) ([]byte, string, error) {
	timeout := c.HTTPTimeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	cl := &http.Client{Timeout: timeout}
	retries := c.MaxRetries
	if retries < 0 {
		retries = 0
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(c.Delay * time.Duration(attempt)):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("User-Agent", "uuparser-downloader/0.1")
		resp, err := cl.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode >= 400 {
			return nil, "", fmt.Errorf("status %d", resp.StatusCode)
		}
		return body, resp.Header.Get("Content-Type"), nil
	}
	return nil, "", fmt.Errorf("gagal setelah %d percobaan: %w", retries+1, lastErr)
}

func isPDF(ctype string, body []byte) bool {
	if strings.Contains(strings.ToLower(ctype), "application/pdf") {
		return true
	}
	return bytes.HasPrefix(bytes.TrimSpace(body), pdfMagic)
}

func looksHTML(ctype string, body []byte) bool {
	if strings.Contains(strings.ToLower(ctype), "text/html") {
		return true
	}
	h := bytes.ToLower(bytes.TrimSpace(body))
	return bytes.HasPrefix(h, []byte("<!doctype html")) || bytes.HasPrefix(h, []byte("<html"))
}

func findPDFLink(html []byte, base string) string {
	m := reHrefPDF.FindSubmatch(html)
	if m == nil {
		return ""
	}
	link := string(m[1])
	if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") {
		return link
	}
	i := strings.Index(base, "://")
	if i < 0 {
		return link
	}
	rest := base[i+3:]
	host := rest
	if j := strings.Index(rest, "/"); j >= 0 {
		host = rest[:j]
	}
	origin := base[:i+3] + host
	if strings.HasPrefix(link, "/") {
		return origin + link
	}
	return origin + "/" + link
}

func isValidPDF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 8)
	n, _ := f.Read(buf)
	return n >= 4 && bytes.HasPrefix(bytes.TrimSpace(buf[:n]), pdfMagic)
}

func sortByID(records []Record) {
	sortSlice(records, func(a, b Record) bool {
		ai, ae := strconv.Atoi(a.IDData)
		bi, be := strconv.Atoi(b.IDData)
		if ae == nil && be == nil {
			return ai < bi
		}
		return a.IDData < b.IDData
	})
}

// sortSlice adalah sort.Slice sederhana tanpa import tambahan di call-site.
func sortSlice(records []Record, less func(a, b Record) bool) {
	for i := 1; i < len(records); i++ {
		for j := i; j > 0 && less(records[j], records[j-1]); j-- {
			records[j], records[j-1] = records[j-1], records[j]
		}
	}
}

// Ensure memastikan PDF untuk satu record tersedia di disk, mengunduhnya bila
// belum ada. Dipakai pada alur per-dokumen (unduh → OCR → parse satu per satu),
// sehingga hasil pertama muncul tanpa menunggu seluruh unduhan selesai.
//
// Mengembalikan path PDF dan apakah unduhan baru saja dilakukan.
func Ensure(ctx context.Context, c Config, r Record) (path string, downloaded bool, err error) {
	slug := Slug(r)
	dest := c.PDFPath(r)
	if isValidPDF(dest) {
		return dest, false, nil
	}
	if c.FailDir != "" && state.ShouldSkip(c.FailDir, slug, c.MaxAttempts) {
		return "", false, fmt.Errorf("dilewati: sudah melewati batas percobaan")
	}
	if err := os.MkdirAll(c.PDFDir, 0o755); err != nil {
		return "", false, err
	}
	if err := downloadOne(ctx, c, r, dest); err != nil {
		if c.FailDir != "" {
			n := state.Record(c.FailDir, slug, "download", err)
			return "", false, fmt.Errorf("percobaan %d: %w", n, err)
		}
		return "", false, err
	}
	if c.FailDir != "" {
		state.Clear(c.FailDir, slug)
	}
	if c.Delay > 0 {
		time.Sleep(c.Delay) // sopan terhadap server JDIH
	}
	return dest, true, nil
}
