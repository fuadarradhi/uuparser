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
		out = append(out, RemoteDoc{
			IDData:  r.IDData,
			Judul:   r.Judul,
			FileURL: r.URLDownload,
			Meta: map[string]string{
				"jenis":              r.Jenis,
				"no_peraturan":       r.NoPeraturan,
				"tahun_pengundangan": r.TahunPengundangan,
				"file_download":      r.FileDownload,
			},
		})
	}
	return out, nil
}

func (s *IntegrasiSource) FetchPDF(ctx context.Context, doc RemoteDoc) ([]byte, error) {
	return DownloadPDF(ctx, s.cfg, doc.FileURL)
}
