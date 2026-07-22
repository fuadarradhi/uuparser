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

## Langkah menjalankan

1. **Siapkan database** (skema ini tidak kompatibel dengan yang lama — hapus
   dulu database sebelumnya):
   ```bash
   psql -d uuparser -f schema.sql
   ```

2. **Daftarkan sumber.** Belum ada UI untuk ini; lakukan lewat SQL. Contoh
   lengkap ada di bagian "Urutan pengerjaan" di bawah — `priority` menentukan
   sumber mana diselesaikan lebih dulu, `source_type` menentukan implementasi
   pengambil daftar dokumen (`integrasi` untuk situs ber-API, atau nama
   scraper khusus).

3. **Siapkan berkas.** Tata letak bawaan (semuanya bisa ditimpa lewat `.env`):
   ```
   ./uuparser                 binari
   ./models/ocr.gguf          model visi (OCR)
   ./models/ocr.mmproj.gguf   proyektor multimodal
   ./models/thinking.gguf     model teks (klasifikasi + perbaikan)
   ./libs/                    libllama, libmtmd, dan pustaka pendukung
   ./prompts/classify.md      dapat disunting tanpa build ulang
   ./prompts/fix_page.md
   ./data/pdf/                terisi sendiri; nama berkas = hash isinya
   ./log/error.log
   ```

4. **Isi `.env`** — minimal `DATABASE_URL`. Lihat `.env.example`.

5. **Bangun dan jalankan:**
   ```bash
   go mod tidy && go build .
   ./uuparser
   ```
   Tidak ada flag baris perintah. Untuk service systemd, `WorkingDirectory`
   penting karena semua jalur bawaan relatif terhadapnya.

Untuk mengulang dari nol tanpa mendaftarkan sumber lagi: `psql -d uuparser -f
reset_data.sql` (mengosongkan semua tabel kecuali `sources`).

## Urutan pengerjaan

Dokumen terbaru dari sumber terpenting dikerjakan lebih dulu, di **ketiga**
tahap (unduh, OCR, parse):

```sql
ORDER BY COALESCE(s.priority, 1000),          -- sumber utama diselesaikan dulu
         d.sort_tahun DESC NULLS LAST,        -- lalu dokumen terbaru
         d.sort_nomor DESC NULLS LAST,
         d.created_at                          -- pemutus seri
```

Atur prioritas saat mendaftarkan sumber (angka kecil = lebih dulu). Tulis
`source_type` secara eksplisit meski ada nilai bawaannya, supaya terlihat
sumber mana yang memakai API `/integrasi` dan mana yang perlu scraper
tersendiri:

```sql
INSERT INTO sources (code, endpoint_url, source_type, priority) VALUES
 ('acehprov',  'http://jdih.acehprov.go.id/integrasi',      'integrasi',       1),
 ('bandaaceh', 'http://jdih.bandaacehkota.go.id/integrasi', 'integrasi',       2),
 -- contoh sumber tanpa API, ditangani scraper khusus (lihat bagian di bawah)
 ('acehbarat', 'http://jdih.acehbaratkab.go.id/produk-hukum', 'scrape_acehbarat', 3);
```

`sort_tahun` & `sort_nomor` diambil dari `tahun_pengundangan` dan
`noPeraturan` pada `/integrasi`, **hanya untuk mengurutkan** — sengaja
dipisah dari kolom `tahun`/`nomor` yang dibaca model dari halaman pertama
dan menjadi satu-satunya sumber kebenaran identitas. Metadata JDIH yang
sesekali keliru karena itu tidak berbahaya: dampaknya paling jauh urutan
pengerjaan meleset sedikit.

Keduanya bertipe `int`, bukan `text` — kalau teks, "10" akan diurutkan
sebelum "2". Nilai yang tidak berisi angka ("-", kosong) menjadi NULL dan
dikerjakan **paling belakang** (`NULLS LAST`), sesuai permintaan.

Terbukti lewat pengujian pada Postgres nyata (data sengaja diacak):

```
u06 acehprov  2024/1     ← terbaru dari sumber utama
u03 acehprov  2023/10    ← 10 sebelum 9: bukti kolom int bekerja
u04 acehprov  2023/9
u08 acehprov  2023/—     ← nomor kosong: belakangan
u02 acehprov  1979/3
u05 acehprov  —/—        ← tanpa metadata: paling belakang di sumbernya
u07 bandaaceh 2025/1     ← meski 2025, tetap SETELAH semua Aceh
u01 bandaaceh 2024/9
```

### Menambah sumber baru (termasuk yang perlu scraping)

Sumber diakses lewat antarmuka `downloader.Source` sehingga tidak semua harus
memakai `/integrasi`:

```go
type Source interface {
	Code() string
	ListDocuments(ctx context.Context) ([]RemoteDoc, error)
	FetchPDF(ctx context.Context, doc RemoteDoc) ([]byte, error)
}
```

`RemoteDoc` sengaja hanya berisi tiga hal — tautan berkas serta tahun dan
nomor untuk pengurutan — karena metadata sumber selebihnya memang tidak
dipercaya. Ini justru membuat scraper mudah ditulis: tidak perlu memetakan
belasan kolom JDIH.

Menambah sumber ber-API `/integrasi` cukup satu baris INSERT (lihat di atas).
Untuk situs tanpa API, dua langkah dan **tidak menyentuh kode pipeline**:

1. Tulis satu berkas, mis. `internal/downloader/scrape_acehbarat.go`:

```go
func (s *AcehBaratScraper) Code() string { return s.code }

func (s *AcehBaratScraper) ListDocuments(ctx context.Context) ([]RemoteDoc, error) {
	// ambil halaman daftar, temukan tautan PDF, isi:
	//   FileURL   (wajib, lewatkan NormalizeURL)
	//   SortTahun / SortNomor (opsional; nil = dikerjakan paling belakang)
}

func (s *AcehBaratScraper) FetchPDF(ctx context.Context, d RemoteDoc) ([]byte, error) {
	return DownloadPDF(ctx, s.cfg, d.FileURL) // pembantu bersama: cek %PDF,
	                                           // ikuti HTML->PDF, coba ulang
}
```

2. Daftarkan di `downloader.NewSource` (tempatnya sudah disiapkan):

```go
case "scrape_acehbarat":
	return NewAcehBaratScraper(row, cfg), nil
```

Parameter khusus per situs (selector CSS, pola URL halaman daftar) disimpan di
kolom `sources.source_config jsonb` dan diteruskan sebagai
`SourceRow.SourceConfigRaw`, sehingga mengganti selector cukup mengubah data,
bukan kode.

SENGAJA tidak disediakan scraper generik: struktur HTML tiap situs pemerintah
berbeda dan gampang berubah, sehingga satu scraper "serba bisa" akan rapuh dan
gagal diam-diam. Tiap situs mendapat implementasi sendiri yang eksplisit.

### Kalau OCR butuh berkas yang belum terunduh

Tidak perlu mekanisme khusus, dan tidak mungkin terjadi: OCR hanya mengambil
dokumen berstatus `downloaded`, yang menurut definisinya berkasnya sudah ada
di disk. Bila belum ada satu pun, `ClaimForOCR` mengembalikan "tidak ada
pekerjaan", model dilepas dari memori, dan worker tidur 30 detik.

Yang perlu diketahui adalah ketidaksempurnaan kecilnya: OCR mengambil dokumen
terbaru **di antara yang sudah terunduh**, bukan yang terbaru secara mutlak.
Bila pengunduh belum sampai ke dokumen 2025 sementara dokumen 2019 sudah
terunduh, OCR mengerjakan yang 2019 lebih dulu. Dalam praktiknya ini nyaris
tak terasa — mengunduh satu berkas hitungan detik sedangkan OCR satu dokumen
hitungan menit, jadi pengunduh selalu jauh di depan, dan karena ia pun
mengunduh dalam urutan yang sama, yang terunduh lebih dulu memang yang
terbaru. Mengurutkan hasil `/integrasi` sebelum insert tidak mengubah apa
pun: urutan ditentukan `ORDER BY` saat pengambilan, bukan urutan baris masuk.

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

## Dua lapis teks per halaman

| Kolom | Isi | Ditimpa? |
|---|---|---|
| `ocr_text` | mentah dari model visi | **tidak pernah** |
| `edited_text` | koreksi manusia dari UI | hanya oleh manusia |

Parser membaca `COALESCE(edited_text, ocr_text)`.

**Tahap "perbaikan salah ketik oleh model teks" DIHAPUS** (2026-07-21).
Setelah membandingkan keluaran OCR mentah dengan hasil perbaikannya, perbaikan
itu tidak memberi manfaat yang sepadan dengan biayanya — satu panggilan model
per halaman, dengan risiko mengubah istilah lokal dan ejaan lama. Kolom
`fixed_text`, `fix_ops_count`, dan `prompt_hash` ikut dihapus.

Model teks kini dipakai HANYA untuk membaca metadata yang sulit diuraikan
parser.

## Pembagian tugas: model membaca, kode menyimpulkan

Ini prinsip yang menentukan bentuk seluruh bagian model teks.

**Model hanya menyalin apa yang tertulis.** Pemetaan jabatan ke badan
pemerintahan bersifat pasti dan berkaidah tetap, jadi dikerjakan kode di
`internal/pipeline/normalize.go` — bukan diserahkan ke model kecil yang bisa
keliru dan sulit diaudit:

```
GUBERNUR ACEH                  -> PEMERINTAH ACEH
BUPATI ACEH BARAT              -> KABUPATEN ACEH BARAT
WALI KOTA BANDA ACEH           -> KOTA BANDA ACEH
PROPINSI DAERAH ISTIMEWA ACEH  -> PEMERINTAH ACEH
```

Keduanya disimpan: `instansi_tertulis` (apa yang tercetak) dan `instansi`
(hasil pemetaan), sehingga hasilnya selalu dapat ditelusuri balik.

**Nomor peraturan disimpan dua bentuk.** Nomor keputusan kerap bukan angka
tunggal, mis. `300.2/ 69 /2026`:

| Kolom | Isi | Kegunaan |
|---|---|---|
| `nomor` | `300.2/ 69 /2026` | bentuk resmi, tidak pernah hilang |
| `nomor_urut` | `300` | pengurutan saja |

Kunci duplikat memakai nomor ASLI, bukan angka urutnya — dua keputusan berbeda
dapat berbagi angka pertama yang sama (`300.2/ 69 /2026` dan
`300.2/ 70 /2026`).

## Prompt dipecah menjadi pertanyaan-pertanyaan kecil

`prompts/gate.md` (produk hukum atau bukan) · `prompts/identity.md` (jenis,
instansi, nomor, tahun, tentang) · `prompts/penetapan.md` (tempat, tanggal,
penanda tangan).

Alasannya konkret: satu prompt panjang yang meminta banyak hal sekaligus
membuat model kecil kehilangan sebagian instruksi. Gejala nyatanya — model
menjawab `KEPUTUSAN ...` lengkap dengan titik-titik, karena **menyalin
placeholder di dalam prompt** sebagai jawaban. Prompt sekarang tidak lagi
memuat `...` atau `<...>` sama sekali, dan `tolakPlaceholder()` di
`thinking.go` membuang jawaban yang tetap berbentuk placeholder — menyimpannya
berarti mencatat isi prompt sebagai data.

## Model dipanggil hanya saat aturan pasti gagal

`internal/pipeline/trigger.go` menentukan kapan model teks perlu dijalankan:

```
tidak ada penanda "Ditetapkan di"  -> model TIDAK dipanggil
penanda ada & pola baku cocok      -> diuraikan regex, model TIDAK dipanggil
penanda ada & pola menyimpang      -> model dipanggil untuk teks itu saja
```

Hasil penguraian pasti selalu didahulukan; model hanya mengisi bagian yang
masih kosong. Pola ini dapat diperluas ke bagian lain yang sulit bagi parser —
tambahkan penanda dan pola bakunya di `trigger.go`, lalu satu prompt sempit di
`prompts/`.

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

### Penjaga permukaan API (internal/apicheck)

Berkas `internal/apicheck/apicheck.go` menyatakan ulang method `localllm` yang
dipakai `main.go` dan `internal/pipeline` sebagai antarmuka, lalu menegaskan
bahwa `*localllm.Client` dan `*localllm.TextClient` memenuhinya.

Ini bukan hiasan. Pernah terjadi sebuah method hanya tertambah pada tiruan
yang dipakai saat pengujian, sementara kode aslinya tidak — seluruh pengujian
lulus, tetapi pemakai mendapat galat kompilasi
`has no field or method Warmup`. Berkas ini membuat penyimpangan semacam itu
gagal saat DIBANGUN, bukan saat dipakai. Sudah diuji: menghapus `Warmup` dari
kode asli langsung membuat paket ini menolak dibangun.

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

### Galat "gagal menerapkan chat template" (diperbaiki)

Gejala: setiap dokumen gugur pada `klasifikasi halaman 1: gagal menerapkan
chat template`.

Sebabnya: contoh resmi yzma memakai **cadangan dua tingkat** —

```go
if *template == "" { *template = llama.ModelChatTemplate(model, "") }
if *template == "" { *template = "chatml" }   // ← yang terlewat
```

Kode semula langsung memakai hasil `ModelChatTemplate` tanpa cadangan. Bila
model tidak menyertakan chat template (atau bentuknya belum dikenali versi
llama.cpp yang terpasang), nilainya kosong dan `llama_chat_apply_template`
mengembalikan −1 sehingga SELURUH dokumen gagal di langkah pertama.

Sekarang: template model dicoba lebih dulu, bila ditolak jatuh ke `chatml`,
dan pesan galatnya menyebutkan template mana saja yang sudah dicoba.

**Yang perlu Anda perhatikan:** cadangan `chatml` mencegah kegagalan, tetapi
belum tentu format yang tepat untuk model Anda. Memakai format keliru tidak
menimbulkan galat — hanya menurunkan mutu jawaban secara diam-diam. Karena itu
saat pertama menjalankan, periksa log:

- Tidak ada peringatan → template bawaan model dipakai, semuanya normal.
- Ada peringatan "tidak menyertakan chat template" → setel `CHAT_TEMPLATE` di
  `.env` sesuai keluarga model Anda (`gemma`, `llama3`, `chatml`, `mistral`).

## Ketahanan terhadap penghentian (Ctrl+C) dan galat model

Prinsipnya satu: **gangguan pada sisi kita tidak boleh menjadi kerusakan pada
data.** Pembatalan, model gagal dimuat, atau database sesaat tak terjangkau
adalah masalah sementara — dokumen dikembalikan ke antrian apa adanya dan
dilanjutkan nanti, tanpa menambah penghitung kegagalan. Hanya kerusakan
sungguhan (PDF tak terbaca, dst.) yang menambah `attempts` dan akhirnya
berstatus `failed`.

Sebelum meng-OCR halaman baru, dokumen yang dilanjutkan melewati tahap
pemulihan dengan urutan yang disengaja:

1. **Hitungan kemajuan dipulihkan dari basis data**, sehingga baris kemajuan
   meneruskan angka sebelumnya alih-alih menampilkan `[0/0/N]` padahal
   sebagian halaman sudah selesai.
2. **Perbaikan yang tertunda diselesaikan** (halaman punya teks OCR tetapi
   belum diperbaiki).
3. **Klasifikasi dipastikan sudah berjalan.** Ini yang paling penting:
   pemeriksaan "ini peraturan atau bukan" semula hanya berjalan saat halaman 1
   diproses. Bila halaman 1 sudah ada dari penjalanan sebelumnya, tahap OCR
   melewatinya — sehingga dokumen diteruskan sampai selesai TANPA PERNAH
   diperiksa, dan dokumen bukan-peraturan pun lolos ke tahap parse. Sekarang
   klasifikasi dijalankan dari teks halaman 1 yang tersimpan, tanpa OCR ulang.

Jumlah halaman juga diambil dari PDF di muka, supaya persentase pada tahap
perbaikan tertunda tidak tampil 0% terus.

Yang dijamin saat Anda menekan Ctrl+C lalu menjalankan ulang:

- **Dokumen yang sedang dikerjakan dilanjutkan lebih dulu**, mendahului
  dokumen mana pun termasuk yang lebih baru — menyelesaikan yang separuh jalan
  lebih berharga daripada memulai yang baru. Dokumen berstatus `processing`
  dan `downloading` ikut diambil kembali; tanpa itu keduanya tertinggal
  selamanya karena tidak pernah cocok dengan penyaring status mana pun.
- **Halaman yang sudah di-OCR tidak di-OCR ulang**, dan halaman yang sudah
  di-OCR **tetapi belum diperbaiki** dikerjakan lewat langkah susulan
  tersendiri. Tanpa langkah itu halaman tersebut tak akan pernah diperbaiki:
  tahap OCR melewatinya dengan alasan "halaman sudah ada".
- **Klasifikasi halaman 1 tidak diulang** bila metadatanya sudah pernah dibaca.
- **Penghentian tidak menambah `attempts`.** Sebelum perbaikan ini, beberapa
  kali Ctrl+C sudah cukup membuat dokumen yang sehat berakhir `failed`.
- **Pembersihan memakai konteksnya sendiri.** Setelah konteks utama
  dibatalkan, perintah SQL apa pun dengan konteks itu ikut gagal — termasuk
  perintah yang seharusnya mengembalikan dokumen ke antrian. Operasi
  pembersihan kini memakai konteks segar berbatas 10 detik.

### Menghentikan proses (Ctrl+C)

Tekanan **pertama** meminta berhenti dengan tertib; tekanan **kedua** keluar
seketika.

Jalan keluar kedua itu perlu, bukan kemewahan: inferensi model adalah satu
panggilan panjang yang tidak dapat disela di tengah jalan. Penyandian gambar
halaman besar bisa memakan beberapa menit, dan selama itu pembatalan konteks
belum berpengaruh sama sekali — sehingga Ctrl+C terasa tidak berfungsi.
Progres tetap aman karena tiap halaman disimpan ke basis data begitu selesai.

Satu perbaikan menyertainya: model **tidak** dilepas saat proses berakhir.
`Release()` menunggu kunci yang sedang dipegang inferensi yang berjalan,
sehingga proses tertahan sampai inferensi selesai — persis yang membuat Ctrl+C
tampak mati. Pada saat proses berakhir, sistem operasi yang membebaskan
memorinya.

### Klasifikasi memakai halaman berisi pertama

Pemeriksaan "ini peraturan atau bukan" semula terikat pada halaman 1. Bila
halaman 1 ternyata kosong (artefak pindaian: lembar sampul terpindai polos,
halaman tergeser), tahap halaman-kosong keluar lebih awal dan pemeriksaan itu
**hilang sama sekali** — dokumen diproses sampai selesai tanpa pernah
diperiksa. Sekarang pemeriksaan bergeser ke halaman berisi berikutnya, baik
pada pemrosesan baru maupun saat melanjutkan.

### Galat model tidak lagi merusak data

Sebelumnya `fixPage` menelan SEMUA galat dan mengembalikan "tidak berhasil",
sehingga pembatalan pun tercatat sebagai "keluaran model tidak layak" dan
halaman ditandai selesai **tanpa perbaikan, selamanya**. Sekarang keduanya
dibedakan tegas:

| Keadaan | Akibat |
|---|---|
| Galat (pembatalan, model gagal dimuat, konteks habis) | Dokumen dikembalikan ke antrian; halaman TIDAK ditandai selesai |
| Model menjawab tapi tak layak (kosong / menyusut >40% = meringkas) | Memakai teks OCR mentah, lanjut — di sini memang benar demikian |

### Konsol tidak lagi diam saat model teks bekerja

Perbaikan dan klasifikasi berjalan di CPU dan bisa memakan menit per halaman.
Sebelumnya tidak ada satu pun laporan selama itu, sehingga proses yang sehat
tampak menggantung — kelas masalah yang sama seperti pada OCR dulu, hanya
berpindah tempat. Kini tahap dan jumlah token dilaporkan berkala:

```
   [0/1/3] hal 1 · perbaikan: model teks membaca      0%
   [0/1/3] hal 1 · perbaikan: 128 token               0%
   [1/1/3] hal 1 · diperbaiki                        33%
   [1/1/3] hal 1 · klasifikasi: 64 token             33%
```

Batas keluaran model teks juga diturunkan dari batas mutlak 4096 token menjadi
**1,5× panjang masukan**: hasil perbaikan seharusnya seukuran aslinya, jadi
model yang mengoceh dihentikan jauh lebih cepat.

### Konsol hanya menampilkan kemajuan; catatan masuk ke berkas

Sebelumnya kemajuan dan catatan berebut konsol yang sama. Karena baris
kemajuan hidup di satu baris yang terus ditimpa (`\r`), setiap catatan yang
muncul harus menghapusnya lebih dulu — sehingga kemajuan terus-menerus lenyap
dan konsol menjadi campur aduk. Pemisahannya kini tegas:

| Tujuan | Isi |
|---|---|
| Konsol | HANYA baris kemajuan (satu baris stabil yang ditimpa di tempat) |
| `log/info.log` | seluruh catatan: pemuatan model, dokumen dikenali, halaman disimpan, dilewati, dst. |
| `log/error.log` | peringatan dan kegagalan saja |

Pengecualian tunggal: `Fatal` (kegagalan yang menghentikan aplikasi) tetap
tampil di konsol — kalau tidak, aplikasi seolah berhenti tanpa sebab. Lokasi
kedua berkas diberitahukan sekali saat mulai.

Tampilan konsol menjadi:

```
memuat model OCR  : ./models/ocr.gguf
memuat model teks : ./models/thinking.gguf
kedua model siap.
   [1/2/12] hal 2 · perbaikan: 128 token   8%
```

Satu baris kemajuan itu diperbarui di tempat dan tidak pernah lagi tertimpa.
Bila keluaran bukan terminal (mis. journald), tiap pembaruan ditulis sebagai
baris tersendiri.

**Kedua model dimuat di muka, sebelum pekerjaan dimulai.** Memuat model
beberapa GB memakan waktu lama; melakukannya di awal berarti waktu tunggu itu
terjadi saat pemakai memang sedang menunggu — bukan mendadak di tengah
pekerjaan, yang membuat proses tampak berhenti tanpa sebab. Pemuatan awal ini
sekaligus memastikan kedua berkas model memang dapat dipakai sebelum satu
dokumen pun tersentuh.

Bila model sempat dilepas saat antrian kosong lalu dimuat lagi, pemuatannya
tetap dilaporkan lewat baris kemajuan sehingga baris itu tidak pernah membeku
tanpa penjelasan.

### Catatan memori

Bawaannya kedua model menempati memori bersamaan selama masih ada antrian.
Bila mesin Anda tidak cukup, setel `LOW_MEMORY=true`: kedua model bergantian
(visi dilepas sebelum teks dipakai, dan sebaliknya), dengan harga pemuatan
ulang model tiap halaman. Pemuatan model diumumkan sebelum dimulai beserta
ukuran berkasnya, sehingga bila proses benar-benar berhenti di tahap itu,
baris terakhir menunjukkan penyebabnya.

Satu perbaikan penting menyertai perubahan ini: `Release()` tidak lagi
memanggil `llama.Close()`. Backend llama.cpp dipakai **bersama** kedua model,
sehingga membebaskannya saat salah satu dilepas akan merusak model yang lain.
Pembebasan backend kini hanya di `localllm.Shutdown()`, sekali saat proses
berakhir.

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
