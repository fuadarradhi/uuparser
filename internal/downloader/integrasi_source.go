package downloader

import (
	"context"
	"regexp"
	"strconv"
)

// reLeadingDigits mengambil deretan angka pertama. Nilai dari JDIH kerap
// bercampur teks ("3", "3A", "-", "NOMOR 3"); yang dibutuhkan hanya angkanya
// untuk diurutkan.
var reLeadingDigits = regexp.MustCompile(`[0-9]+`)

// parseSortInt mengembalikan nil bila tidak ada angka yang masuk akal —
// dokumen tersebut lalu diurutkan paling belakang (NULLS LAST).
func parseSortInt(s string) *int {
	m := reLeadingDigits.FindString(s)
	if m == "" {
		return nil
	}
	v, err := strconv.Atoi(m)
	if err != nil || v <= 0 {
		return nil
	}
	return &v
}

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
		// Yang diambil dari sumber: tautan unduh, plus tahun & nomor SEMATA
		// untuk mengurutkan antrian. Judul/jenis/instansi tetap diabaikan —
		// identitas peraturan dibaca sendiri dari halaman pertama oleh model
		// teks, karena metadata JDIH kerap tidak konsisten. Untuk pengurutan,
		// metadata yang sesekali keliru tidak berbahaya: paling jauh urutan
		// pengerjaan meleset sedikit.
		u := NormalizeURL(r.URLDownload)
		if u == "" {
			continue
		}
		out = append(out, RemoteDoc{
			FileURL:   u,
			SortTahun: parseSortInt(r.TahunPengundangan),
			SortNomor: parseSortInt(r.NoPeraturan),
		})
	}
	return out, nil
}

func (s *IntegrasiSource) FetchPDF(ctx context.Context, doc RemoteDoc) ([]byte, error) {
	return DownloadPDF(ctx, s.cfg, doc.FileURL)
}
