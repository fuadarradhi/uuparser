// Package downloader menyediakan building block untuk menarik metadata &
// mengunduh PDF dari situs JDIH. Orkestrasi tingkat-dokumen (retry, status,
// "sudah selesai atau belum") sekarang di DB lewat internal/store — lihat
// source.go untuk interface Source dan integrasi_source.go untuk implementasi
// /integrasi standar.
package downloader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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

// Config untuk operasi HTTP downloader (dipakai oleh implementasi Source).
type Config struct {
	Endpoint    string
	Delay       time.Duration
	MaxRetries  int
	HTTPTimeout time.Duration
}

var (
	reUnsafe   = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)
	reMultiUnd = regexp.MustCompile(`_{2,}`)
	reEdge     = regexp.MustCompile(`(^[._-]+)|([._-]+$)`)
	pdfMagic   = []byte("%PDF")
	reHrefPDF  = regexp.MustCompile(`(?i)href="([^"]+\.pdf[^"]*)"`)
)

// Slug mengembalikan identitas stabil & aman untuk sebuah record: stem fileDownload.
func Slug(r Record) string {
	name := strings.TrimSpace(r.FileDownload)
	if name == "" {
		return "doc_" + sanitize(r.IDData)
	}
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	return sanitize(stem)
}

// SourceCode menurunkan kode sumber ringkas dari URL endpoint, dipakai sebagai
// nilai default `sources.code` saat mendaftarkan sumber baru:
//
//	http://jdih.acehprov.go.id/integrasi        -> acehprov
//	http://jdih.acehbesarkab.go.id/integrasi    -> acehbesarkab
func SourceCode(endpoint string) string {
	host := endpoint
	port := ""
	if u, err := url.Parse(endpoint); err == nil && u.Hostname() != "" {
		host = u.Hostname()
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

// FetchIntegrasi menarik & mengurai daftar record dari endpoint /integrasi.
// Dipanggil oleh IntegrasiSource.ListDocuments (source.go). TIDAK menyimpan
// mentahnya ke disk — hasil sudah langsung didaftarkan ke DB oleh pemanggil.
func FetchIntegrasi(ctx context.Context, c Config) ([]Record, error) {
	body, _, err := httpGet(ctx, c, c.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("fetch integrasi: %w", err)
	}
	var records []Record
	if err := json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("parse integrasi (%d byte): %w", len(body), err)
	}
	return records, nil
}

// DownloadPDF mengunduh isi PDF untuk satu URL, mengikuti tautan HTML->PDF
// bila server mengembalikan halaman viewer, bukan PDF langsung.
func DownloadPDF(ctx context.Context, c Config, downloadURL string) ([]byte, error) {
	u := strings.TrimSpace(downloadURL)
	if u == "" {
		return nil, fmt.Errorf("urlDownload kosong")
	}
	body, ctype, err := httpGet(ctx, c, u)
	if err != nil {
		return nil, err
	}
	if isPDF(ctype, body) {
		return body, nil
	}
	// fallback: server mengembalikan HTML viewer -> cari tautan .pdf.
	if looksHTML(ctype, body) {
		if link := findPDFLink(body, u); link != "" {
			body2, ctype2, err := httpGet(ctx, c, link)
			if err != nil {
				return nil, fmt.Errorf("mengikuti tautan PDF: %w", err)
			}
			if isPDF(ctype2, body2) {
				return body2, nil
			}
			return nil, fmt.Errorf("tautan %s bukan PDF (ctype=%s)", link, ctype2)
		}
		return nil, fmt.Errorf("HTML tanpa tautan .pdf (ctype=%s)", ctype)
	}
	if bytes.HasPrefix(bytes.TrimSpace(body), pdfMagic) {
		return body, nil
	}
	return nil, fmt.Errorf("bukan PDF (ctype=%s, %d byte)", ctype, len(body))
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
		req.Header.Set("User-Agent", "uuparser-downloader/0.2")
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
