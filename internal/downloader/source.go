package downloader

import "context"

// source.go: interface Source memisahkan "cara mendapat daftar dokumen & PDF
// dari satu situs JDIH" dari orkestrasi pipeline. Ditambahkan 2026-07-20
// karena tidak semua JDIH pakai /integrasi — sebagian punya API custom,
// sebagian tidak punya API sama sekali (perlu scraping).
//
// Nambah sumber baru = tulis satu implementasi baru yang memenuhi interface
// ini + insert baris di tabel `sources` (kolom source_type menentukan
// implementasi mana yang dipakai lewat NewSource) — TIDAK menyentuh kode
// pipeline (main.go, internal/store) sama sekali.
//
// SENGAJA tidak ada "generic scraper": tiap situs yang butuh scraping
// mestinya dapat implementasi bespoke sendiri (struktur HTML tiap situs
// pemerintah biasanya berbeda dan gampang berubah) — jangan dipaksakan lewat
// satu scraper generik yang rapuh.

// RemoteDoc adalah satu entri dokumen dari sebuah Source.
//
// Sejak 2026-07-21 isinya HANYA tautan unduh: pipeline tidak lagi mempercayai
// metadata apa pun dari sumber (judul/jenis/nomor/tahun kerap tidak konsisten
// antar-JDIH). Seluruh identitas peraturan dibaca dari halaman pertama berkas
// itu sendiri oleh model teks.
type RemoteDoc struct {
	FileURL string // URL PDF, sudah dinormalisasi lewat NormalizeURL
}

// Source adalah satu sumber JDIH. Implementasinya BEBAS bagaimana cara
// mendapatkannya (HTTP JSON API, API custom, atau scraping HTML) — pipeline
// hanya bicara lewat dua method ini.
type Source interface {
	// Code adalah slug sumber ini, sama dengan sources.code di DB.
	Code() string
	// ListDocuments mengambil daftar dokumen TERKINI dari sumber.
	ListDocuments(ctx context.Context) ([]RemoteDoc, error)
	// FetchPDF mengunduh isi PDF untuk satu RemoteDoc.
	FetchPDF(ctx context.Context, doc RemoteDoc) ([]byte, error)
}

// SourceRow adalah baris `sources` yang relevan untuk factory NewSource —
// dipetakan dari internal/store, diteruskan di sini supaya package downloader
// tidak perlu import internal/store (hindari dependency siklik).
type SourceRow struct {
	Code            string
	EndpointURL     string
	SourceType      string
	SourceConfigRaw []byte // JSON mentah dari kolom source_config jsonb
}

// NewSource adalah factory: pilih implementasi Source berdasarkan
// SourceRow.SourceType. Tambah case baru di sini setiap kali ada
// source_type baru (mis. "scrape_<nama_situs>").
func NewSource(row SourceRow, cfg Config) (Source, error) {
	switch row.SourceType {
	case "", "integrasi":
		return NewIntegrasiSource(row, cfg), nil
	default:
		// Tempat menambah scraper/API-custom bespoke per situs, mis.:
		//   case "scrape_contoh_jdih":
		//       return NewContohJDIHScraper(row, cfg), nil
		return nil, &unknownSourceTypeError{sourceType: row.SourceType, code: row.Code}
	}
}

type unknownSourceTypeError struct {
	sourceType string
	code       string
}

func (e *unknownSourceTypeError) Error() string {
	return "downloader: source_type '" + e.sourceType + "' untuk source '" + e.code +
		"' belum ada implementasinya — tambahkan case di downloader.NewSource"
}
