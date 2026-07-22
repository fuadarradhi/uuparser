-- reset_parser.sql
--
-- TUJUAN: menghapus hasil parse (parse_snapshots + nodes) untuk dokumen yang
-- dipilih, lalu mengembalikan documents.status ke 'ocr_done' — SEMATA-MATA
-- itu, TIDAK menyentuh document_pages sama sekali, sehingga OCR TIDAK
-- diulang. Setelah script ini selesai, parserWorker yang sedang berjalan
-- otomatis mengambil dokumen itu lagi pada polling berikutnya (paling lama
-- 15 detik — lihat parserIdleInterval di internal/pipeline/parser_worker.go),
-- atau jalankan `./uuparser -once` bila service sedang tidak aktif. Tidak
-- ada langkah tambahan yang perlu dilakukan secara manual: ClaimForParse
-- memang mengambil SEMUA dokumen berstatus 'ocr_done', apa pun sebabnya
-- status itu didapat (baru selesai OCR, atau baru direset lewat script ini).
--
-- KENAPA SCRIPT INI ADA (2026-07-22): sejak keputusan sebelumnya, parser
-- didesain jalan SEKALI SAJA per dokumen (lihat komentar di atas
-- parse_snapshots pada schema.sql) — koreksi sesudahnya semestinya lewat
-- edit manusia langsung di tabel `nodes`, bukan reparse. Script ini adalah
-- JALAN KELUAR YANG DISENGAJA untuk satu skenario spesifik: kode parser-nya
-- SENDIRI yang diperbaiki (bug ditemukan lewat dokumen nyata — lihat
-- internal/parser/parse_batangtubuh.go, dokumen berstruktur Diktum
-- KESATU/KEDUA/dst kehilangan seluruh isinya), sehingga dokumen lama perlu
-- diuraikan ULANG dengan kode yang sudah benar, bukan dikoreksi manual.
--
-- PERINGATAN: bila dokumen yang direset SUDAH pernah dikoreksi manual di
-- tabel `nodes` (drag-drop/relabel lewat UI), koreksi itu IKUT TERHAPUS —
-- parser menulis ulang dari nol. Jangan reset dokumen yang sudah dikoreksi
-- manual kecuali memang bermaksud membuang koreksi tersebut.
--
-- CARA PAKAI: pilih SATU target di bawah (opsi A aktif secara default),
-- lalu jalankan seluruh file ini lewat psql, mis.:
--   psql "$DATABASE_URL" -f reset_parser.sql

BEGIN;

WITH target_ids AS (
    -- ============================================================
    -- OPSI A (AKTIF, DEFAULT) — reset semua dokumen berstruktur Diktum.
    -- Ini persis jenis dokumen yang kehilangan isinya (KESATU/KEDUA/dst)
    -- akibat bug parser sebelum perbaikan 2026-07-22. Aman dijalankan
    -- berkali-kali: dokumen yang belum pernah selesai di-parse (belum
    -- berstatus 'parsed') tidak ikut kena apa-apa.
    -- ============================================================
    SELECT id FROM documents
    WHERE struktur = 'diktum'
      AND status = 'parsed'

    -- ============================================================
    -- OPSI B — reset SEMUA dokumen yang hasil parse-nya berstatus FAIL,
    -- apa pun strukturnya (jangkauan lebih luas dari bug diktum saja).
    -- Untuk memakai: hapus/komentari blok OPSI A di atas, lalu hapus
    -- tanda komentar pada blok di bawah ini.
    -- ------------------------------------------------------------
    -- SELECT d.id FROM documents d
    -- JOIN parse_snapshots ps ON ps.document_id = d.id
    -- WHERE ps.status = 'FAIL'
    --   AND d.status = 'parsed'

    -- ============================================================
    -- OPSI C — reset dokumen TERTENTU saja lewat ID (ganti daftar angka
    -- di bawah dengan id dokumen yang dimaksud). Untuk memakai: hapus/
    -- komentari OPSI A di atas, lalu hapus tanda komentar di bawah ini.
    -- ------------------------------------------------------------
    -- SELECT unnest(ARRAY[123, 456, 789]::bigint[]) AS id
)
, hapus_nodes AS (
    DELETE FROM nodes
    WHERE document_id IN (SELECT id FROM target_ids)
    RETURNING document_id
)
, hapus_snapshot AS (
    DELETE FROM parse_snapshots
    WHERE document_id IN (SELECT id FROM target_ids)
    RETURNING document_id
)
UPDATE documents
SET status     = 'ocr_done',
    parsed_at  = NULL,
    updated_at = now()
WHERE id IN (SELECT id FROM target_ids);

COMMIT;

-- Ringkasan setelah reset (jalankan terpisah, di luar transaksi di atas,
-- untuk memastikan berapa dokumen yang baru saja dikembalikan ke antrian):
--
--   SELECT count(*) AS menunggu_parse_ulang
--   FROM documents
--   WHERE status = 'ocr_done' AND struktur = 'diktum';
