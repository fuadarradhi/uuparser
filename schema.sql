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

-- =====================================================================
-- sources — daftar endpoint yang dijelajahi. Perannya kini kecil: hanya
-- tempat mengambil daftar tautan. Tidak ada lagi pemeriksaan jurisdiksi
-- (semua sumber dianggap tepercaya; yang diperiksa cuma "ini peraturan
-- atau bukan", oleh model teks).
-- =====================================================================
CREATE TABLE IF NOT EXISTS sources (
    id             bigserial PRIMARY KEY,
    code           text NOT NULL UNIQUE,
    endpoint_url   text NOT NULL,

    -- priority menentukan URUTAN PENGERJAAN antar-sumber: angka kecil
    -- dikerjakan lebih dulu sampai habis, baru sumber berikutnya.
    -- Contoh: 1 = Pemerintah Aceh, 2 = Banda Aceh.
    priority       int NOT NULL DEFAULT 100,
    source_type    text NOT NULL DEFAULT 'integrasi',  -- 'integrasi' | 'scrape' | ...
    source_config  jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- Sumber baku (2026-07-22): didaftarkan langsung di sini, bukan lewat
-- ENDPOINTS/.env atau INSERT manual terpisah, supaya sekali schema.sql
-- dijalankan sumbernya sudah ada. ON CONFLICT DO NOTHING membuat baris ini
-- aman dijalankan berkali-kali (mis. schema.sql dipakai ulang) — tidak akan
-- menimpa baris yang sudah ada atau menduplikasi.
--
-- Tambahkan sumber lain di sini juga saat didaftarkan (bukan lewat SQL
-- terpisah), supaya schema.sql tetap satu-satunya sumber kebenaran.
INSERT INTO sources (code, endpoint_url, source_type, priority) VALUES
    ('acehprov', 'http://jdih.acehprov.go.id/integrasi', 'integrasi', 1)
ON CONFLICT (code) DO NOTHING;

-- =====================================================================
-- documents — satu baris per BERKAS. download_url unik: tautan yang sudah
-- pernah didaftarkan tidak masuk dua kali, dari sumber mana pun.
-- =====================================================================
CREATE TABLE IF NOT EXISTS documents (
    id                bigserial PRIMARY KEY,

    -- Identitas berkas.
    download_url      text NOT NULL UNIQUE,   -- sudah dinormalisasi (lihat downloader.NormalizeURL)
    pdf_path          text,                   -- semua PDF di SATU folder
    pdf_sha256        text,                   -- isi identik = dokumen identik
    file_size         bigint,

    -- Jejak asal (informasi saja, bukan identitas).
    first_source_id   bigint REFERENCES sources(id) ON DELETE SET NULL,

    -- sort_tahun / sort_nomor berasal dari metadata SUMBER dan HANYA dipakai
    -- untuk mengurutkan antrian (dokumen terbaru dikerjakan lebih dulu).
    -- SENGAJA dipisah dari kolom `tahun`/`nomor` di bawah, yang dibaca model
    -- dari halaman pertama dan merupakan satu-satunya sumber kebenaran untuk
    -- identitas. Metadata JDIH boleh saja keliru: dampaknya paling jauh hanya
    -- urutan pengerjaan yang meleset, bukan dokumen salah dikenali.
    -- Bertipe int (bukan text) supaya 10 diurutkan setelah 9, bukan sebelum 2.
    sort_tahun        int,
    sort_nomor        int,

    -- Status pipeline.
    status            text NOT NULL DEFAULT 'pending',
    attempts          int NOT NULL DEFAULT 0,
    last_error        text,
    reject_reason     text,     -- 'bukan_peraturan' | 'duplikat' | 'unduh_gagal' | ...
    duplicate_of      bigint REFERENCES documents(id) ON DELETE SET NULL,

    -- Metadata hasil pembacaan model teks atas halaman 1 (bukan dari sumber).
    is_peraturan      boolean,
    jenis             text,
    -- wilayah adalah bentuk baku hasil pemetaan deterministik ke salah satu
    -- dari 25 wilayah yang dikenal sistem: NASIONAL, PEMERINTAH ACEH, atau
    -- salah satu dari 23 kabupaten/kota. Namanya sebelumnya "instansi" —
    -- diganti karena isinya memang selalu nama wilayah administratif.
    wilayah           text,
    -- nomor DISIMPAN DUA BENTUK. Nomor keputusan kerap bukan angka tunggal,
    -- mis. "300.2/ 69 /2026": menyimpannya sebagai angka saja akan
    -- menghilangkan nomor aslinya, sedangkan menyimpannya sebagai teks saja
    -- membuat pengurutan salah ("10" sebelum "2").
    nomor             text,   -- persis seperti tertulis: "300.2/ 69 /2026"
    nomor_urut        int,    -- angka pertama untuk pengurutan: 300
    tahun             text,
    tentang           text,

    -- Bagian penetapan & pengundangan. Diisi deterministik lewat regex bila
    -- polanya baku (lihat pipeline/trigger.go); model teks HANYA dipanggil
    -- ketika parser menemukan penandanya ("Ditetapkan di" / "Diundangkan
    -- di") tetapi tidak dapat menguraikannya sendiri.
    --
    -- *_oleh adalah JABATAN penanda tangan ("GUBERNUR ACEH"); *_oleh_nama
    -- adalah NAMA orangnya ("MUZAKIR MANAF") — dipisah sengaja (permintaan
    -- user, 2026-07-22) karena keduanya diambil dari baris cetak yang
    -- berbeda (jabatan lalu, biasanya setelah "Ttd.", nama).
    ditetapkan_di          text,
    ditetapkan_tanggal     text,
    ditetapkan_oleh        text,
    ditetapkan_oleh_nama   text,
    diundangkan_di         text,
    diundangkan_tanggal    text,
    diundangkan_oleh       text,
    diundangkan_oleh_nama  text,

    -- instansi_tertulis menyimpan apa yang benar-benar tertulis di dokumen
    -- ("GUBERNUR ACEH"), sedangkan wilayah di atas menyimpan hasil pemetaan
    -- ke wilayah bakunya ("PEMERINTAH ACEH"). Pemetaan itu dilakukan kode
    -- secara deterministik, bukan oleh model.
    instansi_tertulis text,
    struktur          text,     -- 'pasal_ayat' | 'diktum' | 'unknown'

    -- Kunci kanonik untuk deteksi duplikat berbasis identitas peraturan,
    -- diisi kode dari metadata di atas: jenis|wilayah|nomor|tahun.
    canonical_key     text,

    total_pages       int,
    downloaded_at     timestamptz,
    classified_at     timestamptz,
    ocr_completed_at  timestamptz,
    parsed_at         timestamptz,
    -- parse_status (2026-07-24, permintaan user: "pastikan status fail atau
    -- success ada di db untuk UI"): salinan REDUNDAN dari
    -- parse_snapshots.status ('SUCCESS'|'WARNING'|'FAIL'), ditulis di
    -- transaksi yang sama oleh InsertParseResult. Datanya SUDAH ada lewat
    -- join ke parse_snapshots (document_id = PK-nya di sana), tapi kolom ini
    -- menghindari join tiap kali UI hanya ingin daftar dokumen + status
    -- ringkasnya. Sumber kebenaran tetap parse_snapshots.status.
    parse_status      text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- Untuk database yang sudah dibuat SEBELUM kolom parse_status ditambahkan
-- (2026-07-24) — pola sama seperti kolom lain di atas, aman dijalankan ulang.
ALTER TABLE documents ADD COLUMN IF NOT EXISTS parse_status text;

CREATE INDEX IF NOT EXISTS idx_documents_status ON documents (status);
CREATE INDEX IF NOT EXISTS idx_documents_parse_status ON documents (parse_status);
CREATE INDEX IF NOT EXISTS idx_documents_sha ON documents (pdf_sha256);
CREATE INDEX IF NOT EXISTS idx_documents_canonical ON documents (canonical_key);

-- Indeks antrian: mendukung "ambil satu pekerjaan berikutnya" pada tiap tahap
-- tanpa memindai seluruh tabel. Urutan kolomnya mengikuti ORDER BY yang
-- dipakai worker: status dulu (penyaring), lalu prioritas sumber, lalu
-- dokumen terbaru.
CREATE INDEX IF NOT EXISTS idx_documents_queue
    ON documents (status, sort_tahun DESC NULLS LAST, sort_nomor DESC NULLS LAST);

-- =====================================================================
-- document_pages — hasil OCR per halaman DAN hasil perbaikan model teks.
--   ocr_text    : mentah dari model visi, TIDAK PERNAH ditimpa
--   edited_text : koreksi manusia lewat UI
-- Parser membaca COALESCE(edited_text, ocr_text).
--
-- Tahap "perbaikan salah ketik oleh model teks" DIHAPUS (2026-07-21):
-- setelah membandingkan keluaran mentah dengan hasil perbaikannya, perbaikan
-- itu tidak memberi manfaat yang sepadan. Model teks kini hanya dipakai untuk
-- membaca metadata yang sulit diuraikan parser.
-- =====================================================================
CREATE TABLE IF NOT EXISTS document_pages (
    id             bigserial PRIMARY KEY,
    document_id    bigint NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    page_number    int NOT NULL,

    ocr_text       text NOT NULL DEFAULT '',
    edited_text    text,

    is_edited      boolean NOT NULL DEFAULT false,
    edited_at      timestamptz,

    is_empty       boolean NOT NULL DEFAULT false,
    is_truncated   boolean NOT NULL DEFAULT false,
    ink_ratio      real,
    cropped_pct    real,
    -- dpi_pakai adalah DPI render yang SUNGGUH dipakai untuk halaman ini
    -- (2026-07-22). DPI dipilih otomatis lewat skor ketajaman HANYA di
    -- halaman 1 dokumen (lihat raster.blurScore, extractor.renderAdaptif);
    -- halaman-halaman berikutnya mengikuti DPI halaman 1 — tidak dihitung
    -- ulang per halaman. Kolom ini tetap diisi di SETIAP halaman (bukan
    -- cuma halaman 1) supaya selalu jelas dari data render mana teks OCR
    -- suatu halaman berasal, tanpa perlu join balik ke halaman 1.
    dpi_pakai      int,
    duration_ms    int,
    notes          jsonb NOT NULL DEFAULT '[]'::jsonb,

    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (document_id, page_number)
);

CREATE INDEX IF NOT EXISTS idx_pages_doc ON document_pages (document_id, page_number);

-- =====================================================================
-- parse_snapshots — SATU baris per dokumen. Parser hanya boleh jalan SEKALI
-- per dokumen (2026-07-22): tidak ada mekanisme reparse lagi — koreksi
-- sesudahnya dilakukan manusia langsung di tabel `nodes` (drag-drop/
-- relabel), bukan dengan menjalankan parser ulang. document_id sebagai
-- PRIMARY KEY (bukan bigserial tersendiri) menegakkan ini: INSERT kedua
-- untuk dokumen yang sama gagal keras (unique violation), bukan menimpa
-- diam-diam.
-- =====================================================================
CREATE TABLE IF NOT EXISTS parse_snapshots (
    document_id       bigint PRIMARY KEY REFERENCES documents(id) ON DELETE CASCADE,
    status            text NOT NULL,
    report            jsonb NOT NULL DEFAULT '{}'::jsonb,
    extraction_notes  jsonb NOT NULL DEFAULT '[]'::jsonb,
    -- ai_reviewed_at (2026-07-24, permintaan user: "AI yang periksa hasil
    -- parser terakhir, setiap selesai 1 dokumen") — dicatat SETIAP kali
    -- pemeriksaan AI post-parse (AskDocumentReview, lihat thinking.go)
    -- SELESAI dijalankan untuk dokumen ini, TERLEPAS apakah ia menemukan
    -- sesuatu atau tidak. Beda tujuan dari review_flags di bawah: kolom ini
    -- menjawab "sudah diperiksa AI belum?" (untuk UI); review_flags
    -- menjawab "apa saja yang perlu ditinjau?". NULL berarti belum pernah
    -- diperiksa (mis. model teks sedang tidak tersedia saat parse ini).
    ai_reviewed_at    timestamptz,
    parsed_at         timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE parse_snapshots ADD COLUMN IF NOT EXISTS ai_reviewed_at timestamptz;

-- =====================================================================
-- nodes — hasil parse siap-edit (drag-drop/relabel di UI). Diisi SEKALI
-- oleh parser (lihat parse_snapshots di atas); koreksi sesudahnya adalah
-- edit manusia langsung ke baris di sini, bukan reparse.
-- =====================================================================
CREATE TABLE IF NOT EXISTS nodes (
    id                 bigserial PRIMARY KEY,
    document_id        bigint NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    parent_id          bigint REFERENCES nodes(id) ON DELETE CASCADE,

    section            text NOT NULL,
    -- 2026-07-22: ditambahkan "diktum" — dokumen jenis Keputusan/Instruksi
    -- berstruktur Diktum (KESATU/KEDUA/dst), bukan Pasal/Ayat. Sebelum ini
    -- node_type tidak pernah benar-benar bernilai "diktum" di kode (lihat
    -- bug yang diperbaiki di internal/parser/parse_batangtubuh.go) meski
    -- sudah direncanakan sejak desain awal.
    node_type          text NOT NULL,   -- bab|bagian|paragraf|pasal|ayat|diktum|judul|item|penetapan|paragraf_isi|catatan

    bab_number         text,
    bagian_label       text,
    paragraf_label     text,
    pasal_number       text,
    ayat_number        text,
    huruf_label        text,
    angka_label        text,
    -- diktum_number: label diktum APA ADANYA seperti tertulis ("KESATU",
    -- bukan "1") — dokumen Keputusan biasa mengutip "Diktum KESATU", bukan
    -- nomor urutnya, jadi bentuk tertulis lebih berguna untuk sitasi.
    -- Selalu NULL untuk dokumen berstruktur pasal_ayat (lihat trigger
    -- nodes_recompute_own_labels di bawah — diktum tidak pernah campur
    -- dengan bab_number/pasal_number dkk dalam satu dokumen).
    diktum_number      text,

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
    -- is_appendix (2026-07-23): true HANYA untuk node section='lampiran'.
    -- Permintaan user: isi Lampiran berguna saat menelusuri/verifikasi
    -- dokumen (mis. cek Rencana Alokasi Air yang dirujuk), tapi TIDAK
    -- relevan saat pencarian benar-benar mencari ATURAN (Pasal/Ayat/Diktum)
    -- — jadi butuh flag agar query/RAG bisa memilih menyertakan atau
    -- mengecualikannya, bukan menghapusnya. Bukan flag umum "penting per
    -- node" yang dinilai satu-satu — sepenuhnya ditentukan oleh section,
    -- diisi otomatis oleh parser (lihat internal/parser/parse_lampiran.go),
    -- bukan sesuatu yang pernah diset manual.
    is_appendix        boolean NOT NULL DEFAULT false,
    -- is_dictum / is_title (2026-07-24, permintaan user): lihat catatan
    -- lengkap pada Node.IsDictum/IsTitle di internal/parser/types.go dan
    -- classifyContentFlags di internal/parser/parser.go. Dihitung otomatis
    -- oleh parser, bukan diset manual — sama seperti is_appendix.
    is_dictum          boolean NOT NULL DEFAULT false,
    is_title           boolean NOT NULL DEFAULT false,
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- Untuk database yang sudah dibuat SEBELUM kolom is_dictum/is_title
-- ditambahkan (2026-07-24) — pola sama seperti diktum_number/content_tsv di
-- bawah, aman dijalankan ulang lewat IF NOT EXISTS.
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS is_dictum boolean NOT NULL DEFAULT false;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS is_title  boolean NOT NULL DEFAULT false;

-- Untuk database yang sudah dibuat SEBELUM kolom diktum_number ditambahkan
-- (2026-07-22) — pola sama seperti content_tsv di bawah, aman dijalankan
-- ulang lewat IF NOT EXISTS.
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS diktum_number text;

CREATE INDEX IF NOT EXISTS idx_nodes_doc_order ON nodes (document_id, order_index);
CREATE INDEX IF NOT EXISTS idx_nodes_doc_pasal_ayat ON nodes (document_id, pasal_number, ayat_number);
CREATE INDEX IF NOT EXISTS idx_nodes_doc_diktum ON nodes (document_id, diktum_number);
CREATE INDEX IF NOT EXISTS idx_nodes_parent ON nodes (parent_id);
CREATE INDEX IF NOT EXISTS idx_nodes_doc_page ON nodes (document_id, start_page);
CREATE INDEX IF NOT EXISTS idx_nodes_dictum_title ON nodes (document_id, is_dictum, is_title);

ALTER TABLE nodes ADD COLUMN IF NOT EXISTS content_tsv tsvector
    GENERATED ALWAYS AS (to_tsvector('indonesian', coalesce(content, ''))) STORED;
CREATE INDEX IF NOT EXISTS idx_nodes_content_tsv ON nodes USING GIN (content_tsv);

-- =====================================================================
-- review_flags (2026-07-24, permintaan user: "mekanisme yang memudahkan
-- saya cek kesalahan parsing, yang bisa saya tampilkan di web UI nanti").
--
-- SATU baris = SATU hal yang layak ditinjau manusia, dari SATU sumber:
--   - 'diagnose'            — Issue dari parser.Diagnose (PASAL_GAP,
--                              NO_PASAL, ANCHOR_LEAK, EMPTY_PASAL, dst).
--                              Selalu document-level (node_id NULL) —
--                              parser.Issue tidak membawa referensi node.
--   - 'parser_warning'      — Node.Warnings (mis. "Teks tidak dikenali
--                              struktur; perlu tinjauan") — SELALU
--                              node-level.
--   - 'model_anchor_leak'   — AskTinjauan (tinjau.md) MENGKONFIRMASI
--                              (bermasalah=true) node dari
--                              parser.AnchorLeakNodes.
--   - 'model_orphan_review' — AskTinjauan (orphan.md) MENGKONFIRMASI
--                              node dari parser.OrphanWarningNodes.
--   - 'model_document_review' — AskDocumentReview (document_review.md)
--                              MENGKONFIRMASI ada yang mencurigakan pada
--                              RINGKASAN dokumen (lihat thinking.go) —
--                              document-level.
--
-- SENGAJA HANYA baris yang benar-benar actionable (bukan "sudah dicek,
-- aman") — sama seperti extraction_notes sebelumnya: kalau setiap
-- pemeriksaan model yang "aman" juga bikin baris, tabel ini membanjir
-- tanpa nilai tinjau. "Apakah dokumen X sudah diperiksa AI" dijawab
-- kolom parse_snapshots.ai_reviewed_at, BUKAN oleh ada/tidaknya baris
-- di sini.
--
-- resolved/resolved_note: UI web menandai satu baris selesai ditinjau
-- TANPA mengubah data hasil parse itu sendiri (koreksi data tetap lewat
-- edit langsung di tabel nodes, seperti sudah disepakati — lihat catatan
-- di InsertParseResult).
CREATE TABLE IF NOT EXISTS review_flags (
    id            bigserial PRIMARY KEY,
    document_id   bigint NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    node_id       bigint REFERENCES nodes(id) ON DELETE SET NULL,
    source        text NOT NULL,   -- lihat daftar di atas
    code          text NOT NULL,   -- 'PASAL_GAP' | 'NODE_WARNING' | 'ANCHOR_LEAK' | dst
    severity      text NOT NULL,   -- samakan dengan parser.Severity: 'info' | 'needs_review'
    message       text NOT NULL,
    resolved      boolean NOT NULL DEFAULT false,
    resolved_at   timestamptz,
    resolved_note text,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_review_flags_document ON review_flags (document_id);
CREATE INDEX IF NOT EXISTS idx_review_flags_unresolved ON review_flags (document_id) WHERE NOT resolved;
CREATE INDEX IF NOT EXISTS idx_review_flags_node ON review_flags (node_id);

-- v_review_queue: SATU query yang langsung dipakai halaman daftar UI web
-- ("dokumen mana yang butuh ditinjau") — jumlah flag belum-selesai per
-- dokumen plus identitas ringkasnya, tanpa UI perlu menyusun JOIN/GROUP BY
-- sendiri. Detail per-flag (untuk halaman satu dokumen) tetap query
-- langsung ke review_flags WHERE document_id = ...
CREATE OR REPLACE VIEW v_review_queue AS
SELECT
    d.id                AS document_id,
    d.jenis, d.wilayah, d.nomor, d.tahun, d.tentang,
    d.parse_status,
    ps.ai_reviewed_at,
    COUNT(rf.id) FILTER (WHERE NOT rf.resolved)                            AS unresolved_count,
    COUNT(rf.id) FILTER (WHERE NOT rf.resolved AND rf.severity = 'needs_review') AS unresolved_needs_review,
    MAX(rf.created_at)                                                      AS last_flag_at
FROM documents d
JOIN parse_snapshots ps ON ps.document_id = d.id
LEFT JOIN review_flags rf ON rf.document_id = d.id
GROUP BY d.id, ps.ai_reviewed_at;

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

    -- diktum_number TIDAK diwarisi dari parent seperti bab/bagian/dst di
    -- atas: node Diktum selalu akar (parent_id NULL — dokumen Keputusan tak
    -- punya Bab/Bagian/Paragraf/Pasal di atasnya), jadi cukup diisi
    -- langsung dari label sendiri, bukan dari lookup parent.
    NEW.diktum_number := NULL;

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
        WHEN 'diktum' THEN
            NEW.diktum_number := NEW.label;
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
    document_id        bigint NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    type               text NOT NULL,
    key                text,
    jenis              text,
    instansi           text,
    nomor              text,
    tahun              text,
    tentang            text,
    confidence         text,
    kutipan            text,
    target_document_id bigint REFERENCES documents(id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_relations_document ON relations (document_id);
CREATE INDEX IF NOT EXISTS idx_relations_key ON relations (key);
CREATE INDEX IF NOT EXISTS idx_relations_target ON relations (target_document_id);
