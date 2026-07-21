# Catatan arsitektur & migrasi (2026-07-21)

Skema ini **menggantikan** versi sebelumnya. Hapus database lama, jalankan
`schema.sql` di database kosong.

## Perubahan mendasar: dokumen sebagai pusat

Sebelumnya identitas dokumen terikat pada sumbernya (`source_id` + `id_data`),
dan metadata (judul/jenis/nomor/tahun) diambil dari JDIH. Sekarang:

- **Downloader tidak mempercayai apa pun dari sumber kecuali tautannya.**
  Judul, jenis, nomor, tahun dari JDIH diabaikan seluruhnya.
- **`documents.download_url` UNIK.** Tautan yang sama dari sumber mana pun
  hanya menghasilkan satu baris. Sumber pertama yang menemukannya dicatat di
  `first_source_id` sebagai jejak, bukan identitas.
- **Semua PDF di satu folder** (`data/pdf/`), nama berkas = SHA-256 isinya.
  Berkas identik dari dua JDIH otomatis menjadi satu berkas fisik.
- **Identitas peraturan dibaca dari halaman pertama dokumen** oleh model teks,
  bukan dari metadata sumber.
- **Tidak ada lagi pemeriksaan jurisdiksi.** Semua sumber dianggap tepercaya;
  yang diperiksa hanya "ini peraturan atau bukan". Peraturan nasional yang
  muncul di JDIH kabupaten tetap disimpan.

## Alur

```
downloader:  daftar tautan (unik)  →  unduh  →  hash
                                       ├─ hash sudah ada → status 'duplicate'
                                       └─ baru           → status 'downloaded'

ocr worker (satu goroutine, DUA model menempel):
   OCR hal 1 → perbaiki hal 1 → klasifikasi (model teks, JSON)
      ├─ bukan peraturan  → 'rejected'  (sisa halaman TIDAK di-OCR)
      ├─ duplikat identitas → 'duplicate' (sisa halaman TIDAK di-OCR)
      └─ lolos → OCR hal 2 → perbaiki hal 2 → ... → 'ocr_done'

parser worker: 'ocr_done' → nodes + parse_snapshot → 'parsed'
```

Urutan **berurutan per halaman** (OCR → perbaiki → OCR → perbaiki) supaya
kemajuan terpantau per halaman di UI dan dokumen yang gugur di halaman 1
langsung ditinggalkan. Kedua model dimuat **sekali** dan dilepas **bersamaan**
hanya ketika antrian habis — tidak ada bongkar-pasang model per halaman.

## Tiga lapis teks per halaman

| Kolom | Isi | Ditimpa? |
|---|---|---|
| `ocr_text` | mentah dari model visi | **tidak pernah** |
| `fixed_text` | hasil perbaikan model teks | ya, tiap kali diproses ulang |
| `edited_text` | koreksi manusia dari UI | hanya oleh manusia |

Parser membaca `COALESCE(edited_text, fixed_text, ocr_text)`.

**Diff tidak disimpan** — dihitung saat dibutuhkan oleh UI dari `ocr_text` dan
`fixed_text` (data turunan cepat basi begitu `edited_text` berubah). Yang
disimpan hanya `fix_ops_count` (berapa bagian yang diubah model) untuk
keperluan dashboard, dan `prompt_hash` (versi prompt yang dipakai).

Saran format untuk UI: render diff sebagai daftar operasi, bukan HTML di
database —
`[{"op":"same","text":"..."},{"op":"del","text":"..."},{"op":"ins","text":"..."}]`
sehingga UI bebas memilih tampilan inline atau berdampingan tanpa mengubah data.

## Model teks tidak pernah memerintah aplikasi

Teks OCR berasal dari dokumen di internet, jadi diperlakukan sebagai masukan
**tak tepercaya**. Model hanya mengembalikan satu objek JSON berskema tetap;
kode Go yang memutuskan apa yang di-update. Nilai divalidasi sebelum dipakai:

- `nomor` harus cocok `^[0-9]{1,4}[A-Za-z]?$`, `tahun` harus 4 digit wajar —
  bila tidak, dikosongkan (bukan dipercaya)
- `struktur` di luar `pasal_ayat`/`diktum` menjadi `unknown`
- Jawaban yang bukan JSON menjadi galat, bukan diterima diam-diam
- Identitas tidak lengkap → `canonical_key` kosong → pemeriksaan duplikat
  dilewati (lebih baik memproses dua kali daripada membuang dokumen sah)

Untuk perbaikan halaman: bila model gagal, mengembalikan kosong, atau hasilnya
menyusut di bawah 60% panjang aslinya (indikasi meringkas), hasilnya
**dibuang** dan teks OCR mentah yang dipakai.

## Prompt dapat disunting

`prompts/classify.md` dan `prompts/fix_page.md` dibaca saat start. Ubah isinya,
restart service — tidak perlu build ulang. Sidik jarinya tersimpan di
`document_pages.prompt_hash`, jadi Anda bisa mencari halaman mana yang masih
diproses dengan prompt versi lama.

`fix_page.md` sengaja memuat larangan tegas: jangan mengubah angka apa pun,
jangan mengubah kata yang tidak dikenal (Qanun, Reusam, Keuchik, Propinsi,
Atjeh, Dati II), jangan menambah/menghapus/meringkas.

## Status verifikasi

| Bagian | Status |
|---|---|
| `schema.sql` + trigger label | **Dijalankan sungguhan** di Postgres 16 + pgvector; trigger diuji (label flat terisi otomatis, cascade ke anak/cucu) |
| **Seluruh SQL `internal/store`** | **Diuji VERBATIM** terhadap Postgres nyata lewat PREPARE/EXECUTE, berurutan mengikuti alur: daftar tautan (2 sumber → 1 baris) → unduh → duplikat-hash → klaim OCR (duplikat tidak ikut terklaim) → simpan halaman + fixed_text + prioritas teks → metadata → duplikat-identitas → tolak bukan-peraturan → ocr_done → parse → nodes → kegagalan & batas percobaan → relations. **Semua asersi lulus.** |
| `internal/pipeline/thinking.go` | **Diuji `go test`**: JSON terbungkus markdown tetap terbaca, nilai ngawur ditolak, jawaban non-JSON jadi galat, hasil perbaikan yang meringkas ditolak, ejaan lama disamakan di canonical key |
| `internal/downloader` | **Diuji**: normalisasi URL mempertahankan param identitas (`?id=`), membuang sampah analitik, urutan param tidak berpengaruh |
| `internal/parser` | Suite regresi sebelumnya tetap hijau |
| `internal/localllm/text.go` | **Type-check bersih terhadap API yzma v1.19.0 YANG ASLI** (bukan stub): sumber yzma diunduh, disalin ke modul lokal go1.22, lalu `go build`+`go vet` dijalankan atas localllm — lulus. Perilaku runtime (memuat model sungguhan) tetap belum diuji |
| `main.go`, worker loop | Type-check + `go vet` bersih; loop nyata menempel Postgres belum dijalankan |

**Kirimkan galat kompilasi/runtime apa pun** — perbaikan kemungkinan besar
terbatas di satu berkas.

### Koreksi API yzma (setelah user mengirim contoh resmi jalur teks)

Contoh resmi yzma untuk model teks membongkar tiga kekeliruan yang tidak
terlihat lewat stub:

1. **`llama.Tokenize` mengembalikan `[]Token` saja, tanpa galat.** Kode semula
   menulis `tokens, err := llama.Tokenize(...)` — pasti gagal kompilasi.
2. **Posisi token tidak boleh diatur manual.** `BatchGetOne` mendokumentasikan
   bahwa "posisi token dilacak otomatis oleh Decode"; penyetelan `batch.Pos`
   dihapus dari jalur teks, mengikuti pola contoh resmi (Decode di awal
   perulangan, lalu Sample, lalu batch baru dari token hasil).
3. **Bug laten yang ditemukan saat memeriksa:** `text.go` punya blok
   `sync.Once` sendiri yang HANYA memuat `llama`, tanpa `mtmd`. Karena
   `libsOnce` dipakai bersama, klien mana pun yang kebetulan memuat lebih dulu
   menentukan pustaka yang tersedia — bila model teks menang, model visi rusak
   dengan galat menyesatkan. Kini keduanya melewati satu fungsi `loadLibs()`
   yang memuat llama + mtmd sekaligus.

### Koreksi kedua dari contoh chat resmi yzma

Contoh sesi chat resmi menemukan **bug yang akan membuat proses panic**:

- **`ChatApplyTemplate` mengembalikan UKURAN YANG DIBUTUHKAN, bukan jumlah byte
  yang tertulis.** Bila lebih besar dari buffer, isi buffer tidak lengkap dan
  pemanggil wajib memperbesar lalu mengulang. Kode semula langsung melakukan
  `buf[:n]` — yang panic "slice out of range" begitu hasilnya melebihi buffer.
  Ini bukan kemungkinan teoretis: jalur teks mengirim satu halaman peraturan
  penuh beserta prompt sistem, yang dengan mudah melewati 8 KB. Kini keduanya
  (visi dan teks) memakai `applyChatTemplate()` yang memperbesar buffer sesuai
  ukuran yang diminta llama.cpp lalu mengulang.
- **`TokenToPiece(..., special=false)`** pada jalur teks, mengikuti contoh
  chat resmi: token kontrol tidak lagi dirender menjadi teks biasa sehingga
  tidak bocor ke hasil perbaikan.

### Tampilan kemajuan

Log unduhan dihapus dari konsol (statusnya tetap terlihat di database);
hanya kegagalan yang dicatat. Baris kemajuan OCR kini
`[sudah diperbaiki / sudah di-OCR / total halaman]`, dan **dimulai dari
`[0/0/N]` sebelum halaman pertama dikerjakan**:

```
   [0/0/12] hal 1 · menyiapkan halaman                       0%
   [0/0/12] hal 1 · 1029x1945 px · menyandikan gambar        0%
   [0/0/12] hal 1 · 1029x1945 px · 128 token · 47s           0%
   [0/1/12] hal 1 · 1029x1945 px · dipotong -48% · 1m12s     0%
   [1/1/12] hal 1 · diperbaiki                               8%
   [1/1/12] hal 2 · menyiapkan halaman                       8%
```

Tiga hal yang membuatnya berguna:

- **Mulai dari `[0/0/N]`.** Dokumen langsung terlihat dimulai beserta jumlah
  halamannya, tidak menunggu halaman pertama selesai.
- **Tahap dilaporkan selama OCR berjalan.** Penyandian gambar adalah bagian
  paling lambat dan terjadi sebelum token pertama keluar; tanpa laporan tahap,
  konsol diam beberapa menit dan proses tampak macet padahal normal. Ini juga
  alat pemilah: macet di "menyandikan gambar" dengan dimensi sebesar halaman
  penuh berarti pemotongan margin gagal; dimensi kecil tapi jumlah token terus
  naik berarti model mengoceh (turunkan batas token).
- **Angka pertama tertinggal satu langkah** — halaman di-OCR dulu, baru
  diperbaiki. Bila angka pertama berhenti bergerak sementara angka kedua terus
  naik, yang bermasalah tahap perbaikan, bukan OCR-nya.

## Catatan RAM

Model visi + model teks menempel bersamaan selama antrian belum habis. Pada
mesin 8 GB ini ketat: periksa ukuran `ocr.gguf` + `mmproj` Anda; bila totalnya
dengan `thinking.gguf` mendekati 6 GB, KV cache dan MuPDF (render A4 200 DPI)
bisa membuatnya melewati batas. Bila terjadi OOM, jalan keluar tercepat adalah
menurunkan kuantisasi model teks.

Satu perbaikan penting menyertai perubahan ini: `Release()` tidak lagi
memanggil `llama.Close()`. Backend llama.cpp dipakai **bersama** kedua model,
sehingga membebaskannya saat salah satu model dilepas akan merusak model yang
lain. Pembebasan backend kini hanya di `localllm.Shutdown()`, dipanggil sekali
saat proses berakhir.

## Perubahan status manual lewat SQL

```sql
-- Ringkasan kemajuan
SELECT status, count(*) FROM documents GROUP BY status ORDER BY 2 DESC;

-- Dokumen yang ditolak/duplikat beserta alasannya
SELECT id, download_url, status, reject_reason, jenis, instansi, nomor, tahun
FROM documents WHERE status IN ('rejected','duplicate') ORDER BY updated_at DESC;

-- Coba lagi dokumen yang gagal (kembali ke awal: unduh ulang)
UPDATE documents SET status='pending', attempts=0, last_error=NULL WHERE id='<uuid>';

-- Proses ulang OCR tanpa unduh ulang (berkas sudah ada)
DELETE FROM document_pages WHERE document_id='<uuid>';
UPDATE documents SET status='downloaded', attempts=0 WHERE id='<uuid>';

-- Batalkan tanda "bukan peraturan"/"duplikat" (Anda menilai model salah)
UPDATE documents SET status='downloaded', reject_reason=NULL, duplicate_of=NULL,
       is_peraturan=NULL, attempts=0 WHERE id='<uuid>';

-- Parse ulang saja (OCR & perbaikan dipertahankan)
UPDATE documents SET status='ocr_done' WHERE id='<uuid>';

-- Setelah mengoreksi teks OCR lewat UI, minta parse ulang
UPDATE document_pages SET edited_text='<teks koreksi>', is_edited=true, edited_at=now()
WHERE document_id='<uuid>' AND page_number=<n>;
UPDATE documents SET status='ocr_done' WHERE id='<uuid>';

-- Halaman yang diproses dengan prompt versi lama
SELECT document_id, page_number FROM document_pages WHERE prompt_hash <> '<hash_baru>';

-- Halaman yang paling banyak diubah model (kandidat tinjauan manusia)
SELECT document_id, page_number, fix_ops_count FROM document_pages
ORDER BY fix_ops_count DESC NULLS LAST LIMIT 20;
```

**Mengosongkan semua data kecuali `sources`:** jalankan `reset_data.sql`.

## Berkas yang DIHAPUS — hapus juga di tempat Anda

- `internal/state/` (folder)
- `internal/pipeline/pagestore.go`
- `catter_go.txt`
- Database lama beserta seluruh tabelnya (skema baru tidak kompatibel)

## Berkas BARU

- `schema.sql` (menggantikan yang lama), `reset_data.sql`
- `prompts/classify.md`, `prompts/fix_page.md`
- `internal/prompts/prompts.go`
- `internal/localllm/text.go`
- `internal/pipeline/thinking.go`

Selebihnya diganti isinya: `main.go`, `.env.example`, `internal/config`,
`internal/downloader`, `internal/extractor/extractor.go`, `internal/store`,
`internal/pipeline/*`, `internal/localllm/localllm.go`.
`internal/parser`, `internal/raster`, `internal/logx`, `internal/fsutil`,
`go.mod`, `go.sum` tidak berubah.
