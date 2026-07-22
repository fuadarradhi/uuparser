-- reset_data.sql — kosongkan SEMUA data kecuali tabel `sources`.
--
-- Dipakai saat ingin mengulang seluruh proses dari nol (mis. setelah mengubah
-- prompt atau model) tanpa perlu mendaftarkan ulang endpoint JDIH.
--
-- TRUNCATE ... CASCADE dipilih daripada DELETE: jauh lebih cepat, sekaligus
-- mengosongkan tabel yang berelasi. RESTART IDENTITY mengembalikan penghitung
-- bigserial (documents.id, nodes.id, document_pages.id, relations.id) ke 1 —
-- aman karena tidak ada lagi baris yang menunjuk ke nilai lama. sources.id
-- TIDAK ikut reset (tabelnya tidak di-TRUNCATE, baris sources dipertahankan).
--
-- CATATAN: berkas PDF di data/pdf/ TIDAK ikut terhapus. Karena nama berkas
-- diturunkan dari hash isinya, mengunduh ulang akan menghasilkan nama yang
-- sama dan menimpa berkas lama — jadi membiarkannya aman. Bila memang ingin
-- bersih total, hapus juga foldernya:  rm -rf data/pdf/*

BEGIN;

TRUNCATE TABLE
    node_embeddings,
    nodes,
    parse_snapshots,
    relations,
    document_pages,
    documents
RESTART IDENTITY CASCADE;

COMMIT;

-- Periksa hasilnya:
SELECT 'sources'        AS tabel, count(*) FROM sources
UNION ALL SELECT 'documents',      count(*) FROM documents
UNION ALL SELECT 'document_pages', count(*) FROM document_pages
UNION ALL SELECT 'nodes',          count(*) FROM nodes
UNION ALL SELECT 'relations',      count(*) FROM relations
ORDER BY 1;
