package downloader

import "context"

// integrasi_source.go: implementasi Source untuk endpoint JDIH standar
// (/integrasi mengembalikan seluruh dataset sebagai satu array JSON).
// Ini SATU-SATUNYA implementasi bawaan — sumber lain (API custom, scraping)
// dapat file implementasi masing-masing dan didaftarkan di source.go's
// NewSource.

type IntegrasiSource struct {
	code string
	cfg  Config
}

// NewIntegrasiSource membuat Source untuk endpoint /integrasi standar.
func NewIntegrasiSource(row SourceRow, cfg Config) *IntegrasiSource {
	cfg.Endpoint = row.EndpointURL
	return &IntegrasiSource{code: row.Code, cfg: cfg}
}

func (s *IntegrasiSource) Code() string { return s.code }

func (s *IntegrasiSource) ListDocuments(ctx context.Context) ([]RemoteDoc, error) {
	records, err := FetchIntegrasi(ctx, s.cfg)
	if err != nil {
		return nil, err
	}
	out := make([]RemoteDoc, 0, len(records))
	for _, r := range records {
		// Hanya tautan unduh yang diambil. Metadata dari sumber (judul,
		// jenis, nomor, tahun) SENGAJA diabaikan — sejak 2026-07-21 seluruh
		// identitas peraturan dibaca sendiri dari halaman pertama dokumen
		// oleh model teks, karena metadata JDIH kerap tidak konsisten.
		u := NormalizeURL(r.URLDownload)
		if u == "" {
			continue
		}
		out = append(out, RemoteDoc{FileURL: u})
	}
	return out, nil
}

func (s *IntegrasiSource) FetchPDF(ctx context.Context, doc RemoteDoc) ([]byte, error) {
	return DownloadPDF(ctx, s.cfg, doc.FileURL)
}
