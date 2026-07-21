-- uuparser — skema PostgreSQL (versi dokumen-sentris, 2026-07-21)
--
-- Dijalankan MANUAL oleh user. Jalankan sekali di database kosong; skema ini
-- MENGGANTI versi sebelumnya (hapus dulu yang lama).
--
-- Perubahan mendasar dari versi sebelumnya: DOKUMEN adalah pusatnya, bukan
-- sumber. Satu berkas PDF = satu baris `documents`, diidentifikasi dari URL
-- unduhnya (unik) dan hash isinya. Dari sumber mana ia ditemukan hanyalah
-- jejak (`documents.first_source_id`), bukan bagian identitas — dokumen yang
-- sama muncul di banyak JDIH tetap satu baris.

CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- =====================================================================
-- sources — daftar endpoint yang dijelajahi. Perannya kini kecil: hanya
-- tempat mengambil daftar tautan. Tidak ada lagi pemeriksaan jurisdiksi
-- (semua sumber dianggap tepercaya; yang diperiksa cuma "ini peraturan
-- atau bukan", oleh model teks).
-- =====================================================================
CREATE TABLE IF NOT EXISTS sources (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code           text NOT NULL UNIQUE,
    endpoint_url   text NOT NULL,
    source_type    text NOT NULL DEFAULT 'integrasi',  -- 'integrasi' | 'scrape' | ...
    source_config  jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- =====================================================================
-- documents — satu baris per BERKAS. download_url unik: tautan yang sudah
-- pernah didaftarkan tidak masuk dua kali, dari sumber mana pun.
-- =====================================================================
CREATE TABLE IF NOT EXISTS documents (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Identitas berkas.
    download_url      text NOT NULL UNIQUE,   -- sudah dinormalisasi (lihat downloader.NormalizeURL)
    pdf_path          text,                   -- semua PDF di SATU folder
    pdf_sha256        text,                   -- isi identik = dokumen identik
    file_size         bigint,

    -- Jejak asal (informasi saja, bukan identitas).
    first_source_id   uuid REFERENCES sources(id) ON DELETE SET NULL,

    -- Status pipeline.
    status            text NOT NULL DEFAULT 'pending',
    attempts          int NOT NULL DEFAULT 0,
    last_error        text,
    reject_reason     text,     -- 'bukan_peraturan' | 'duplikat' | 'unduh_gagal' | ...
    duplicate_of      uuid REFERENCES documents(id) ON DELETE SET NULL,

    -- Metadata hasil pembacaan model teks atas halaman 1 (bukan dari sumber).
    is_peraturan      boolean,
    jenis             text,
    instansi          text,
    nomor             text,
    tahun             text,
    tentang           text,
    struktur          text,     -- 'pasal_ayat' | 'diktum' | 'unknown'

    -- Kunci kanonik untuk deteksi duplikat berbasis identitas peraturan,
    -- diisi kode dari metadata di atas: jenis|instansi|nomor|tahun.
    canonical_key     text,

    total_pages       int,
    downloaded_at     timestamptz,
    classified_at     timestamptz,
    ocr_completed_at  timestamptz,
    parsed_at         timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_documents_status ON documents (status);
CREATE INDEX IF NOT EXISTS idx_documents_sha ON documents (pdf_sha256);
CREATE INDEX IF NOT EXISTS idx_documents_canonical ON documents (canonical_key);

-- =====================================================================
-- document_pages — hasil OCR per halaman DAN hasil perbaikan model teks.
--   ocr_text    : mentah dari model visi, TIDAK PERNAH ditimpa
--   fixed_text  : hasil perbaikan model teks (salah ketik/struktur)
--   edited_text : koreksi manusia lewat UI
-- Parser membaca COALESCE(edited_text, fixed_text, ocr_text).
-- Diff antara ocr_text dan fixed_text TIDAK disimpan — dihitung saat
-- dibutuhkan oleh UI (keputusan 2026-07-21: data turunan cepat basi).
-- =====================================================================
CREATE TABLE IF NOT EXISTS document_pages (
    id             bigserial PRIMARY KEY,
    document_id    uuid NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    page_number    int NOT NULL,

    ocr_text       text NOT NULL DEFAULT '',
    fixed_text     text,
    edited_text    text,

    is_edited      boolean NOT NULL DEFAULT false,
    edited_at      timestamptz,

    is_empty       boolean NOT NULL DEFAULT false,
    is_truncated   boolean NOT NULL DEFAULT false,
    ink_ratio      real,
    cropped_pct    real,
    duration_ms    int,
    fix_ops_count  int,      -- berapa banyak potongan teks yang diubah model
    prompt_hash    text,     -- versi prompt perbaikan yang dipakai
    notes          jsonb NOT NULL DEFAULT '[]'::jsonb,

    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (document_id, page_number)
);

CREATE INDEX IF NOT EXISTS idx_pages_doc ON document_pages (document_id, page_number);

-- =====================================================================
-- parse_snapshots — SATU baris per dokumen (upsert), bukan riwayat.
-- =====================================================================
CREATE TABLE IF NOT EXISTS parse_snapshots (
    document_id       uuid PRIMARY KEY REFERENCES documents(id) ON DELETE CASCADE,
    status            text NOT NULL,
    report            jsonb NOT NULL DEFAULT '{}'::jsonb,
    extraction_notes  jsonb NOT NULL DEFAULT '[]'::jsonb,
    parsed_at         timestamptz NOT NULL DEFAULT now()
);

-- =====================================================================
-- nodes — hasil parse siap-edit (drag-drop/relabel di UI).
-- =====================================================================
CREATE TABLE IF NOT EXISTS nodes (
    id                 bigserial PRIMARY KEY,
    document_id        uuid NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    parent_id          bigint REFERENCES nodes(id) ON DELETE CASCADE,

    section            text NOT NULL,
    node_type          text NOT NULL,   -- bab|bagian|paragraf|pasal|ayat|judul|item|penetapan|paragraf_isi|catatan

    bab_number         text,
    bagian_label       text,
    paragraf_label     text,
    pasal_number       text,
    ayat_number        text,
    huruf_label        text,
    angka_label        text,

    label              text,            -- sumber kebenaran label level ini (dipakai trigger)
    content            text NOT NULL DEFAULT '',

    start_page         int NOT NULL DEFAULT 0,
    end_page           int NOT NULL DEFAULT 0,
    order_index        bigint NOT NULL,

    original_node_type text,
    is_edited          boolean NOT NULL DEFAULT false,
    edited_at          timestamptz,
    warnings           jsonb NOT NULL DEFAULT '[]'::jsonb,
    citation           text,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_nodes_doc_order ON nodes (document_id, order_index);
CREATE INDEX IF NOT EXISTS idx_nodes_doc_pasal_ayat ON nodes (document_id, pasal_number, ayat_number);
CREATE INDEX IF NOT EXISTS idx_nodes_parent ON nodes (parent_id);
CREATE INDEX IF NOT EXISTS idx_nodes_doc_page ON nodes (document_id, start_page);

ALTER TABLE nodes ADD COLUMN IF NOT EXISTS content_tsv tsvector
    GENERATED ALWAYS AS (to_tsvector('indonesian', coalesce(content, ''))) STORED;
CREATE INDEX IF NOT EXISTS idx_nodes_content_tsv ON nodes USING GIN (content_tsv);

-- =====================================================================
-- Trigger label flat (bab_number/bagian_label/paragraf_label/pasal_number/
-- ayat_number) — memindahkan satu simpul lewat parent_id otomatis
-- memperbarui SELURUH keturunannya (anak, cucu, cicit) secara rekursif.
-- =====================================================================
CREATE OR REPLACE FUNCTION nodes_recompute_own_labels() RETURNS trigger AS $$
DECLARE
    p RECORD;
BEGIN
    IF NEW.parent_id IS NULL THEN
        NEW.bab_number := NULL; NEW.bagian_label := NULL;
        NEW.paragraf_label := NULL; NEW.pasal_number := NULL;
    ELSE
        SELECT bab_number, bagian_label, paragraf_label, pasal_number
          INTO p FROM nodes WHERE id = NEW.parent_id;
        NEW.bab_number := p.bab_number;
        NEW.bagian_label := p.bagian_label;
        NEW.paragraf_label := p.paragraf_label;
        NEW.pasal_number := p.pasal_number;
    END IF;

    CASE NEW.node_type
        WHEN 'bab' THEN
            NEW.bab_number := NEW.label;
            NEW.bagian_label := NULL; NEW.paragraf_label := NULL; NEW.pasal_number := NULL;
        WHEN 'bagian' THEN
            NEW.bagian_label := NEW.label;
            NEW.paragraf_label := NULL; NEW.pasal_number := NULL;
        WHEN 'paragraf' THEN
            NEW.paragraf_label := NEW.label;
            NEW.pasal_number := NULL;
        WHEN 'pasal' THEN
            NEW.pasal_number := NEW.label;
        WHEN 'ayat' THEN
            NEW.ayat_number := NEW.label;
        ELSE NULL;
    END CASE;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_nodes_recompute_own
    BEFORE INSERT OR UPDATE OF parent_id, label, node_type ON nodes
    FOR EACH ROW EXECUTE FUNCTION nodes_recompute_own_labels();

CREATE OR REPLACE FUNCTION nodes_cascade_to_descendants() RETURNS trigger AS $$
BEGIN
    IF (TG_OP = 'UPDATE' AND NEW.parent_id IS NOT DISTINCT FROM OLD.parent_id
        AND NEW.bab_number IS NOT DISTINCT FROM OLD.bab_number
        AND NEW.bagian_label IS NOT DISTINCT FROM OLD.bagian_label
        AND NEW.paragraf_label IS NOT DISTINCT FROM OLD.paragraf_label
        AND NEW.pasal_number IS NOT DISTINCT FROM OLD.pasal_number) THEN
        RETURN NEW;
    END IF;

    WITH RECURSIVE tree AS (
        SELECT id, node_type, label, bab_number, bagian_label, paragraf_label, pasal_number
        FROM nodes WHERE id = NEW.id
        UNION ALL
        SELECT n.id, n.node_type, n.label,
               CASE WHEN n.node_type = 'bab' THEN n.bab_number ELSE t.bab_number END,
               CASE WHEN n.node_type IN ('bab') THEN NULL
                    WHEN n.node_type = 'bagian' THEN n.bagian_label
                    ELSE t.bagian_label END,
               CASE WHEN n.node_type IN ('bab','bagian') THEN NULL
                    WHEN n.node_type = 'paragraf' THEN n.paragraf_label
                    ELSE t.paragraf_label END,
               CASE WHEN n.node_type IN ('bab','bagian','paragraf') THEN NULL
                    WHEN n.node_type = 'pasal' THEN n.pasal_number
                    ELSE t.pasal_number END
        FROM nodes n JOIN tree t ON n.parent_id = t.id
    )
    UPDATE nodes SET
        bab_number = tree.bab_number,
        bagian_label = tree.bagian_label,
        paragraf_label = tree.paragraf_label,
        pasal_number = tree.pasal_number
    FROM tree
    WHERE nodes.id = tree.id AND tree.id <> NEW.id;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_nodes_cascade_descendants
    AFTER UPDATE OF parent_id, bab_number, bagian_label, paragraf_label, pasal_number ON nodes
    FOR EACH ROW EXECUTE FUNCTION nodes_cascade_to_descendants();

-- =====================================================================
-- node_embeddings — terpisah dari nodes agar ganti model embedding tidak
-- memaksa migrasi tabel utama. Dimensi 1024 = keluaran dense bge-m3.
-- =====================================================================
CREATE TABLE IF NOT EXISTS node_embeddings (
    node_id     bigint NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    model       text NOT NULL DEFAULT 'bge-m3',
    embedding   vector(1024),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, model)
);

-- =====================================================================
-- relations — relasi antar-peraturan (mencabut/mengubah/dasar_hukum/disebut).
-- =====================================================================
CREATE TABLE IF NOT EXISTS relations (
    id                 bigserial PRIMARY KEY,
    document_id        uuid NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    type               text NOT NULL,
    key                text,
    jenis              text,
    instansi           text,
    nomor              text,
    tahun              text,
    tentang            text,
    confidence         text,
    kutipan            text,
    target_document_id uuid REFERENCES documents(id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_relations_document ON relations (document_id);
CREATE INDEX IF NOT EXISTS idx_relations_key ON relations (key);
CREATE INDEX IF NOT EXISTS idx_relations_target ON relations (target_document_id);
