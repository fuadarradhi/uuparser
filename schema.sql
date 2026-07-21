-- uuparser — skema PostgreSQL
--
-- Dijalankan MANUAL oleh user (tidak ada migration tool). Idempotent lewat
-- IF NOT EXISTS di mana masuk akal, tapi ini bukan pengganti migration tool —
-- jalankan sekali di database kosong. Butuh ekstensi pgvector untuk kolom
-- embedding (lihat node_embeddings di bawah).
--
-- Urutan tabel sengaja mengikuti alur pipeline: sources -> documents ->
-- document_pages -> parse_snapshots -> nodes -> node_embeddings -> relations.

CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto; -- untuk gen_random_uuid()

-- =====================================================================
-- sources — satu baris per endpoint JDIH yang dipantau.
-- =====================================================================
CREATE TABLE IF NOT EXISTS sources (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code                text NOT NULL UNIQUE,        -- slug, mis. 'acehbarat' (dari hostname)
    endpoint_url        text NOT NULL,
    hostname            text NOT NULL,

    -- Sumber bisa API /integrasi standar, API custom, atau perlu di-scrape.
    -- Menentukan implementasi downloader.Source mana yang dipakai untuk
    -- source ini — lihat internal/downloader/source.go.
    source_type         text NOT NULL DEFAULT 'integrasi',  -- 'integrasi' | 'scrape' | dst
    source_config       jsonb NOT NULL DEFAULT '{}'::jsonb, -- parameter spesifik per source_type

    -- Identitas jurisdiksi source ini sendiri — dipakai header.go untuk
    -- memvalidasi bahwa dokumen yang diproses memang produk instansi INI,
    -- bukan produk daerah/nasional lain yang kebetulan ikut terindeks.
    jurisdiction_level  text NOT NULL DEFAULT 'kabupaten',  -- 'nasional' | 'provinsi' | 'kabupaten' | 'kota'
    instansi_name       text NOT NULL,                      -- mis. 'Kabupaten Aceh Barat', 'Pemerintah Aceh'

    created_at          timestamptz NOT NULL DEFAULT now()
);

-- =====================================================================
-- documents — satu baris per dokumen dari /integrasi (atau hasil scrape).
-- Kolom `status` MENGGANTIKAN deteksi file-existence yang lama.
-- =====================================================================
CREATE TABLE IF NOT EXISTS documents (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id               uuid NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    id_data                 text NOT NULL,          -- idData dari JDIH, unik per source
    judul                   text NOT NULL,
    slug                    text NOT NULL,
    pdf_url                 text,
    pdf_path                text,                   -- path lokal setelah diunduh
    pdf_sha256              text,

    -- state machine pipeline, lihat README bagian "Alur status dokumen".
    status                  text NOT NULL DEFAULT 'pending',
    attempts                int NOT NULL DEFAULT 0,
    last_error              text,

    -- hasil klasifikasi page-1 header check (lihat internal/parser/header.go).
    is_regulation           boolean,                -- null = belum diprobe
    reject_reason           text,                   -- 'no_legal_signal' | 'wrong_jurisdiction' | dst
    structure_type          text,                   -- 'pasal_ayat' | 'diktum' | 'unknown'
    extracted_instansi      text,                   -- instansi yang terbaca dari header hal.1

    -- reparse: diisi user (lewat UI/SQL) untuk memicu parser worker memproses
    -- ulang dokumen ini memakai document_pages.edited_text terbaru.
    reparse_requested_at    timestamptz,

    -- validitas regulasi (untuk RAG: jangan kutip peraturan yang sudah dicabut
    -- tanpa bilang begitu). target_document_id di relations diisi belakangan.
    status_berlaku          text NOT NULL DEFAULT 'berlaku', -- 'berlaku' | 'dicabut' | 'diubah_sebagian'
    tanggal_penetapan       date,
    tanggal_pengundangan    date,

    downloaded_at           timestamptz,
    ocr_started_at           timestamptz,
    ocr_completed_at         timestamptz,
    parsed_at                timestamptz,

    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),

    UNIQUE (source_id, id_data)
);

CREATE INDEX IF NOT EXISTS idx_documents_status ON documents (status);
CREATE INDEX IF NOT EXISTS idx_documents_source_status ON documents (source_id, status);
CREATE INDEX IF NOT EXISTS idx_documents_reparse ON documents (reparse_requested_at)
    WHERE reparse_requested_at IS NOT NULL;

-- =====================================================================
-- document_pages — hasil OCR per halaman. edited_text menyimpan koreksi
-- manual TANPA menimpa ocr_text mentah (jejak audit tetap ada).
-- =====================================================================
CREATE TABLE IF NOT EXISTS document_pages (
    id              bigserial PRIMARY KEY,
    document_id     uuid NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    page_number     int NOT NULL,

    ocr_text        text NOT NULL DEFAULT '',   -- TIDAK PERNAH ditimpa
    edited_text     text,                       -- null = belum dikoreksi user
    is_edited       boolean NOT NULL DEFAULT false,
    edited_at       timestamptz,

    is_empty        boolean NOT NULL DEFAULT false,
    is_truncated    boolean NOT NULL DEFAULT false,
    ink_ratio       real,
    cropped_pct     real,
    notes           jsonb NOT NULL DEFAULT '[]'::jsonb,  -- pengganti _page_notes.json
    duration_ms     int,

    created_at      timestamptz NOT NULL DEFAULT now(),

    UNIQUE (document_id, page_number)
);

CREATE INDEX IF NOT EXISTS idx_document_pages_doc ON document_pages (document_id, page_number);

-- =====================================================================
-- parse_snapshots — SATU baris per dokumen (bukan log). Reparse meng-UPSERT,
-- bukan menambah histori — lihat catatan di README soal ini.
-- =====================================================================
CREATE TABLE IF NOT EXISTS parse_snapshots (
    document_id         uuid PRIMARY KEY REFERENCES documents(id) ON DELETE CASCADE,
    status               text NOT NULL,             -- SUCCESS | WARNING | FAIL
    report               jsonb NOT NULL DEFAULT '{}'::jsonb,       -- Report dari internal/parser (issues + stats)
    extraction_notes     jsonb NOT NULL DEFAULT '[]'::jsonb,
    parsed_at            timestamptz NOT NULL DEFAULT now()
);

-- =====================================================================
-- nodes — hasil parse siap-edit (drag-drop/relabel di web). DIHAPUS &
-- DIISI ULANG TOTAL tiap (re)parse dalam satu transaksi — lihat README.
-- =====================================================================
CREATE TABLE IF NOT EXISTS nodes (
    id                  bigserial PRIMARY KEY,
    document_id         uuid NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    parent_id            bigint REFERENCES nodes(id) ON DELETE CASCADE,

    section              text NOT NULL,   -- 'judul' | 'menimbang' | 'mengingat' | 'penetapan'
                                           -- | 'batang_tubuh' | 'penjelasan_umum'
                                           -- | 'penjelasan_pasal' | 'penutup'
    node_type            text NOT NULL,   -- 'judul' | 'pembukaan' | 'item' | 'penetapan' | 'bab'
                                           -- | 'bagian' | 'paragraf' | 'pasal' | 'ayat'
                                           -- | 'paragraf_isi' | 'catatan'
                                           -- (SENGAJA tanpa 'huruf'/'angka' — lihat README)

    bab_number           text,
    bagian_label          text,
    paragraf_label         text,
    pasal_number            text,
    ayat_number              text,
    huruf_label               text,   -- hanya dipakai node_type='item' (poin Menimbang/Mengingat)
    angka_label                text,  -- idem

    label                       text,  -- tampilan asli, mis. "Pasal 5", "KESATU"
    content                      text NOT NULL DEFAULT '',

    start_page                    int NOT NULL DEFAULT 0,
    end_page                       int NOT NULL DEFAULT 0,

    order_index                     bigint NOT NULL,  -- spasi 1000, drag-insert tanpa renumber

    original_node_type               text,   -- tebakan parser SEBELUM user relabel (audit)
    is_edited                         boolean NOT NULL DEFAULT false,
    edited_at                          timestamptz,

    warnings                            jsonb NOT NULL DEFAULT '[]'::jsonb,

    -- rujukan siap-pakai, dihitung sekali saat parse (walk ke atas pohon
    -- mahal kalau dihitung ulang tiap query RAG).
    citation                             text,

    created_at                            timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_nodes_doc_order ON nodes (document_id, order_index);
CREATE INDEX IF NOT EXISTS idx_nodes_doc_pasal_ayat ON nodes (document_id, pasal_number, ayat_number);
CREATE INDEX IF NOT EXISTS idx_nodes_parent ON nodes (parent_id);
CREATE INDEX IF NOT EXISTS idx_nodes_doc_page ON nodes (document_id, start_page);

-- full-text search (Indonesian) — hybrid retrieval bareng vector search,
-- karena istilah/angka pasal butuh exact match yang lemah di vector murni.
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS content_tsv tsvector
    GENERATED ALWAYS AS (to_tsvector('indonesian', coalesce(content, ''))) STORED;
CREATE INDEX IF NOT EXISTS idx_nodes_content_tsv ON nodes USING GIN (content_tsv);

-- =====================================================================
-- Trigger cascade label flat (bab_number/bagian_label/paragraf_label/
-- pasal_number) — DIUJI sungguhan (pindah Pasal antar-BAB, dan pindah
-- Bagian 4 tingkat termasuk cucu+cicit, dua-duanya lulus).
--
-- Kenapa perlu: parent_id sendirian TIDAK cukup. Kalau user drag-drop
-- pindahkan satu Pasal (beserta semua Ayat-nya) ke BAB lain, cuma ganti
-- parent_id Pasal-nya nggak bikin kolom flat bab_number di Pasal & Ayat-
-- ayatnya ikut berubah — jadi ada dua trigger:
--   1) BEFORE INSERT/UPDATE: node itu sendiri hitung ulang bab_number/
--      bagian_label/paragraf_label/pasal_number dari parent barunya,
--      lalu override kolom levelnya sendiri.
--   2) AFTER UPDATE: kalau ada perubahan, CASCADE turun ke SEMUA descendant
--      (anak, cucu, cicit, ...) lewat WITH RECURSIVE — berhenti otomatis
--      begitu tidak ada anak lagi.
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

    -- override kolom milik levelnya sendiri (pakai `label` sbg sumber nilai).
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
        RETURN NEW; -- tak ada perubahan yang perlu di-cascade, hemat kerja
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
-- node_embeddings — TABEL TERPISAH dari nodes, supaya ganti model embedding
-- nanti tidak perlu migrasi kolom vector di tabel utama.
-- Dimensi 1024 = output dense bge-m3.
-- =====================================================================
CREATE TABLE IF NOT EXISTS node_embeddings (
    node_id      bigint NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    model        text NOT NULL DEFAULT 'bge-m3',
    embedding    vector(1024),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, model)
);

-- =====================================================================
-- relations — relasi antar-peraturan (mencabut/mengubah/dasar_hukum/disebut),
-- diekstrak deterministik oleh internal/parser/relations.go.
-- =====================================================================
CREATE TABLE IF NOT EXISTS relations (
    id                   bigserial PRIMARY KEY,
    document_id           uuid NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    type                   text NOT NULL,      -- 'mencabut' | 'mengubah' | 'dasar_hukum' | 'disebut'
    key                     text NOT NULL,     -- kanonik: jenis|nomor|tahun
    jenis                    text,
    instansi                  text,
    nomor                      text,
    tahun                       text,
    tentang                      text,
    confidence                    text,        -- 'tinggi' | 'perlu_review'
    kutipan                        text,

    -- diisi BELAKANGAN begitu dokumen yang dirujuk juga ada di DB — peraturan
    -- yang mencabut bisa saja ke-parse lebih dulu daripada yang dicabutnya.
    target_document_id             uuid REFERENCES documents(id),

    created_at                      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_relations_document ON relations (document_id);
CREATE INDEX IF NOT EXISTS idx_relations_key ON relations (key);
CREATE INDEX IF NOT EXISTS idx_relations_target ON relations (target_document_id);
