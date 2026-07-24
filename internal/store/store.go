// Package store adalah satu-satunya titik akses ke Postgres.
//
// Sejak skema dokumen-sentris (2026-07-21): satu berkas PDF = satu baris
// `documents`, diidentifikasi tautan unduhnya (unik) dan hash isinya. Sumber
// hanyalah tempat tautan itu ditemukan, bukan bagian identitas.
//
// Seluruh primary key memakai bigserial/bigint (2026-07-22) — BUKAN UUID.
// Sebelumnya `sources`/`documents` memakai UUID sementara tabel anak
// (document_pages/nodes/relations) memakai bigserial; user meminta
// konsistensi satu skema ID di semua tabel. Tipe PK tidak mempengaruhi
// kecepatan filter WHERE (itu urusan index pada kolom yang difilter, sudah
// ada untuk status/wilayah/canonical_key/dst) — bigint dipilih di sini
// semata untuk konsistensi, bukan optimasi.
//
// Worker berkoordinasi lewat `SELECT ... FOR UPDATE SKIP LOCKED` — idiom
// antrian Postgres yang aman dipakai banyak goroutine (atau banyak proses)
// tanpa penguncian manual di sisi Go.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoWork: tidak ada baris yang bisa diambil saat ini. Worker
// memperlakukannya sebagai "tidur sebentar", bukan galat.
var ErrNoWork = errors.New("store: tidak ada pekerjaan tersedia")

// cleanupTimeout membatasi operasi pembersihan yang dijalankan SETELAH
// konteks utama dibatalkan (Ctrl+C). Operasi itu wajib memakai konteks
// tersendiri: memakai konteks yang sudah dibatalkan membuat pembersihan ikut
// gagal, sehingga dokumen tertinggal berstatus 'processing' selamanya.
const cleanupTimeout = 10 * time.Second

// cleanupCtx mengembalikan konteks segar berbatas waktu untuk pembersihan.
func cleanupCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), cleanupTimeout)
}

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// ---- Sumber ----

type SourceRow struct {
	ID              int64
	Code            string
	EndpointURL     string
	SourceType      string
	SourceConfigRaw []byte
}

func (s *Store) ListSources(ctx context.Context) ([]SourceRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, code, endpoint_url, source_type, source_config
		FROM sources ORDER BY code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceRow
	for rows.Next() {
		var r SourceRow
		if err := rows.Scan(&r.ID, &r.Code, &r.EndpointURL, &r.SourceType, &r.SourceConfigRaw); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- Pendaftaran tautan ----

// RegisterURL mendaftarkan satu tautan unduh. download_url UNIK: tautan yang
// sudah pernah didaftarkan — dari sumber mana pun — diabaikan diam-diam.
// Mengembalikan true bila baris benar-benar baru.
func (s *Store) RegisterURL(ctx context.Context, sourceID int64, downloadURL string, sortTahun, sortNomor *int) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO documents (download_url, first_source_id, sort_tahun, sort_nomor, status)
		VALUES ($1, $2, $3, $4, 'pending')
		ON CONFLICT (download_url) DO NOTHING`, downloadURL, sourceID, sortTahun, sortNomor)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ---- Tahap 1: unduh ----

type DownloadJob struct {
	ID          int64
	DownloadURL string
	SourceID    int64 // 0 bila first_source_id NULL
}

func (s *Store) ClaimForDownload(ctx context.Context) (DownloadJob, error) {
	var j DownloadJob
	var srcID *int64
	err := s.pool.QueryRow(ctx, `
		UPDATE documents SET status = 'downloading', updated_at = now()
		WHERE id = (
			-- URUTAN PENGERJAAN: sumber berprioritas kecil diselesaikan lebih
			-- dulu, lalu dokumen terbaru (tahun & nomor menurun). Dokumen
			-- tanpa tahun/nomor dari sumber dikerjakan paling belakang
			-- (NULLS LAST). created_at hanya pemutus seri agar urutannya
			-- tetap pasti. FOR UPDATE OF d wajib: LEFT JOIN membuat sisi
			-- sources bisa NULL, dan Postgres menolak mengunci sisi itu.
			SELECT d.id FROM documents d
			LEFT JOIN sources s ON s.id = d.first_source_id
			WHERE d.status IN ('pending', 'downloading')
			ORDER BY (d.status = 'downloading') DESC,
			         COALESCE(s.priority, 1000),
			         d.sort_tahun DESC NULLS LAST,
			         d.sort_nomor DESC NULLS LAST,
			         d.created_at
			LIMIT 1 FOR UPDATE OF d SKIP LOCKED
		)
		RETURNING id, download_url, first_source_id`,
	).Scan(&j.ID, &j.DownloadURL, &srcID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DownloadJob{}, ErrNoWork
	}
	if srcID != nil {
		j.SourceID = *srcID
	}
	return j, err
}

// MarkDownloaded menyimpan lokasi & hash berkas. Bila berkas dengan hash sama
// SUDAH ada (dokumen identik dari sumber lain), dokumen ini ditandai duplikat
// dan tidak diproses lebih lanjut. Mengembalikan true bila duplikat.
// MarkDownloaded menandai unduhan selesai. totalPages diisi pemanggil dari
// extractor.PageCount(dest) SEGERA setelah berkas ditulis ke disk (2026-07-24)
// — lihat downloader_worker.go. nil berarti penghitungannya gagal (mis. PDF
// rusak); disimpan sebagai NULL, bukan 0, supaya beda dari "memang 0
// halaman" dan tidak ikut disaring oleh ClaimForOCR (lihat query di sana).
func (s *Store) MarkDownloaded(ctx context.Context, id int64, pdfPath, sha string, size int64, totalPages *int) (bool, error) {
	var existing *int64
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM documents
		WHERE pdf_sha256 = $1 AND id <> $2 AND status NOT IN ('rejected','duplicate')
		LIMIT 1`, sha, id).Scan(&existing)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}

	if existing != nil {
		_, err := s.pool.Exec(ctx, `
			UPDATE documents
			SET status = 'duplicate', reject_reason = 'duplikat_isi', duplicate_of = $2,
			    pdf_path = $3, pdf_sha256 = $4, file_size = $5, total_pages = $6,
			    downloaded_at = now(), updated_at = now()
			WHERE id = $1`, id, *existing, pdfPath, sha, size, totalPages)
		return true, err
	}

	_, err = s.pool.Exec(ctx, `
		UPDATE documents
		SET status = 'downloaded', pdf_path = $2, pdf_sha256 = $3, file_size = $4,
		    total_pages = $5, downloaded_at = now(), updated_at = now(),
		    attempts = 0, last_error = NULL
		WHERE id = $1`, id, pdfPath, sha, size, totalPages)
	return false, err
}

func (s *Store) MarkDownloadFailed(ctx context.Context, id int64, errMsg string, maxAttempts int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents
		SET attempts = attempts + 1, last_error = $2,
		    status = CASE WHEN attempts + 1 >= $3 THEN 'failed' ELSE 'pending' END,
		    reject_reason = CASE WHEN attempts + 1 >= $3 THEN 'unduh_gagal' ELSE reject_reason END,
		    updated_at = now()
		WHERE id = $1`, id, errMsg, maxAttempts)
	return err
}

// RequeueDocument mengembalikan dokumen ke status semula TANPA menambah
// penghitung kegagalan. Dipakai ketika pekerjaan dihentikan bukan karena
// datanya bermasalah — pembatalan (Ctrl+C), model gagal dimuat, database
// sesaat tak terjangkau.
//
// Membedakan ini dari kegagalan sungguhan itu penting: menghitungnya sebagai
// kegagalan berarti beberapa kali Ctrl+C saja sudah cukup membuat dokumen
// yang sehat berstatus 'failed' dan tidak pernah diproses lagi.
//
// Memakai konteksnya sendiri karena biasanya dipanggil setelah konteks utama
// dibatalkan.
func (s *Store) RequeueDocument(id int64, status string) error {
	ctx, cancel := cleanupCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `
		UPDATE documents SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	return err
}

// ---- Tahap 2: OCR + perbaikan + klasifikasi ----

type OCRJob struct {
	ID      int64
	PDFPath string
}

// ClaimForOCR mengambil dokumen yang sudah terunduh dan belum diproses.
//
// maxPage (2026-07-24, konsep baru menggantikan pemotongan per-halaman):
// dokumen yang total_pages-nya (dihitung SEKALI saat unduh — lihat
// MarkDownloaded) MELEBIHI maxPage TIDAK PERNAH diambil sama sekali selama
// maxPage masih berlaku — bukan dipotong di tengah seperti skema lama.
// total_pages IS NULL (penghitungan saat unduh gagal) TETAP diambil seperti
// biasa — supaya dokumen begitu tidak tersangkut permanen hanya karena
// jumlah halamannya tidak diketahui. maxPage<=0 berarti tanpa saringan sama
// sekali (mode produksi, MAX_PAGE dimatikan).
//
// Urutan queue JUGA berubah: total_pages ASC disisipkan sebagai penentu
// utama (setelah dokumen 'processing' yang harus didahulukan, dan prioritas
// sumber) — dokumen PENDEK dikerjakan lebih dulu, permintaan eksplisit user
// supaya iterasi uji parser tidak tersandera satu peraturan tebal.
// ClaimForOCR mengambil dokumen yang sudah terunduh dan belum diproses.
//
// minPage/maxPage (2026-07-24, dari config.PageCountRange/env
// PAGE_COUNT_RANGE — sebelumnya cuma maxPage/PAGE_COUNT_MAX): dokumen yang
// total_pages-nya DI LUAR rentang [minPage, maxPage] TIDAK PERNAH diambil
// sama sekali — bukan dipotong di tengah. <=0 pada salah satu sisi berarti
// sisi itu tidak dibatasi. total_pages IS NULL (penghitungan saat unduh
// gagal) TETAP diambil seperti biasa terlepas dari kedua batas ini — supaya
// dokumen begitu tidak tersangkut permanen hanya karena jumlah halamannya
// tidak diketahui.
//
// order ("asc" | "desc", lihat config.PageCountOrder/normalizeSortOrder —
// pemanggil bertanggung jawab memvalidasi/menormalisasi SEBELUM sampai
// sini): arah urutan total_pages. "asc" (bawaan) = dokumen PENDEK duluan.
// "desc" = dokumen PANJANG duluan (permintaan user, 2026-07-24, supaya bisa
// sengaja uji dokumen paling tebal lebih dulu). Nilai APA PUN selain persis
// "desc" (termasuk kosong/typo) diperlakukan sebagai "asc" — dibangun lewat
// fmt.Sprintf ke klausa ORDER BY karena arah sort tidak bisa di-bind lewat
// placeholder biasa; AMAN karena nilainya HANYA PERNAH salah satu dari dua
// literal tetap ini, tidak pernah data dari luar.
func (s *Store) ClaimForOCR(ctx context.Context, minPage, maxPage int, order string) (OCRJob, error) {
	dir := "ASC"
	if order == "desc" {
		dir = "DESC"
	}
	var j OCRJob
	query := fmt.Sprintf(`
		UPDATE documents SET status = 'processing', updated_at = now()
		WHERE id = (
			-- 'processing' ikut diambil: itu dokumen yang terhenti di tengah
			-- jalan (mis. Ctrl+C). Tanpa ini dokumen tersebut tidak akan
			-- pernah tersentuh lagi dan tertinggal selamanya.
			--
			-- Dokumen yang terhenti DIDAHULUKAN daripada dokumen mana pun,
			-- termasuk yang lebih baru: menyelesaikan yang sudah separuh
			-- jalan lebih berharga daripada memulai yang baru.
			SELECT d.id FROM documents d
			LEFT JOIN sources s ON s.id = d.first_source_id
			WHERE d.status IN ('downloaded', 'processing')
			      AND (d.total_pages IS NULL
			           OR (($1 <= 0 OR d.total_pages <= $1)
			               AND ($2 <= 0 OR d.total_pages >= $2)))
			ORDER BY (d.status = 'processing') DESC,
			         COALESCE(s.priority, 1000),
			         d.total_pages %s NULLS LAST,
			         d.sort_tahun DESC NULLS LAST,
			         d.sort_nomor DESC NULLS LAST,
			         d.created_at
			LIMIT 1 FOR UPDATE OF d SKIP LOCKED
		)
		RETURNING id, pdf_path`, dir)
	err := s.pool.QueryRow(ctx, query, maxPage, minPage).Scan(&j.ID, &j.PDFPath)
	if errors.Is(err, pgx.ErrNoRows) {
		return OCRJob{}, ErrNoWork

	}
	return j, err
}

// GetTotalPages mengembalikan jumlah halaman ASLI dokumen yang sudah dicatat
// SEKALI saat unduh (lihat MarkDownloaded), tanpa membuka berkas PDF-nya.
// ok=false berarti belum tercatat (dokumen dari sebelum fitur ini ada, atau
// penghitungan saat unduh gagal) — pemanggil sebaiknya jatuh ke
// extractor.PageCount sebagai jaga-jaga.
func (s *Store) GetTotalPages(ctx context.Context, documentID int64) (n int, ok bool, err error) {
	var v *int
	err = s.pool.QueryRow(ctx, `SELECT total_pages FROM documents WHERE id = $1`, documentID).Scan(&v)
	if err != nil {
		return 0, false, err
	}
	if v == nil {
		return 0, false, nil
	}
	return *v, true, nil
}

func (s *Store) MarkOCRFailed(ctx context.Context, id int64, errMsg string, maxAttempts int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents
		SET attempts = attempts + 1, last_error = $2,
		    status = CASE WHEN attempts + 1 >= $3 THEN 'failed' ELSE 'downloaded' END,
		    updated_at = now()
		WHERE id = $1`, id, errMsg, maxAttempts)
	return err
}

// SavePage menyimpan hasil OCR satu halaman. ocr_text tidak pernah diubah
// setelahnya; koreksi manusia ditulis ke edited_text lewat UI.
func (s *Store) SavePage(ctx context.Context, documentID int64, page int, ocrText string,
	isEmpty, isTruncated bool, inkRatio, croppedPct float64, dpiPakai, durationMS int, notes []string) error {
	notesJSON, _ := json.Marshal(notes)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO document_pages
			(document_id, page_number, ocr_text, is_empty, is_truncated,
			 ink_ratio, cropped_pct, dpi_pakai, duration_ms, notes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (document_id, page_number) DO UPDATE SET
			ocr_text = EXCLUDED.ocr_text, is_empty = EXCLUDED.is_empty,
			is_truncated = EXCLUDED.is_truncated, ink_ratio = EXCLUDED.ink_ratio,
			cropped_pct = EXCLUDED.cropped_pct, dpi_pakai = EXCLUDED.dpi_pakai,
			duration_ms = EXCLUDED.duration_ms, notes = EXCLUDED.notes`,
		documentID, page, ocrText, isEmpty, isTruncated, inkRatio, croppedPct, dpiPakai, durationMS, notesJSON)
	return err
}

// DPIPage1 mengembalikan DPI yang SUDAH dipakai untuk halaman 1 dokumen ini,
// atau 0 bila halaman 1 belum diproses.
//
// Dipakai saat MELANJUTKAN dokumen yang terhenti SETELAH halaman 1 selesai:
// tanpa ini, extractor akan mengira halaman berikutnya (yang diproses
// pertama pada penjalanan yang dilanjutkan) adalah "halaman pertama" dan
// menghitung ulang skor ketajaman dari situ — padahal keputusan DPI
// seharusnya SELALU mengikuti halaman 1 aslinya (lihat
// extractor.renderAdaptif).
func (s *Store) DPIPage1(ctx context.Context, documentID int64) (int, error) {
	var dpi *int
	err := s.pool.QueryRow(ctx, `
		SELECT dpi_pakai FROM document_pages
		WHERE document_id = $1 AND page_number = 1`, documentID).Scan(&dpi)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if dpi == nil {
		return 0, nil
	}
	return *dpi, nil
}

// GetTextSource mengembalikan keputusan sumber teks untuk dokumen ini
// ('ocr' | 'pdftotext'), atau string kosong bila belum diputuskan — lihat
// pipeline.resolveTextSource.
func (s *Store) GetTextSource(ctx context.Context, documentID int64) (string, error) {
	var v *string
	err := s.pool.QueryRow(ctx, `SELECT text_source FROM documents WHERE id = $1`, documentID).Scan(&v)
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", nil
	}
	return *v, nil
}

// SetTextSource menandai keputusan sumber teks dokumen ini. Sekali
// diputuskan, dipakai apa adanya saat dokumen dilanjutkan (resume) —
// keputusan TIDAK dihitung ulang per halaman maupun antar-penjalanan.
func (s *Store) SetTextSource(ctx context.Context, documentID int64, source string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents SET text_source = $2, updated_at = now() WHERE id = $1`,
		documentID, source)
	return err
}

func (s *Store) HasPage(ctx context.Context, documentID int64, page int) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM document_pages WHERE document_id = $1 AND page_number = $2)`,
		documentID, page).Scan(&ok)
	return ok, err
}

// FirstNonEmptyPage mengembalikan halaman BERISI pertama beserta teksnya.
//
// Klasifikasi memakai ini, bukan selalu halaman 1: halaman pertama bisa saja
// kosong karena artefak pindaian (lembar sampul terpindai polos, halaman
// tergeser). Memaksakan halaman 1 pada kasus itu berarti model diberi teks
// kosong, dan dokumen yang sebenarnya sah ikut tertolak.
func (s *Store) FirstNonEmptyPage(ctx context.Context, documentID int64) (page int, text string, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT page_number, COALESCE(edited_text, ocr_text)
		FROM document_pages
		WHERE document_id = $1 AND is_empty = false
		      AND COALESCE(edited_text, ocr_text) <> ''
		ORDER BY page_number LIMIT 1`, documentID).Scan(&page, &text)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return page, text, true, nil
}

// CountPagesDone melaporkan berapa halaman yang sudah di-OCR dan berapa yang
// sudah diperbaiki. Dipakai saat melanjutkan dokumen supaya baris kemajuan
// meneruskan hitungan sebelumnya, bukan mulai dari nol seolah belum ada
// pekerjaan yang selesai.
func (s *Store) CountPagesDone(ctx context.Context, documentID int64) (ocred, done int, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(*) FROM document_pages WHERE document_id = $1`,
		documentID).Scan(&ocred, &done)
	return ocred, done, err
}

// IsClassified melaporkan apakah metadata halaman 1 sudah pernah dibaca,
// sehingga dokumen yang dilanjutkan tidak diklasifikasi ulang percuma.
func (s *Store) IsClassified(ctx context.Context, documentID int64) (bool, error) {
	var done bool
	err := s.pool.QueryRow(ctx, `
		SELECT classified_at IS NOT NULL FROM documents WHERE id = $1`, documentID).Scan(&done)
	return done, err
}

// SavedPage adalah SATU baris document_pages LENGKAP dengan metadata
// render/OCR-nya — dipakai ListSavedPages untuk merekonstruksi entri
// extractor.PageResult bagi halaman yang sudah tersimpan dari penjalanan
// SEBELUMNYA (lihat catatan di ListSavedPages).
type SavedPage struct {
	Page        int
	Text        string
	IsEmpty     bool
	IsTruncated bool
	InkRatio    float64
	CroppedPct  float64
	DPI         int
	DurationMS  int
	Notes       []string
}

// ListSavedPages mengembalikan SEMUA halaman yang sudah tersimpan untuk satu
// dokumen, urut nomor halaman, lengkap dengan metadata render/OCR-nya.
//
// [Ditambahkan 2026-07-23] Sebelumnya, ocr.txt mode debug (lihat
// debug_writer.go) HANYA berisi halaman yang diproses pada PENJALANAN INI —
// halaman yang sudah tersimpan dari penjalanan sebelumnya (dokumen yang
// sempat berhenti lalu dilanjutkan) tidak pernah tampil lagi, sehingga
// ocr.txt terlihat seolah dokumennya cuma sepanjang itu. Dipakai
// ocr_worker.go untuk mengisi ulang debugWriter dengan halaman-halaman lama
// SEBELUM halaman baru diproses, supaya ocr.txt selalu mencerminkan SELURUH
// dokumen, bukan cuma sisa yang diproses penjalanan terakhir.
func (s *Store) ListSavedPages(ctx context.Context, documentID int64) ([]SavedPage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT page_number, COALESCE(edited_text, ocr_text), is_empty, is_truncated,
		       COALESCE(ink_ratio, 0), COALESCE(cropped_pct, 0), COALESCE(dpi_pakai, 0),
		       COALESCE(duration_ms, 0), notes
		FROM document_pages
		WHERE document_id = $1
		ORDER BY page_number`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SavedPage
	for rows.Next() {
		var p SavedPage
		var notesJSON []byte
		if err := rows.Scan(&p.Page, &p.Text, &p.IsEmpty, &p.IsTruncated,
			&p.InkRatio, &p.CroppedPct, &p.DPI, &p.DurationMS, &notesJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(notesJSON, &p.Notes) // notes cuma kosmetik; abaikan galat urai
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPageText mengembalikan teks yang berlaku untuk satu halaman:
// koreksi manusia > perbaikan model > OCR mentah.
func (s *Store) GetPageText(ctx context.Context, documentID int64, page int) (string, error) {
	var t string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(edited_text, ocr_text) FROM document_pages
		WHERE document_id = $1 AND page_number = $2`, documentID, page).Scan(&t)
	return t, err
}

func (s *Store) ReadPageRange(ctx context.Context, documentID int64, a, b int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(edited_text, ocr_text) FROM document_pages
		WHERE document_id = $1 AND page_number BETWEEN $2 AND $3
		ORDER BY page_number`, documentID, a, b)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DocMeta adalah metadata hasil pembacaan model teks atas halaman pertama.
type DocMeta struct {
	IsPeraturan bool
	Jenis       string
	// Wilayah adalah bentuk baku hasil pemetaan deterministik ke salah satu
	// dari 25 wilayah yang dikenal sistem ("PEMERINTAH ACEH", "KABUPATEN
	// ACEH BARAT", "NASIONAL", dst — lihat pipeline.WilayahList);
	// InstansiTertulis menyimpan apa yang benar-benar tercetak di dokumen
	// ("GUBERNUR ACEH"). Keduanya disimpan supaya hasil pemetaan selalu
	// dapat ditelusuri balik ke sumbernya. Nama kolom sebelumnya "instansi" —
	// diganti "wilayah" karena isinya memang selalu nama wilayah
	// administratif, bukan instansi generik.
	Wilayah          string
	InstansiTertulis string
	// Nomor disimpan apa adanya ("300.2/ 69 /2026"); NomorUrut adalah angka
	// pertamanya (300) semata untuk pengurutan.
	Nomor     string
	NomorUrut int
	Tahun     string
	Tentang   string
	Struktur  string
	Alasan    string
}

// Penetapan memuat bagian penutup dokumen.
type Penetapan struct {
	DitetapkanDi      string
	DitetapkanTanggal string
	DitetapkanOleh    string
	// DitetapkanOlehNama adalah NAMA orang penanda tangan (mis. "MUZAKIR
	// MANAF"), terpisah dari DitetapkanOleh yang jabatannya saja (mis.
	// "GUBERNUR ACEH") — permintaan user, 2026-07-22.
	DitetapkanOlehNama string

	DiundangkanDi       string
	DiundangkanTanggal  string
	DiundangkanOleh     string
	DiundangkanOlehNama string
}

// SavePenetapan menyimpan bagian penetapan & pengundangan. Hanya kolom yang
// terisi yang ditimpa, sehingga hasil penguraian deterministik tidak terhapus
// oleh pemanggilan berikutnya yang kebetulan mengembalikan nilai kosong.
func (s *Store) SavePenetapan(ctx context.Context, id int64, p Penetapan) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents SET
			ditetapkan_di         = COALESCE(NULLIF($2,''), ditetapkan_di),
			ditetapkan_tanggal    = COALESCE(NULLIF($3,''), ditetapkan_tanggal),
			ditetapkan_oleh       = COALESCE(NULLIF($4,''), ditetapkan_oleh),
			ditetapkan_oleh_nama  = COALESCE(NULLIF($5,''), ditetapkan_oleh_nama),
			diundangkan_di        = COALESCE(NULLIF($6,''), diundangkan_di),
			diundangkan_tanggal   = COALESCE(NULLIF($7,''), diundangkan_tanggal),
			diundangkan_oleh      = COALESCE(NULLIF($8,''), diundangkan_oleh),
			diundangkan_oleh_nama = COALESCE(NULLIF($9,''), diundangkan_oleh_nama),
			updated_at = now()
		WHERE id = $1`, id,
		p.DitetapkanDi, p.DitetapkanTanggal, p.DitetapkanOleh, p.DitetapkanOlehNama,
		p.DiundangkanDi, p.DiundangkanTanggal, p.DiundangkanOleh, p.DiundangkanOlehNama)
	return err
}

// RejectNotRegulation menandai dokumen bukan peraturan; sisa halaman tidak
// akan di-OCR.
func (s *Store) RejectNotRegulation(ctx context.Context, id int64, alasan string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents
		SET status = 'rejected', is_peraturan = false, reject_reason = 'bukan_peraturan',
		    last_error = $2, classified_at = now(), updated_at = now()
		WHERE id = $1`, id, alasan)
	return err
}

// ApplyMetaAndCheckDuplicate menyimpan metadata halaman 1 lalu memeriksa
// apakah peraturan dengan identitas kanonik sama sudah ada. Bila ya, dokumen
// ini ditandai duplikat (true dikembalikan) dan tidak diproses lanjut.
//
// canonicalKey kosong (identitas tidak lengkap terbaca) berarti pemeriksaan
// duplikat dilewati — lebih baik memproses dua kali daripada membuang dokumen
// sah karena metadatanya tak terbaca.
func (s *Store) ApplyMetaAndCheckDuplicate(ctx context.Context, id int64, m DocMeta, canonicalKey string) (bool, error) {
	if canonicalKey != "" {
		var existing *int64
		err := s.pool.QueryRow(ctx, `
			SELECT id FROM documents
			WHERE canonical_key = $1 AND id <> $2 AND status NOT IN ('rejected','duplicate')
			LIMIT 1`, canonicalKey, id).Scan(&existing)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
		if existing != nil {
			_, err := s.pool.Exec(ctx, `
				UPDATE documents
				SET status = 'duplicate', reject_reason = 'duplikat_peraturan', duplicate_of = $2,
				    is_peraturan = true, jenis = $3, wilayah = $4, instansi_tertulis = $5,
				    nomor = $6, nomor_urut = $7, tahun = $8,
				    tentang = $9, struktur = $10, canonical_key = $11,
				    classified_at = now(), updated_at = now()
				WHERE id = $1`, id, *existing, nullIfEmpty(m.Jenis), nullIfEmpty(m.Wilayah),
				nullIfEmpty(m.InstansiTertulis), nullIfEmpty(m.Nomor), nullIfZero(m.NomorUrut),
				nullIfEmpty(m.Tahun), nullIfEmpty(m.Tentang),
				nullIfEmpty(m.Struktur), canonicalKey)
			return true, err
		}
	}

	_, err := s.pool.Exec(ctx, `
		UPDATE documents
		SET is_peraturan = true, jenis = $2, wilayah = $3, instansi_tertulis = $4,
		    nomor = $5, nomor_urut = $6, tahun = $7,
		    tentang = $8, struktur = $9, canonical_key = $10,
		    classified_at = now(), updated_at = now()
		WHERE id = $1`, id, nullIfEmpty(m.Jenis), nullIfEmpty(m.Wilayah),
		nullIfEmpty(m.InstansiTertulis), nullIfEmpty(m.Nomor), nullIfZero(m.NomorUrut),
		nullIfEmpty(m.Tahun), nullIfEmpty(m.Tentang),
		nullIfEmpty(m.Struktur), nullIfEmpty(canonicalKey))
	return false, err
}

func (s *Store) MarkOCRDone(ctx context.Context, id int64, totalPages int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents
		SET status = 'ocr_done', total_pages = $2, ocr_completed_at = now(), updated_at = now()
		WHERE id = $1`, id, totalPages)
	return err
}

// ---- Tahap 3: parse (SEKALI SAJA — lihat InsertParseResult) ----

type ParseJob struct {
	ID       int64
	NumPages int
	// Jenis (2026-07-24): dibawa dari documents.jenis (hasil classify tahap
	// OCR) supaya parser_worker bisa memutuskan bypass gerbang deterministik
	// parser untuk jenis TERTENTU yang memang tidak selalu punya Pasal/BAB
	// (mis. "SURAT EDARAN") — lihat parser.Parse / ParseAllowNonRegulation.
	Jenis string
}

func (s *Store) ClaimForParse(ctx context.Context) (ParseJob, error) {
	var j ParseJob
	var jenis *string
	err := s.pool.QueryRow(ctx, `
		UPDATE documents SET status = 'parsing', updated_at = now()
		WHERE id = (
			SELECT d.id FROM documents d
			LEFT JOIN sources s ON s.id = d.first_source_id
			WHERE d.status = 'ocr_done'
			ORDER BY COALESCE(s.priority, 1000),
			         d.sort_tahun DESC NULLS LAST,
			         d.sort_nomor DESC NULLS LAST,
			         d.ocr_completed_at
			LIMIT 1 FOR UPDATE OF d SKIP LOCKED
		)
		RETURNING id, jenis,
			(SELECT COUNT(*) FROM document_pages WHERE document_id = documents.id)`,
	).Scan(&j.ID, &jenis, &j.NumPages)
	if errors.Is(err, pgx.ErrNoRows) {
		return ParseJob{}, ErrNoWork
	}
	if jenis != nil {
		j.Jenis = *jenis
	}
	return j, err
}

// RequeueStuckParsing mengembalikan dokumen yang macet berstatus 'parsing'
// (proses mati mendadak di tengah parse, ATAU reset_parser.sql baru saja
// mengembalikan dokumen ke 'ocr_done' sementara worker sedang tidak
// menyadarinya) kembali dapat diklaim. Dipanggil SEKALI saat parserWorker
// mulai (lihat pipeline/parser_worker.go). Bukan skenario kegagalan data
// (prinsip "interruption != data failure") — tidak menambah attempts, dan
// tidak perlu dipanggil manual setelah reset_parser.sql: skrip itu sendiri
// sudah men-set status ke 'ocr_done', jadi ClaimForParse otomatis
// mengambilnya lagi pada polling berikutnya. Fungsi ini murni jaring
// pengaman untuk kasus proses mati di TENGAH parse (status nyangkut di
// 'parsing', tidak pernah sampai 'ocr_done' lagi tanpa ini).
func (s *Store) RequeueStuckParsing(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE documents SET status = 'ocr_done', updated_at = now()
		WHERE status = 'parsing'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

type NodeInsert struct {
	ParentIdx        int // indeks ke NodeInsert lain dalam slice yang sama, -1 = akar
	Section          string
	NodeType         string
	BabNumber        *string
	BagianLabel      *string
	ParagrafLabel    *string
	PasalNumber      *string
	AyatNumber       *string
	HurufLabel       *string
	AngkaLabel       *string
	Label            *string
	Content          string
	StartPage        int
	EndPage          int
	OrderIndex       int64
	OriginalNodeType *string
	Warnings         []byte
	Citation         *string
	// IsAppendix (2026-07-23): lihat catatan di parser.Node.IsAppendix
	// dan kolom nodes.is_appendix di schema.sql.
	IsAppendix bool
	// IsDictum/IsTitle (2026-07-24): lihat catatan di parser.Node.IsDictum/
	// IsTitle dan kolom nodes.is_dictum/is_title di schema.sql.
	IsDictum bool
	IsTitle  bool
}

// ReviewFlagInsert (2026-07-24) — satu baris untuk tabel review_flags,
// dikirim bersama nodes dalam SATU panggilan InsertParseResult supaya
// keduanya masuk dalam satu transaksi (atomik dengan penyimpanan node,
// konsisten dengan prinsip "parse sekali, transaksi sekali").
//
// NodeIdx mengacu ke INDEKS pada slice `nodes` yang dikirim BERSAMAAN ke
// InsertParseResult (BUKAN id database — id baru belum ada saat pemanggil
// menyusun slice ini). -1 berarti flag di level dokumen, tidak terikat
// node manapun (mis. dari parser.Issue, yang memang tidak membawa
// referensi node).
type ReviewFlagInsert struct {
	NodeIdx  int // -1 = level dokumen (tidak terikat node)
	Source   string
	Code     string
	Severity string
	Message  string
}

// InsertParseResult menyimpan hasil parse PERTAMA (dan SATU-SATUNYA) untuk
// dokumen ini, dalam satu transaksi.
//
// Reparse DIHAPUS (2026-07-22, permintaan user): parser hanya boleh jalan
// sekali per dokumen — koreksi sesudahnya dilakukan MANUSIA langsung di
// tabel `nodes` (drag-drop/relabel via UI), bukan dengan menjalankan parser
// ulang. Sebelumnya fungsi ini bernama ReplaceParseResult dan melakukan
// DELETE lalu INSERT ulang (dirancang supaya reparse berkali-kali tidak
// menumpuk baris) — DELETE itu sudah dibuang. Memanggil fungsi ini untuk
// dokumen yang SUDAH punya parse_snapshot sekarang GAGAL KERAS lewat
// constraint PRIMARY KEY (document_id) di parse_snapshots — kesalahan
// pemrograman, bukan alur normal, dan sengaja tidak ditangkap diam-diam.
//
// flags (2026-07-24): baris review_flags yang menyertai hasil parse ini —
// lihat catatan ReviewFlagInsert.
func (s *Store) InsertParseResult(ctx context.Context, documentID int64, status string,
	report, extractionNotes []byte, nodes []NodeInsert, flags []ReviewFlagInsert) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `
		INSERT INTO parse_snapshots (document_id, status, report, extraction_notes, parsed_at)
		VALUES ($1,$2,$3,$4, now())`,
		documentID, status, report, extractionNotes); err != nil {
		return fmt.Errorf("simpan parse_snapshot (dokumen sudah pernah di-parse?): %w", err)
	}

	ids := make([]int64, len(nodes))
	for i, n := range nodes {
		var parentID *int64
		if n.ParentIdx >= 0 {
			parentID = &ids[n.ParentIdx]
		}
		var newID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO nodes (
				document_id, parent_id, section, node_type,
				bab_number, bagian_label, paragraf_label, pasal_number, ayat_number,
				huruf_label, angka_label, label, content,
				start_page, end_page, order_index,
				original_node_type, warnings, citation, is_appendix,
				is_dictum, is_title
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
			RETURNING id`,
			documentID, parentID, n.Section, n.NodeType,
			n.BabNumber, n.BagianLabel, n.ParagrafLabel, n.PasalNumber, n.AyatNumber,
			n.HurufLabel, n.AngkaLabel, n.Label, n.Content,
			n.StartPage, n.EndPage, n.OrderIndex,
			n.OriginalNodeType, n.Warnings, n.Citation, n.IsAppendix,
			n.IsDictum, n.IsTitle,
		).Scan(&newID); err != nil {
			return err
		}
		ids[i] = newID
	}

	for _, f := range flags {
		var nodeID *int64
		if f.NodeIdx >= 0 && f.NodeIdx < len(ids) {
			nodeID = &ids[f.NodeIdx]
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO review_flags (document_id, node_id, source, code, severity, message)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			documentID, nodeID, f.Source, f.Code, f.Severity, f.Message); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE documents SET status = 'parsed', parse_status = $2, parsed_at = now(), updated_at = now()
		WHERE id = $1`, documentID, status); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) InsertRelation(ctx context.Context, documentID int64, relType, key, jenis,
	instansi, nomor, tahun, tentang, confidence, kutipan string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO relations (document_id, type, key, jenis, instansi, nomor, tahun,
		                        tentang, confidence, kutipan)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		documentID, relType, key, jenis, instansi, nomor, tahun, tentang, confidence, kutipan)
	return err
}

// ---- Tinjauan (review_flags) — untuk UI web ----

// ReviewFlag adalah satu baris review_flags untuk dibaca (lawan dari
// ReviewFlagInsert, yang untuk ditulis). NodeID nil berarti flag level
// dokumen. Field apa adanya (bukan JSON marshal terpisah) supaya API
// web bisa langsung json.Marshal slice-nya.
type ReviewFlag struct {
	ID           int64      `json:"id"`
	DocumentID   int64      `json:"document_id"`
	NodeID       *int64     `json:"node_id,omitempty"`
	Source       string     `json:"source"`
	Code         string     `json:"code"`
	Severity     string     `json:"severity"`
	Message      string     `json:"message"`
	Resolved     bool       `json:"resolved"`
	ResolvedAt   *time.Time `json:"resolved_at,omitempty"`
	ResolvedNote string     `json:"resolved_note,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// ListReviewFlags mengembalikan seluruh review_flags untuk satu dokumen,
// terbaru dulu — dipakai halaman detail dokumen di UI web. onlyUnresolved
// membatasi ke yang belum ditandai selesai (dipakai daftar antrian).
func (s *Store) ListReviewFlags(ctx context.Context, documentID int64, onlyUnresolved bool) ([]ReviewFlag, error) {
	q := `SELECT id, document_id, node_id, source, code, severity, message,
	             resolved, resolved_at, coalesce(resolved_note,''), created_at
	      FROM review_flags WHERE document_id = $1`
	if onlyUnresolved {
		q += ` AND NOT resolved`
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.pool.Query(ctx, q, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewFlag
	for rows.Next() {
		var f ReviewFlag
		if err := rows.Scan(&f.ID, &f.DocumentID, &f.NodeID, &f.Source, &f.Code, &f.Severity,
			&f.Message, &f.Resolved, &f.ResolvedAt, &f.ResolvedNote, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ResolveReviewFlag menandai satu baris review_flags selesai ditinjau —
// dipanggil dari UI web saat manusia menekan "sudah dicek". TIDAK PERNAH
// mengubah data hasil parse (nodes/parse_snapshots) itu sendiri — koreksi
// data tetap lewat edit langsung ke tabel nodes, seperti sudah disepakati.
func (s *Store) ResolveReviewFlag(ctx context.Context, id int64, note string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE review_flags SET resolved = true, resolved_at = now(), resolved_note = $2
		WHERE id = $1`, id, note)
	return err
}

func nullIfZero(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
