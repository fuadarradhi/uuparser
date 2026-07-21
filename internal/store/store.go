// Package store adalah satu-satunya titik akses ke Postgres.
//
// Sejak skema dokumen-sentris (2026-07-21): satu berkas PDF = satu baris
// `documents`, diidentifikasi tautan unduhnya (unik) dan hash isinya. Sumber
// hanyalah tempat tautan itu ditemukan, bukan bagian identitas.
//
// Worker berkoordinasi lewat `SELECT ... FOR UPDATE SKIP LOCKED` — idiom
// antrian Postgres yang aman dipakai banyak goroutine (atau banyak proses)
// tanpa penguncian manual di sisi Go.
package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoWork: tidak ada baris yang bisa diambil saat ini. Worker
// memperlakukannya sebagai "tidur sebentar", bukan galat.
var ErrNoWork = errors.New("store: tidak ada pekerjaan tersedia")

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
	IDStr           string
	Code            string
	EndpointURL     string
	SourceType      string
	SourceConfigRaw []byte
}

func (s *Store) ListSources(ctx context.Context) ([]SourceRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, code, endpoint_url, source_type, source_config
		FROM sources ORDER BY code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceRow
	for rows.Next() {
		var r SourceRow
		if err := rows.Scan(&r.IDStr, &r.Code, &r.EndpointURL, &r.SourceType, &r.SourceConfigRaw); err != nil {
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
func (s *Store) RegisterURL(ctx context.Context, sourceID, downloadURL string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO documents (download_url, first_source_id, status)
		VALUES ($1, $2, 'pending')
		ON CONFLICT (download_url) DO NOTHING`, downloadURL, sourceID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ---- Tahap 1: unduh ----

type DownloadJob struct {
	ID          string
	DownloadURL string
	SourceID    string
}

func (s *Store) ClaimForDownload(ctx context.Context) (DownloadJob, error) {
	var j DownloadJob
	var srcID *string
	err := s.pool.QueryRow(ctx, `
		UPDATE documents SET status = 'downloading', updated_at = now()
		WHERE id = (
			SELECT id FROM documents WHERE status = 'pending'
			ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		RETURNING id::text, download_url, first_source_id::text`,
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
func (s *Store) MarkDownloaded(ctx context.Context, id, pdfPath, sha string, size int64) (bool, error) {
	var existing *string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text FROM documents
		WHERE pdf_sha256 = $1 AND id <> $2 AND status NOT IN ('rejected','duplicate')
		LIMIT 1`, sha, id).Scan(&existing)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}

	if existing != nil {
		_, err := s.pool.Exec(ctx, `
			UPDATE documents
			SET status = 'duplicate', reject_reason = 'duplikat_isi', duplicate_of = $2,
			    pdf_path = $3, pdf_sha256 = $4, file_size = $5,
			    downloaded_at = now(), updated_at = now()
			WHERE id = $1`, id, *existing, pdfPath, sha, size)
		return true, err
	}

	_, err = s.pool.Exec(ctx, `
		UPDATE documents
		SET status = 'downloaded', pdf_path = $2, pdf_sha256 = $3, file_size = $4,
		    downloaded_at = now(), updated_at = now(), attempts = 0, last_error = NULL
		WHERE id = $1`, id, pdfPath, sha, size)
	return false, err
}

func (s *Store) MarkDownloadFailed(ctx context.Context, id, errMsg string, maxAttempts int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents
		SET attempts = attempts + 1, last_error = $2,
		    status = CASE WHEN attempts + 1 >= $3 THEN 'failed' ELSE 'pending' END,
		    reject_reason = CASE WHEN attempts + 1 >= $3 THEN 'unduh_gagal' ELSE reject_reason END,
		    updated_at = now()
		WHERE id = $1`, id, errMsg, maxAttempts)
	return err
}

// ---- Tahap 2: OCR + perbaikan + klasifikasi ----

type OCRJob struct {
	ID      string
	PDFPath string
}

// ClaimForOCR mengambil dokumen yang sudah terunduh dan belum diproses.
func (s *Store) ClaimForOCR(ctx context.Context) (OCRJob, error) {
	var j OCRJob
	err := s.pool.QueryRow(ctx, `
		UPDATE documents SET status = 'processing', updated_at = now()
		WHERE id = (
			SELECT id FROM documents WHERE status = 'downloaded'
			ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		RETURNING id::text, pdf_path`,
	).Scan(&j.ID, &j.PDFPath)
	if errors.Is(err, pgx.ErrNoRows) {
		return OCRJob{}, ErrNoWork
	}
	return j, err
}

func (s *Store) MarkOCRFailed(ctx context.Context, id, errMsg string, maxAttempts int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents
		SET attempts = attempts + 1, last_error = $2,
		    status = CASE WHEN attempts + 1 >= $3 THEN 'failed' ELSE 'downloaded' END,
		    updated_at = now()
		WHERE id = $1`, id, errMsg, maxAttempts)
	return err
}

// SavePage menyimpan hasil OCR mentah satu halaman (fixed_text menyusul lewat
// SaveFixedText). ocr_text TIDAK PERNAH ditimpa oleh perbaikan model.
func (s *Store) SavePage(ctx context.Context, documentID string, page int, ocrText string,
	isEmpty, isTruncated bool, inkRatio, croppedPct float64, durationMS int, notes []string) error {
	notesJSON, _ := json.Marshal(notes)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO document_pages
			(document_id, page_number, ocr_text, is_empty, is_truncated,
			 ink_ratio, cropped_pct, duration_ms, notes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (document_id, page_number) DO UPDATE SET
			ocr_text = EXCLUDED.ocr_text, is_empty = EXCLUDED.is_empty,
			is_truncated = EXCLUDED.is_truncated, ink_ratio = EXCLUDED.ink_ratio,
			cropped_pct = EXCLUDED.cropped_pct, duration_ms = EXCLUDED.duration_ms,
			notes = EXCLUDED.notes`,
		documentID, page, ocrText, isEmpty, isTruncated, inkRatio, croppedPct, durationMS, notesJSON)
	return err
}

// SaveFixedText menyimpan hasil perbaikan model teks untuk satu halaman.
func (s *Store) SaveFixedText(ctx context.Context, documentID string, page int,
	fixedText string, opsCount int, promptHash string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE document_pages
		SET fixed_text = $3, fix_ops_count = $4, prompt_hash = $5
		WHERE document_id = $1 AND page_number = $2`,
		documentID, page, fixedText, opsCount, promptHash)
	return err
}

func (s *Store) HasPage(ctx context.Context, documentID string, page int) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM document_pages WHERE document_id = $1 AND page_number = $2)`,
		documentID, page).Scan(&ok)
	return ok, err
}

// GetPageText mengembalikan teks yang berlaku untuk satu halaman:
// koreksi manusia > perbaikan model > OCR mentah.
func (s *Store) GetPageText(ctx context.Context, documentID string, page int) (string, error) {
	var t string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(edited_text, fixed_text, ocr_text) FROM document_pages
		WHERE document_id = $1 AND page_number = $2`, documentID, page).Scan(&t)
	return t, err
}

func (s *Store) ReadPageRange(ctx context.Context, documentID string, a, b int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(edited_text, fixed_text, ocr_text) FROM document_pages
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
	Instansi    string
	Nomor       string
	Tahun       string
	Tentang     string
	Struktur    string
	Alasan      string
}

// RejectNotRegulation menandai dokumen bukan peraturan; sisa halaman tidak
// akan di-OCR.
func (s *Store) RejectNotRegulation(ctx context.Context, id, alasan string) error {
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
func (s *Store) ApplyMetaAndCheckDuplicate(ctx context.Context, id string, m DocMeta, canonicalKey string) (bool, error) {
	if canonicalKey != "" {
		var existing *string
		err := s.pool.QueryRow(ctx, `
			SELECT id::text FROM documents
			WHERE canonical_key = $1 AND id <> $2 AND status NOT IN ('rejected','duplicate')
			LIMIT 1`, canonicalKey, id).Scan(&existing)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
		if existing != nil {
			_, err := s.pool.Exec(ctx, `
				UPDATE documents
				SET status = 'duplicate', reject_reason = 'duplikat_peraturan', duplicate_of = $2,
				    is_peraturan = true, jenis = $3, instansi = $4, nomor = $5, tahun = $6,
				    tentang = $7, struktur = $8, canonical_key = $9,
				    classified_at = now(), updated_at = now()
				WHERE id = $1`, id, *existing, nullIfEmpty(m.Jenis), nullIfEmpty(m.Instansi),
				nullIfEmpty(m.Nomor), nullIfEmpty(m.Tahun), nullIfEmpty(m.Tentang),
				nullIfEmpty(m.Struktur), canonicalKey)
			return true, err
		}
	}

	_, err := s.pool.Exec(ctx, `
		UPDATE documents
		SET is_peraturan = true, jenis = $2, instansi = $3, nomor = $4, tahun = $5,
		    tentang = $6, struktur = $7, canonical_key = $8,
		    classified_at = now(), updated_at = now()
		WHERE id = $1`, id, nullIfEmpty(m.Jenis), nullIfEmpty(m.Instansi),
		nullIfEmpty(m.Nomor), nullIfEmpty(m.Tahun), nullIfEmpty(m.Tentang),
		nullIfEmpty(m.Struktur), nullIfEmpty(canonicalKey))
	return false, err
}

func (s *Store) MarkOCRDone(ctx context.Context, id string, totalPages int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents
		SET status = 'ocr_done', total_pages = $2, ocr_completed_at = now(), updated_at = now()
		WHERE id = $1`, id, totalPages)
	return err
}

// ---- Tahap 3: parse ----

type ParseJob struct {
	ID       string
	NumPages int
}

func (s *Store) ClaimForParse(ctx context.Context) (ParseJob, error) {
	var j ParseJob
	err := s.pool.QueryRow(ctx, `
		UPDATE documents SET status = 'parsing', updated_at = now()
		WHERE id = (
			SELECT id FROM documents WHERE status = 'ocr_done'
			ORDER BY ocr_completed_at LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		RETURNING id::text,
			(SELECT COUNT(*) FROM document_pages WHERE document_id = documents.id)`,
	).Scan(&j.ID, &j.NumPages)
	if errors.Is(err, pgx.ErrNoRows) {
		return ParseJob{}, ErrNoWork
	}
	return j, err
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
}

// ReplaceParseResult mengganti TOTAL nodes & snapshot dokumen ini dalam satu
// transaksi. Bukan riwayat: reparse berkali-kali berbiaya penyimpanan tetap.
func (s *Store) ReplaceParseResult(ctx context.Context, documentID, status string,
	report, extractionNotes []byte, nodes []NodeInsert) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `DELETE FROM nodes WHERE document_id = $1`, documentID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO parse_snapshots (document_id, status, report, extraction_notes, parsed_at)
		VALUES ($1,$2,$3,$4, now())
		ON CONFLICT (document_id) DO UPDATE SET
			status = EXCLUDED.status, report = EXCLUDED.report,
			extraction_notes = EXCLUDED.extraction_notes, parsed_at = now()`,
		documentID, status, report, extractionNotes); err != nil {
		return err
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
				original_node_type, warnings, citation
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
			RETURNING id`,
			documentID, parentID, n.Section, n.NodeType,
			n.BabNumber, n.BagianLabel, n.ParagrafLabel, n.PasalNumber, n.AyatNumber,
			n.HurufLabel, n.AngkaLabel, n.Label, n.Content,
			n.StartPage, n.EndPage, n.OrderIndex,
			n.OriginalNodeType, n.Warnings, n.Citation,
		).Scan(&newID); err != nil {
			return err
		}
		ids[i] = newID
	}

	if _, err := tx.Exec(ctx, `
		UPDATE documents SET status = 'parsed', parsed_at = now(), updated_at = now()
		WHERE id = $1`, documentID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) InsertRelation(ctx context.Context, documentID, relType, key, jenis,
	instansi, nomor, tahun, tentang, confidence, kutipan string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO relations (document_id, type, key, jenis, instansi, nomor, tahun,
		                        tentang, confidence, kutipan)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		documentID, relType, key, jenis, instansi, nomor, tahun, tentang, confidence, kutipan)
	return err
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
