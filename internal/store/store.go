// Package store adalah satu-satunya titik akses ke Postgres. Menggantikan
// deteksi "sudah selesai" berbasis file-existence yang lama (lihat CATATAN-
// MIGRASI.md) — sekarang semua status hidup di kolom `documents.status`,
// dan tiga worker (downloader/OCR/parser di main.go) berkoordinasi lewat
// `SELECT ... FOR UPDATE SKIP LOCKED`, idiom antrian standar Postgres yang
// aman dipakai bareng banyak goroutine (atau nanti banyak instance proses)
// tanpa locking manual di sisi Go.
//
// VERIFIKASI: package ini TIDAK bisa di-compile di sandbox pembuatnya —
// modul pgx/v5 versi terbaru butuh Go >= 1.25 dan sebagian dependency
// transitifnya (gopkg.in/yaml.v3) diblokir oleh allowlist jaringan sandbox
// tersebut. Kode ini ditulis mengikuti API pgx/v5 yang terdokumentasi, tapi
// BELUM PERNAH di-build sungguhan — jalankan `go mod tidy && go build ./...`
// dan kirim galat kompilasi apa pun; perbaikannya kemungkinan besar terbatas
// di berkas ini saja. Pola yang sama persis dengan CATATAN-BUILD.md untuk
// localllm/yzma.
package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoWork dikembalikan oleh method Claim* ketika tidak ada baris yang bisa
// diambil saat ini — caller (worker loop) memperlakukan ini sebagai "tidur
// sebentar, coba lagi", bukan error fatal.
var ErrNoWork = errors.New("store: tidak ada pekerjaan tersedia")

type Store struct {
	pool *pgxpool.Pool
}

// Open membuka connection pool ke Postgres. databaseURL wajib diisi (lihat
// internal/config).
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
	IDStr             string
	Code              string
	EndpointURL       string
	SourceType        string
	SourceConfigRaw   []byte
	JurisdictionLevel string
	InstansiName      string
}

// GetSource mengambil satu baris sources by id — dipakai downloader worker
// saat memproses satu DownloadJob untuk tahu source_type/endpoint/code-nya.
func (s *Store) GetSource(ctx context.Context, id string) (SourceRow, error) {
	var r SourceRow
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, code, endpoint_url, source_type, source_config,
		       jurisdiction_level, instansi_name
		FROM sources WHERE id = $1`, id,
	).Scan(&r.IDStr, &r.Code, &r.EndpointURL, &r.SourceType, &r.SourceConfigRaw,
		&r.JurisdictionLevel, &r.InstansiName)
	return r, err
}

// ListSources mengembalikan semua sumber terdaftar — dipanggil sekali saat
// start untuk membangun satu downloader.Source per baris.
func (s *Store) ListSources(ctx context.Context) ([]SourceRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, code, endpoint_url, source_type, source_config,
		       jurisdiction_level, instansi_name
		FROM sources ORDER BY code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SourceRow
	for rows.Next() {
		var r SourceRow
		if err := rows.Scan(&r.IDStr, &r.Code, &r.EndpointURL, &r.SourceType,
			&r.SourceConfigRaw, &r.JurisdictionLevel, &r.InstansiName); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- Dokumen: insert dari hasil ListDocuments() sumber ----

// UpsertDocumentMeta mendaftarkan (atau mengabaikan bila sudah ada) satu
// dokumen hasil downloader.Source.ListDocuments. Status awal selalu
// 'pending' — tidak menimpa dokumen yang sudah diproses (ON CONFLICT DO
// NOTHING), sehingga aman dipanggil berulang tiap siklus tanpa mereset
// progres yang sudah ada.
func (s *Store) UpsertDocumentMeta(ctx context.Context, sourceID, idData, judul, slug, fileURL string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO documents (source_id, id_data, judul, slug, pdf_url, status)
		VALUES ($1, $2, $3, $4, $5, 'pending')
		ON CONFLICT (source_id, id_data) DO NOTHING`,
		sourceID, idData, judul, slug, fileURL)
	return err
}

// ---- Worker 1: downloader ----

type DownloadJob struct {
	ID       string
	SourceID string
	PDFURL   string
	Slug     string
}

// ClaimForDownload mengambil SATU dokumen berstatus 'pending' dan
// menandainya 'downloading' dalam satu transaksi (SKIP LOCKED mencegah dua
// goroutine/proses mengambil baris yang sama).
func (s *Store) ClaimForDownload(ctx context.Context) (DownloadJob, error) {
	var job DownloadJob
	err := s.pool.QueryRow(ctx, `
		UPDATE documents SET status = 'downloading', updated_at = now()
		WHERE id = (
			SELECT id FROM documents
			WHERE status = 'pending'
			ORDER BY created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id::text, source_id::text, pdf_url, slug`,
	).Scan(&job.ID, &job.SourceID, &job.PDFURL, &job.Slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return DownloadJob{}, ErrNoWork
	}
	return job, err
}

// MarkDownloaded menandai dokumen sudah terunduh, siap untuk gate/OCR.
func (s *Store) MarkDownloaded(ctx context.Context, id, pdfPath, sha256 string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents
		SET status = 'downloaded', pdf_path = $2, pdf_sha256 = $3,
		    downloaded_at = now(), updated_at = now(), attempts = 0, last_error = NULL
		WHERE id = $1`, id, pdfPath, sha256)
	return err
}

// MarkDownloadFailed mencatat kegagalan unduh dan mengembalikan status ke
// 'pending' (akan dicoba lagi siklus berikutnya) kecuali sudah melewati
// MAX_ATTEMPTS, dalam hal ini pindah ke 'failed'.
func (s *Store) MarkDownloadFailed(ctx context.Context, id string, errMsg string, maxAttempts int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents
		SET attempts = attempts + 1,
		    last_error = $2,
		    status = CASE WHEN attempts + 1 >= $3 THEN 'failed' ELSE 'pending' END,
		    updated_at = now()
		WHERE id = $1`, id, errMsg, maxAttempts)
	return err
}

// ---- Worker 2: probe (header halaman 1) + OCR ----

type OCRJob struct {
	ID                string
	PDFPath           string
	SourceInstansi    string
}

// ClaimForOCR mengambil dokumen berstatus 'downloaded' (belum pernah
// diprobe/OCR sama sekali). Ikut mengembalikan instansi_name sumbernya
// (JOIN ke sources) supaya pemanggil bisa menjalankan
// parser.MatchesJurisdiction — tanpa ini gate jurisdiksi tidak bisa jalan.
func (s *Store) ClaimForOCR(ctx context.Context) (OCRJob, error) {
	var job OCRJob
	err := s.pool.QueryRow(ctx, `
		UPDATE documents SET status = 'probing', ocr_started_at = now(), updated_at = now()
		WHERE id = (
			SELECT id FROM documents
			WHERE status = 'downloaded'
			ORDER BY created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id::text, pdf_path,
			(SELECT s.instansi_name FROM sources s WHERE s.id = documents.source_id)`,
	).Scan(&job.ID, &job.PDFPath, &job.SourceInstansi)
	if errors.Is(err, pgx.ErrNoRows) {
		return OCRJob{}, ErrNoWork
	}
	return job, err
}

// SavePage menyimpan hasil OCR satu halaman (upsert per page_number, sehingga
// resume per-halaman tetap berfungsi seperti versi berbasis-file yang lama).
func (s *Store) SavePage(ctx context.Context, documentID string, pageNumber int, ocrText string, isEmpty, isTruncated bool, inkRatio, croppedPct float64, notes []string, durationMS int) error {
	notesJSON, _ := json.Marshal(notes)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO document_pages
			(document_id, page_number, ocr_text, is_empty, is_truncated, ink_ratio, cropped_pct, notes, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (document_id, page_number) DO UPDATE SET
			ocr_text = EXCLUDED.ocr_text,
			is_empty = EXCLUDED.is_empty,
			is_truncated = EXCLUDED.is_truncated,
			ink_ratio = EXCLUDED.ink_ratio,
			cropped_pct = EXCLUDED.cropped_pct,
			notes = EXCLUDED.notes,
			duration_ms = EXCLUDED.duration_ms`,
		documentID, pageNumber, ocrText, isEmpty, isTruncated, inkRatio, croppedPct, notesJSON, durationMS)
	return err
}

// HasPage melapor apakah halaman ini sudah punya hasil OCR tersimpan —
// dasar resume per-halaman (menggantikan cek file-existence lama).
func (s *Store) HasPage(ctx context.Context, documentID string, pageNumber int) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM document_pages WHERE document_id = $1 AND page_number = $2)`,
		documentID, pageNumber,
	).Scan(&exists)
	return exists, err
}

// ReadPageRange membaca teks halaman [a,b] (inklusif, 1-based) yang SUDAH
// tersimpan, terurut, memakai COALESCE(edited_text, ocr_text) seperti
// GetPageText. Halaman yang belum ada dilewati (bukan error) — sama seperti
// perilaku readPageRange berbasis-file yang lama.
func (s *Store) ReadPageRange(ctx context.Context, documentID string, a, b int) ([]string, error) {
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

// GetPageText mengembalikan teks yang HARUS dipakai parser untuk satu
// halaman: edited_text bila user sudah mengoreksi, kalau tidak ocr_text
// mentah.
func (s *Store) GetPageText(ctx context.Context, documentID string, pageNumber int) (string, error) {
	var text string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(edited_text, ocr_text) FROM document_pages
		WHERE document_id = $1 AND page_number = $2`, documentID, pageNumber,
	).Scan(&text)
	return text, err
}

// ApplyHeaderResult menyimpan hasil pemeriksaan header halaman-1
// (internal/parser.ExtractHeader + MatchesJurisdiction) dan memutuskan
// status berikutnya:
//   - bukan peraturan sama sekali           -> 'rejected'
//   - instansi tak cocok jurisdiksi sumber  -> 'rejected' (reason=wrong_jurisdiction)
//   - dokumen sebelum UU 12/2011            -> 'review_manual'
//   - lolos semua                           -> 'ocr_in_progress' (lanjut ke hal. 2 dst)
func (s *Store) ApplyHeaderResult(ctx context.Context, id string, isRegulation bool, rejectReason, structureType, extractedInstansi string, preUU122011 bool) error {
	status := "ocr_in_progress"
	switch {
	case !isRegulation:
		status = "rejected"
	case rejectReason == "wrong_jurisdiction":
		status = "rejected"
	case preUU122011:
		status = "review_manual"
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE documents
		SET is_regulation = $2, reject_reason = $3, structure_type = $4,
		    extracted_instansi = $5, status = $6, updated_at = now()
		WHERE id = $1`, id, isRegulation, nullIfEmpty(rejectReason), structureType,
		nullIfEmpty(extractedInstansi), status)
	return err
}

// MarkOCRFailed mencatat kegagalan OCR dan mengembalikan status ke
// 'downloaded' (BUKAN 'pending' — PDF-nya sudah ada, jangan diunduh ulang)
// supaya dicoba OCR lagi di putaran berikutnya, kecuali sudah melewati batas
// percobaan → 'failed'.
func (s *Store) MarkOCRFailed(ctx context.Context, id string, errMsg string, maxAttempts int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents
		SET attempts = attempts + 1,
		    last_error = $2,
		    status = CASE WHEN attempts + 1 >= $3 THEN 'failed' ELSE 'downloaded' END,
		    updated_at = now()
		WHERE id = $1`, id, errMsg, maxAttempts)
	return err
}

// MarkOCRDone menandai dokumen selesai di-OCR seluruhnya, siap diparse.
func (s *Store) MarkOCRDone(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents SET status = 'ocr_done', ocr_completed_at = now(), updated_at = now()
		WHERE id = $1`, id)
	return err
}

// ---- Approve/reject dokumen berstatus review_manual ----
// Dipakai oleh flag CLI -approve-id/-reject-id (main.go) — nanti UI web
// tinggal jalankan UPDATE yang sama persis lewat SQL langsung.

func (s *Store) ApproveDocument(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents SET status = 'ocr_in_progress', updated_at = now()
		WHERE id = $1 AND status = 'review_manual'`, id)
	return err
}

func (s *Store) RejectDocument(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents SET status = 'rejected', reject_reason = 'manual_reject', updated_at = now()
		WHERE id = $1 AND status = 'review_manual'`, id)
	return err
}

// ResetDocument mengembalikan dokumen ke 'pending' dan menghapus jejak
// percobaan sebelumnya — pengganti "hapus failed/<slug>.json" versi lama.
func (s *Store) ResetDocument(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE documents
		SET status = 'pending', attempts = 0, last_error = NULL, updated_at = now()
		WHERE id = $1`, id)
	return err
}

// ---- Worker 3: parser ----

type ParseJob struct {
	ID       string
	NumPages int
}

// ClaimForParse mengambil dokumen yang SIAP diparse: baru selesai OCR
// ('ocr_done') ATAU eksplisit diminta reparse (reparse_requested_at diisi
// user setelah mengoreksi teks OCR). Reparse TIDAK menunggu status
// tertentu — bisa menimpa dokumen yang statusnya sudah 'parsed'/'reviewed'
// sekalipun, karena ini permintaan eksplisit user (sudah diperingatkan lewat
// UI bahwa reparse mereset editan manual).
func (s *Store) ClaimForParse(ctx context.Context) (ParseJob, error) {
	var job ParseJob
	err := s.pool.QueryRow(ctx, `
		UPDATE documents SET status = 'parsing', reparse_requested_at = NULL, updated_at = now()
		WHERE id = (
			SELECT d.id FROM documents d
			WHERE d.status = 'ocr_done' OR d.reparse_requested_at IS NOT NULL
			ORDER BY COALESCE(d.reparse_requested_at, d.ocr_completed_at)
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id::text,
			(SELECT COUNT(*) FROM document_pages WHERE document_id = documents.id)`,
	).Scan(&job.ID, &job.NumPages)
	if errors.Is(err, pgx.ErrNoRows) {
		return ParseJob{}, ErrNoWork
	}
	return job, err
}

// NodeInsert adalah satu baris siap-insert ke `nodes` (dipetakan dari
// parser.Node oleh main.go — lihat CATATAN-MIGRASI.md untuk pemetaan field).
type NodeInsert struct {
	ParentIdx        int // indeks ke NodeInsert lain dalam slice yang sama, -1 bila root
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
	Warnings         []byte // JSON
	Citation         *string
}

// ReplaceParseResult MENGHAPUS TOTAL nodes/parse_snapshot lama untuk
// dokumen ini lalu menulis hasil parse baru, semuanya dalam SATU transaksi.
// Ini SENGAJA bukan append-only history — lihat CATATAN-MIGRASI.md bagian
// "kenapa parse_snapshots tidak punya banyak generasi": reparse yang sering
// diklik tidak boleh membuat tabel membengkak, dan editan manual sudah
// diterima sebagai sesuatu yang boleh hilang saat reparse (dengan peringatan
// di UI), jadi tidak ada gunanya menahan versi lama hasil parser otomatis.
func (s *Store) ReplaceParseResult(ctx context.Context, documentID string, status string, report, extractionNotes []byte, nodes []NodeInsert) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op bila sudah Commit

	if _, err := tx.Exec(ctx, `DELETE FROM nodes WHERE document_id = $1`, documentID); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO parse_snapshots (document_id, status, report, extraction_notes, parsed_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (document_id) DO UPDATE SET
			status = EXCLUDED.status, report = EXCLUDED.report,
			extraction_notes = EXCLUDED.extraction_notes, parsed_at = now()`,
		documentID, status, report, extractionNotes); err != nil {
		return err
	}

	// insert berurutan sesuai index di slice supaya ParentIdx bisa dipetakan
	// ke id bigserial yang baru terbit.
	ids := make([]int64, len(nodes))
	for i, n := range nodes {
		var parentID *int64
		if n.ParentIdx >= 0 {
			parentID = &ids[n.ParentIdx]
		}
		var newID int64
		err := tx.QueryRow(ctx, `
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
		).Scan(&newID)
		if err != nil {
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

// ---- Relasi antar-peraturan ----

func (s *Store) InsertRelation(ctx context.Context, documentID, relType, key, jenis, instansi, nomor, tahun, tentang, confidence, kutipan string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO relations (document_id, type, key, jenis, instansi, nomor, tahun, tentang, confidence, kutipan)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		documentID, relType, key, jenis, instansi, nomor, tahun, tentang, confidence, kutipan)
	return err
}

// ---- util ----

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
