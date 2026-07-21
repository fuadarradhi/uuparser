# Catatan migrasi ke Postgres (2026-07-20)

Dokumen ini menjelaskan apa yang berubah, apa yang sudah **teruji sungguhan**,
apa yang **belum bisa diuji** di lingkungan pembuatnya (dan kenapa), serta
langkah yang perlu kamu lakukan sendiri. Ikuti pola `CATATAN-BUILD.md` yang
sudah ada — jujur soal batas verifikasi, bukan basa-basi.

## Ringkasan perubahan

- **State pindah total ke Postgres.** Tidak ada lagi deteksi "sudah selesai"
  lewat keberadaan file. Semua status hidup di kolom `documents.status`.
- **Tidak ada file perantara sama sekali** kecuali PDF sumber (dipakai untuk
  pratinjau) dan log. Teks OCR, catatan per halaman, dan hasil parse langsung
  ke Postgres — tidak lewat `.txt`/`.json` dulu.
- **Tiga worker independen** (`internal/pipeline`): downloader, OCR (satu
  goroutine — model cuma boleh dipakai satu waktu), parser. Berkoordinasi
  lewat `SELECT ... FOR UPDATE SKIP LOCKED`.
- **Tidak ada flag CLI sama sekali.** Konfigurasi murni dari `.env` (6 kunci —
  lihat `.env.example`). Aksi admin (reset/approve/reject/reparse) lewat SQL
  langsung — lihat bagian di bawah.
- **Parser berhenti di level Ayat** — huruf/angka dilipat ke teks Ayat induk,
  bukan node terpisah.
- **Gate kelayakan HANYA baca halaman 1** (bukan 5 halaman) — cek header resmi
  (jenis + instansi + nomor + tahun) sesuai Lampiran II UU 12/2011, plus
  kecocokan jurisdiksi terhadap sumbernya sendiri.
- **Parser "smart"**: koreksi fuzzy (Levenshtein) untuk anchor struktural yang
  typo (`Menimbing` → `Menimbang`), pembersihan watermark/URL JDIH,
  penghapusan duplikasi "catchword" lintas-halaman, normalisasi ejaan lama
  `Propinsi` → `Provinsi`.

## Status verifikasi — baca ini sebelum menjalankan

| Bagian | Status |
|---|---|
| `schema.sql` (termasuk trigger cascade) | **Dijalankan sungguhan** di Postgres 16 + pgvector nyata — instal penuh dari nol berulang kali, plus tes trigger (pindah Pasal antar-BAB, pindah Bagian 4 tingkat, insert gaya-store dan gaya-UI) |
| **Seluruh SQL di `internal/store`** | **Setiap query diuji VERBATIM** terhadap Postgres nyata lewat PREPARE/EXECUTE, berurutan mengikuti alur pipeline: daftar sumber → upsert dokumen (idempoten) → claim download → mark downloaded → claim OCR (subselect instansi) → save page ×3 + upsert ulang → HasPage/ReadPageRange → ApplyHeaderResult (cabang terima / tolak / review_manual) → MarkOCRDone → ClaimForParse → transaksi ReplaceParseResult (delete+snapshot+insert pohon, label flat terisi otomatis oleh trigger) → alur reparse (edit teks → klaim ulang → flag terhapus) → approve/reset → MarkDownloadFailed & MarkOCRFailed (termasuk batas percobaan) → InsertRelation. **Semua lulus.** Yang belum teruji hanyalah pgx sebagai *driver* (pemanggilan dari Go) — SQL-nya sendiri sudah terbukti benar |
| `internal/parser` + `internal/extractor` + `mapNodesToInserts` | **Tes integrasi bertahap sungguhan** (`go test`, model OCR & raster diganti stub yang mengembalikan teks Perda realistis 3 halaman): (1) jalur terima — 3 halaman tersimpan, header terbaca benar; (2) jalur tolak jurisdiksi lain — HANYA halaman 1 di-OCR; (3) jalur tolak naskah akademik; (4) resume per-halaman; (5) parse → pohon node: parent benar (Pasal→BAB, Ayat→Pasal), Label terisi, penjelasan tidak nyebrang parent ke batang tubuh, StartPage benar, urutan insert valid |
| `internal/config`, `internal/downloader` | Compile bersih standalone |
| `internal/localllm`, `internal/raster` | Tidak bisa di-build di sandbox (CGo + yzma Go≥1.26) — type-check lewat stub; **tidak diubah sesi ini** kecuali pemanggilan dari extractor |
| `main.go`, `internal/pipeline` | Type-check + `go vet` bersih; logic worker teruji lewat tes integrasi di atas; loop worker sungguhan (goroutine + polling DB) belum pernah dijalankan menempel Postgres |

### Bug NYATA yang ditemukan & diperbaiki oleh tes bertahap ini

1. **`Label` tidak pernah diisi** di `mapNodesToInserts` — padahal trigger DB
   menghitung semua label flat dari `NEW.label`. Tanpa perbaikan ini, parse
   pertama di produksi menghasilkan `bab_number`/`pasal_number` NULL semua.
2. **Pergantian section tidak me-reset tracker parent** — Pasal di
   penjelasan_pasal akan salah-parent ke BAB terakhir batang tubuh.
3. **OCR gagal mengembalikan status ke `pending`** (via MarkDownloadFailed) —
   dokumen diunduh ulang sia-sia. Ditambah `MarkOCRFailed` yang kembali ke
   `downloaded` saja.
4. **`StructureType` selalu "unknown"** — `rePasalAnywhere` ber-anchor awal
   baris tapi dipanggil pada teks yang sudah diratakan (newline dibuang).
5. **Trigger belum menangani `ayat`** — insert Ayat dari UI (cuma set label)
   akan meninggalkan `ayat_number` NULL. Trigger dilengkapi.

**Kalau ada galat kompilasi atau runtime saat kamu `go build`/jalankan**,
kirim pesan galatnya — perbaikannya kemungkinan besar terbatas di satu berkas
saja (pola yang sama seperti kasus yzma sebelumnya).

## Langkah menjalankan

1. `psql -d uuparser < schema.sql` (di database kosong)
2. Daftarkan sumber JDIH secara manual (belum ada UI untuk ini):
   ```sql
   INSERT INTO sources (code, endpoint_url, hostname, source_type,
                         jurisdiction_level, instansi_name)
   VALUES ('acehbarat', 'http://jdih.acehbaratkab.go.id/integrasi',
           'jdih.acehbaratkab.go.id', 'integrasi', 'kabupaten',
           'Kabupaten Aceh Barat');
   ```
   `instansi_name` HARUS persis sama seperti yang tertulis di kepala dokumen
   resmi sumber itu (lihat `internal/parser/header.go` — exact match, bukan
   `Contains`).
3. Isi `.env` (contoh di `.env.example`) — minimal `DATABASE_URL`,
   `MODEL_PATH`, `MMPROJ_PATH`, `LIB_PATH`.
4. `go mod tidy && go build .`
5. Jalankan `./uuparser` — tidak ada flag, langsung baca `.env` di direktori
   kerja.

## Perubahan status manual lewat SQL (pengganti flag CLI yang dihapus)

Semua ini sengaja SQL langsung, bukan CLI — biar kamu (atau nanti UI web)
pakai jalur yang sama persis.

**Lihat progres cepat:**
```sql
SELECT status, count(*) FROM documents GROUP BY status ORDER BY count(*) DESC;
```

**Retry dokumen yang gagal (reset ke pending, hapus jejak percobaan):**
```sql
UPDATE documents
SET status = 'pending', attempts = 0, last_error = NULL, updated_at = now()
WHERE id = '<uuid>';
```

**Setujui dokumen berstatus `review_manual`** (biasanya dokumen sebelum
UU 12/2011 — kamu sudah baca halaman 1-nya dan yakin ini memang peraturan
sumber ini) — lanjutkan OCR dari halaman 2:
```sql
UPDATE documents SET status = 'ocr_in_progress', updated_at = now()
WHERE id = '<uuid>' AND status = 'review_manual';
```

**Tolak dokumen `review_manual`:**
```sql
UPDATE documents
SET status = 'rejected', reject_reason = 'manual_reject', updated_at = now()
WHERE id = '<uuid>' AND status = 'review_manual';
```

**Minta reparse** (setelah kamu edit `document_pages.edited_text` untuk
memperbaiki OCR yang salah) — parser worker akan mengambilnya di putaran
berikutnya, MENIMPA total `nodes` yang ada untuk dokumen ini:
```sql
UPDATE document_pages
SET edited_text = '<teks yang sudah dikoreksi>', is_edited = true, edited_at = now()
WHERE document_id = '<uuid>' AND page_number = <n>;

UPDATE documents SET reparse_requested_at = now() WHERE id = '<uuid>';
```

**Lihat dokumen yang macet/gagal:**
```sql
SELECT id, slug, status, attempts, last_error FROM documents
WHERE status IN ('failed', 'rejected') ORDER BY updated_at DESC LIMIT 20;
```

## Batasan yang perlu diketahui (bukan bug tersembunyi, tapi belum sempurna)

- **`mapNodesToInserts`** kini teruji lewat tes integrasi (pohon multi-BAB,
  penjelasan terpisah, invarian urutan insert) — tapi tetap terhadap teks
  sintetis, bukan hasil OCR dokumen ASLI. Kalau `nodes.parent_id` terlihat
  aneh setelah parse dokumen nyata pertama, kirim `document_pages.ocr_text`-nya.
- **Watermark/URL stripping, koreksi fuzzy anchor, dan dedup catchword**
  lintas-halaman semuanya HEURISTIK yang sengaja dibuat KONSERVATIF (lebih
  suka tidak menyentuh daripada salah menghapus konten asli). Kalau ternyata
  korpus nyata kamu punya pola lain yang belum tertangkap, kirim contoh
  `document_pages.ocr_text` yang bermasalah — akan saya sesuaikan pola
  regex/heuristiknya, bukan ditambah LLM.
- **Cross-page dedup HANYA menangani kasus PERSIS** (baris akhir halaman N
  adalah prefiks utuh baris awal halaman N+1). Overlap yang tidak persis
  sengaja TIDAK disentuh — lihat komentar di `internal/parser/stitch.go`.
- **pgx sebagai driver & loop worker sungguhan** belum pernah dijalankan
  menempel Postgres (SQL-nya sendiri sudah diuji verbatim, lihat tabel di
  atas) — sumber galat pertama paling mungkin saat kamu jalankan.

## Berkas yang DIHAPUS sejak sesi ini — hapus juga di tempatmu

- `internal/state/` (seluruh folder, `state.go`) — retry sepenuhnya di kolom
  `documents.attempts`/`last_error`
- `catter_go.txt` — dump kode lama, sudah tidak representatif

Berkas lain semuanya DIGANTI ISINYA (copy-paste/replace langsung aman):
`main.go`, `schema.sql` (baru), `CATATAN-MIGRASI.md` (baru), `.env.example`,
`README.md` (hanya tambah peringatan di atas), seluruh `internal/config`,
`internal/downloader` (+ `source.go`, `integrasi_source.go` baru),
`internal/extractor/extractor.go`, `internal/parser` (+ `line.go`,
`header.go`, `fuzzyfix.go` baru), `internal/store/` (baru),
`internal/pipeline/` (baru: `pipeline.go`, `downloader_worker.go`,
`ocr_worker.go`, `parser_worker.go`, `pagestore.go`, `util.go`).
`internal/localllm`, `internal/raster`, `internal/logx`, `internal/fsutil`,
`go.mod`, `go.sum` tidak berubah.

## Yang dihapus dari desain lama

- Semua flag CLI (`-env`, `-once`, `-render`, `-render-out`, `-limit`,
  `-ids`, dan rencana `-reset-id`/`-approve-id`/`-reject-id` yang sempat
  dirancang tapi tidak jadi dipakai — semua jadi SQL manual di atas)
- `PROBE_PAGES`, `DOWNLOAD_FIRST`, `SAVE_PNG`/`DEBUG_CROP`, `OCR_PROMPT`,
  `OCR_MAX_TOKENS`, `DPI`, `AUTO_CROP`, `BLANK_INK`, `DELAY_MS`,
  `MAX_ATTEMPTS`, `INTERVAL`, `ENDPOINTS` sebagai kunci `.env` — semua jadi
  konstanta di kode (lihat `internal/pipeline/*_worker.go` dan
  `internal/extractor/extractor.go`)
- `internal/state` (dua kali dihapus — sempat dikembalikan sebentar karena
  masih dipakai `extractor.go` versi lama, sekarang benar-benar tidak
  dipakai lagi karena retry sepenuhnya di kolom `documents.attempts`)
- Semua penulisan file `.txt`/`.json` perantara (`ocr/<slug>/pageN.txt`,
  `json/<slug>.json`, `_rejected.json`, `_page_notes.json`,
  `_last_run.json`, `integrasi.json` mentah)
