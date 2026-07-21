# uuparser

> **⚠️ Baca `CATATAN-MIGRASI.md` dulu.** Proyek ini baru saja migrasi total ke
> Postgres (2026-07-20): tidak ada lagi file perantara, tidak ada flag CLI,
> arsitektur jadi tiga worker independen. Sebagian isi README di bawah ini
> (tata letak folder, flag CLI, output JSON) menjelaskan desain LAMA dan
> **sudah tidak berlaku** — dibiarkan di sini karena bagian mekanisme
> OCR/crop/GLM-OCR-nya masih akurat dan berguna. `CATATAN-MIGRASI.md` adalah
> sumber kebenaran untuk arsitektur saat ini.

Service yang menjaga agar setiap peraturan baru pada endpoint JDIH `/integrasi`
otomatis terunduh, di-OCR, dirapikan, lalu di-parse menjadi JSON terstruktur
(Pasal/ayat/huruf) yang tinggal disimpan ke database.

Ini **aplikasi**, bukan pustaka — root hanya berisi berkas Go (`go.mod`,
`go.sum`, `main.go`), seluruh kode ada di `internal/`.

## Banyak sumber

Satu proses menangani banyak situs JDIH sekaligus. Data tiap sumber dipisah ke
foldernya sendiri, sehingga nama berkas tak bertabrakan antar-daerah dan jumlah
berkas per folder tetap wajar:

```
data/
  _last_run.json                       ringkasan siklus terakhir
  acehprov/
    integrasi.json                     metadata mentah

    pdf/<slug>.pdf                     ada berkas   = selesai
    ocr/<slug>/pageN.txt               OCR, per halaman
    json/<slug>.json                   ada json     = selesai
    failed/<slug>.json                 catatan gagal + jumlah percobaan
  acehbesarkab/
    ...
  bandaacehkota/
    ...
```

Kode sumber diturunkan otomatis dari hostname endpoint:
`jdih.acehprov.go.id` → `acehprov`, `jdih.acehbesarkab.go.id` → `acehbesarkab`.

```
./uuparser -endpoints "http://jdih.acehprov.go.id/integrasi,http://jdih.acehbesarkab.go.id/integrasi,http://jdih.bandaacehkota.go.id/integrasi"
```

## Siklus & ketahanan

Setiap siklus memproses tiap sumber secara berurutan. Di dalam satu sumber,
**tiap dokumen diselesaikan penuh sebelum lanjut ke dokumen berikutnya**:

```
dokumen 1: unduh → OCR → perbaiki → parse → json
dokumen 2: unduh → OCR → perbaiki → parse → json
...
```

Dengan begitu JSON pertama muncul dalam hitungan menit, bukan setelah ribuan
unduhan selesai — penting saat masih menyetel kualitas OCR.

Bila Anda justru ingin mengamankan seluruh PDF lebih dulu (mis. khawatir situs
sumber hilang), pakai `DOWNLOAD_FIRST`: semua PDF diunduh dulu, baru OCR.
Setelah semua sumber selesai, service tidur `INTERVAL` (default 60 menit) lalu
mengulang, sehingga dokumen baru langsung tertangkap.

Deteksi "sudah selesai" sepenuhnya dari keberadaan berkas — tanpa database.

- **Resume per halaman.** Bila OCR baru sampai halaman 4, siklus berikutnya
  meneruskan dari halaman 5 — tidak mengulang yang sudah selesai.
- **Penulisan atomik.** Semua berkas ditulis lewat berkas sementara lalu di-rename,
  sehingga service yang mati mendadak tidak meninggalkan berkas separuh yang
  terlanjur dianggap selesai.
- **Batas percobaan.** Kegagalan dicatat di `failed/<slug>.json` beserta jumlah
  percobaan; setelah `MAX_ATTEMPTS` (default 3) dokumen dilewati, agar satu PDF
  rusak tidak dicoba ulang tiap jam selamanya. Hapus berkas itu untuk mencoba lagi.
- **OCR kosong ditangani.** Hasil OCR kosong dicoba ulang sekali; bila tetap kosong,
  halaman dicatat di `ocr/<slug>/_empty_pages.json` dan muncul sebagai
  `extraction_notes` pada JSON akhir, bukan diam-diam dianggap sukses.
- **Endpoint mati bukan masalah.** Bila `/integrasi` tak terjangkau, siklus tetap
  melanjutkan pekerjaan yang tertinggal memakai `integrasi.json` yang tersimpan.
- Aman dihentikan (Ctrl-C) kapan saja — progres sudah ada di disk.

### Gate awal (hemat OCR)

Meng-OCR PDF 1000 halaman yang ternyata bukan peraturan itu mubazir. Hanya
`PROBE_PAGES` halaman pertama (default 5) yang di-OCR, lalu diperiksa: bila tak
ada satu pun sinyal peraturan (Pasal / BAB / Menimbang / Mengingat / MEMUTUSKAN /
judul peraturan), sisa halaman **tidak** di-OCR. Dokumen ditandai
`ocr/<slug>/_rejected.json` (berisi cuplikan teks untuk audit) dan dilewati pada
siklus berikutnya.

Gerbang ini sengaja longgar — satu sinyal cukup untuk melanjutkan — karena
salah-menolak peraturan asli lebih merugikan daripada meng-OCR beberapa halaman
ekstra. Bila ada dokumen tertolak padahal seharusnya diproses (mis. scan halaman
awal rusak), **hapus `_rejected.json`** dan siklus berikutnya memprosesnya lagi.

## Prasyarat

Model dijalankan **langsung di dalam proses** lewat
[yzma](https://github.com/hybridgroup/yzma), pengikat Go untuk llama.cpp. Tidak
ada server inferensi yang perlu dijalankan dan tidak ada permintaan keluar —
satu-satunya lalu lintas jaringan aplikasi ini adalah pengunduhan PDF.

Tata letak bawaan meletakkan semuanya di sebelah binari, sehingga service dapat
dijalankan tanpa mengisi konfigurasi apa pun:

```
uuparser
models/ocr.gguf              model GGUF
models/ocr.mmproj.gguf       proyektor multimodal (wajib untuk model visi)
libs/                        libllama, libmtmd, dan pustaka pendukungnya
log/error.log                dibuat sendiri
data/                        hasil unduhan & keluaran
.env
```

Lokasi lain dapat ditunjuk lewat `MODEL_PATH`, `MMPROJ_PATH`, `LIB_PATH`
(atau variabel lingkungan `YZMA_LIB`), dan `LOG_DIR`. Ketiadaan berkas model
dilaporkan saat mulai, lengkap dengan jalur yang dicari.

Untuk membangun juga diperlukan compiler C (go-fitz/MuPDF):
`sudo apt install build-essential`.

## Menjalankan

```
cp .env.example .env      # umumnya cukup mengisi ENDPOINTS
go mod tidy               # sekali saja, mengambil dependensi
go build -o uuparser .

./uuparser                # service: siklus penuh, ulang tiap INTERVAL
./uuparser -once          # satu siklus lalu keluar
./uuparser -env prod.env  # pakai berkas konfigurasi lain
./uuparser -render x.pdf  # render PDF jadi PNG untuk uji manual, lalu keluar
```

### Pemuatan model

Model dimuat **sekali** lalu dipakai untuk seluruh dokumen dan seluruh halaman —
tidak ada buka-tutup per dokumen. Pemuatan bersifat malas: baru terjadi ketika
halaman pertama benar-benar perlu di-OCR, sehingga siklus yang tidak menemukan
pekerjaan tidak menyita memori sama sekali.

Bila pustaka atau berkas model tidak dapat disiapkan, siklus dihentikan dengan
pesan yang jelas — bukan ditandai sebagai kegagalan tiap dokumen, sehingga satu
pustaka yang belum terpasang tidak menghabiskan jatah percobaan seluruh dokumen.

Ketika sebuah siklus selesai dan tidak ada lagi yang perlu di-OCR, model dilepas
dari memori selama service menunggu siklus berikutnya, lalu dimuat ulang sendiri
saat ada dokumen baru:

```
model dimuat: /path/ke/GLM-OCR.gguf
  ...
model dilepas dari memori
menunggu 60m0s sebelum siklus berikutnya...
```

## Log

**Konsol** — ringkas, berwarna, dengan penomoran dokumen dan kemajuan halaman:

```
═══ Siklus #1 — 2026-07-19 15:41:07 — 2 sumber

▸ acehprov  http://jdih.acehprov.go.id/integrasi
[1/128] Perda_Aceh_No_5_Tahun_2023_ttg_Keterbukaan_Informasi
   unduh    1.2 MB
   OCR      4/12 (33%) 1029x1955 px (-52.0% piksel) · 4s
   · hal 5/12 — kosong, dilewati
   ! hal 6/12 — keluaran berulang, dipangkas
   ✓ parse · pasal=27 ayat=63 relasi=2
[2/128] NA_Raqan_Grand_Design
   ✗ OCR gagal (percobaan 2): evaluasi gambar gagal (kode 1)

── Siklus selesai dalam 4m12s · 3 unduh baru · 1 OK · 1 peringatan · 1 gagal
```

Hijau untuk berhasil, kuning untuk perlu ditinjau, merah untuk gagal, abu-abu
untuk yang dilewati. Baris kemajuan OCR menimpa dirinya sendiri di terminal.

**Warna dan kemajuan otomatis dinonaktifkan bila keluaran bukan terminal** —
misalnya saat ditangkap journald — sehingga log systemd tidak dipenuhi kode
escape maupun baris yang saling menimpa. Variabel `NO_COLOR` juga dihormati.

**Berkas galat** — setiap kegagalan dicatat permanen ke `log/error.log` beserta
waktu dan konteksnya:

```
2026-07-19 15:40:38  [Perda_SGLang] model tidak dapat disiapkan: memuat pustaka llama: ...
2026-07-19 15:52:11  [NA_Raqan_Grand_Design] OCR gagal (percobaan 2): evaluasi gambar gagal (kode 1)
```

Ini terpisah dari keluaran konsol karena keluaran konsol dapat hilang tergulung,
sementara kegagalan pemrosesan perlu dapat ditelusuri kemudian. Berkas digulung
sendiri setelah melewati 5 MB (menjadi `error.log.1`), dan tiap baris langsung
di-`sync` ke disk agar tidak hilang bila service mati mendadak.

### Menjalankan sebagai service systemd

```ini
[Unit]
Description=uuparser
After=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/uuparser
ExecStart=/opt/uuparser/uuparser
Restart=on-failure
RestartSec=30

[Install]
WantedBy=multi-user.target
```

`WorkingDirectory` penting: seluruh jalur bawaan (`models/`, `libs/`, `log/`,
`data/`) relatif terhadapnya.

## Daftar konfigurasi

17 kunci `.env` dan 6 flag. Semuanya punya nilai bawaan — isi hanya yang perlu diubah.

### Flag baris perintah

| Flag | Bawaan | Keterangan |
|---|---|---|
| `-env` | `.env` | berkas konfigurasi yang dipakai |
| `-once` | — | jalankan satu siklus lalu keluar |
| `-render <pdf>` | — | render PDF menjadi PNG lalu keluar, tanpa memanggil model |
| `-render-out` | `render` | folder keluaran `-render` |
| `-limit` | `0` | batasi N dokumen pertama per sumber (uji coba) |
| `-ids` | — | hanya proses `idData` tertentu, dipisah koma |

`-limit` dan `-ids` sengaja **tidak** disimpan di `.env`: keduanya alat uji sesaat,
dan nilai uji coba yang tertinggal akan diam-diam membuat service produksi hanya
memproses sebagian dokumen.

### Sumber data

| Kunci | Bawaan | Keterangan |
|---|---|---|
| `ENDPOINTS` | *(Aceh)* | Daftar URL `/integrasi`, dipisah koma. Tiap sumber mendapat foldernya sendiri, dinamai dari nama host (`jdih.acehprov.go.id` → `acehprov`). |
| `DATA_DIR` | `data` | Folder akar penyimpanan. Gunakan folder berbeda untuk membandingkan pengaturan tanpa menimpa hasil lama. |
| `INTERVAL` | `60m` | Jeda antar siklus pemeriksaan dokumen baru. Format: `30m`, `2h`, `90s`. |

### Model

| Kunci | Bawaan | Keterangan |
|---|---|---|
| `MODEL_PATH` | `./models/ocr.gguf` | Berkas model GGUF. |
| `MMPROJ_PATH` | `./models/ocr.mmproj.gguf` | Berkas proyektor multimodal. Tanpa ini gambar tidak dapat diproses. |
| `LIB_PATH` | `./libs` | Folder berisi `libllama`/`libmtmd`. Dapat juga diatur lewat variabel lingkungan `YZMA_LIB`. |
| `LOG_DIR` | `./log` | Folder berkas log; kegagalan dicatat ke `error.log`. |
| `VERBOSE` | `false` | Tampilkan log llama.cpp. |

Parameter inferensi lain — suhu, top-k, top-p, ukuran konteks, sampler — bersifat
tetap di dalam kode dan tidak disediakan sebagai pengaturan. Suhu dipatok `0`
agar hasil OCR dapat diulang persis; pada suhu 0 pengambilan token bersifat
greedy sehingga parameter lain tidak berpengaruh.

### Prompt

| Kunci | Bawaan | Keterangan |
|---|---|---|
| `OCR_PROMPT` | `Text Recognition:` | GLM-OCR **hanya** mengenali `Text Recognition:`, `Table Recognition:`, `Figure Recognition:`, `Formula Recognition:`. |
| `OCR_MAX_TOKENS` | `2048` | Batas token keluaran per halaman — pengaman bila model tidak berhenti sendiri. |

### Render halaman

| Kunci | Bawaan | Keterangan |
|---|---|---|
| `DPI` | `200` | Resolusi rasterisasi. Menentukan ketajaman huruf, jadi berpengaruh langsung pada ketelitian pembacaan angka pasal/ayat. |
| `AUTO_CROP` | `true` | Potong margin kosong halaman — cara utama mempercepat OCR tanpa mengorbankan ketelitian. Lihat penjelasan di bawah. |
| `PROBE_PAGES` | `5` | Halaman awal yang di-OCR untuk menguji apakah berkas ini benar peraturan. Bila tak ada tandanya, sisa halaman tidak di-OCR sama sekali. |
| `BLANK_INK` | `0.0004` | Ambang proporsi piksel gelap; di bawah ini halaman dianggap kosong dan tidak dikirim ke model. |
| `SAVE_PNG` | `false` | Simpan gambar yang dikirim ke model ke `data/<sumber>/png/<slug>/`, untuk diperiksa manual. |

### Unduhan & ketahanan

| Kunci | Bawaan | Keterangan |
|---|---|---|
| `DELAY_MS` | `1500` | Jeda antar permintaan unduh, agar sopan terhadap server pemerintah. |
| `MAX_ATTEMPTS` | `3` | Batas percobaan per dokumen sebelum dilewati (`0` = tanpa batas). Hapus `data/<sumber>/failed/<slug>.json` untuk mencoba lagi. |
| `DOWNLOAD_FIRST` | `false` | Bila `true`, semua PDF diunduh dulu baru OCR. Bawaannya per dokumen agar JSON pertama muncul dalam hitungan menit. |

### Kecepatan tanpa mengorbankan ketelitian

Pengecilan gambar sengaja **tidak disediakan**: mengecilkan huruf membuat angka
pasal dan ayat rawan salah baca, dan satu digit keliru merusak struktur seluruh
dokumen. Angka tidak dapat ditebak dari konteks bahasa seperti kata biasa.

`AUTO_CROP` mempercepat tanpa risiko itu — margin kosong dibuang, ukuran huruf
tidak berubah sedikit pun. Pemotongan bersifat agresif: halaman A4 yang hanya
berisi satu baris terpotong menjadi seukuran baris itu saja.

| Isi halaman A4 | Sebelum | Sesudah | Piksel tersisa |
|---|---|---|---|
| 1 baris | 1653×2339 | 907×49 | **1,1%** |
| 2 baris | 1653×2339 | 701×83 | **1,5%** |
| Penuh teks | 1653×2339 | 1029×1955 | 52% |

Pengamannya bukan batas luas minimum — halaman satu baris memang seharusnya
menghasilkan potongan yang sangat kecil. Yang dijaga adalah keutuhan isi:

1. Kotak isi dicari dari kepadatan tinta per baris dan kolom (bukan piksel
   tunggal), sehingga bintik noda dan bayangan tepi hasil pindai tidak
   menggagalkan pemotongan.
2. Hasilnya diperiksa wajib menahan sekurangnya 99,5% piksel bertinta halaman.
   Bila ada baris samar yang nyaris terlewat, deteksi diulang memakai ambang
   paling ketat sehingga tidak ada isi yang terbuang.
3. Pemotongan dibatalkan bila hasilnya tidak masuk akal atau nyaris sama dengan
   halaman aslinya.

### Menyimpan gambar halaman untuk uji manual

Dua cara mendapatkan PNG yang **persis sama** dengan yang dikirim ke model, untuk
diuji ulang lewat CLI atau memeriksa apakah pemotongan margin sudah tepat.

Mode render mandiri — tanpa menjalankan pipeline, tanpa memanggil model:

```
./uuparser -render data/acehprov/pdf/<slug>.pdf
```

```
render ...pdf — 12 halaman, DPI=200 AUTO_CROP=true
  render/<slug>/page1.png — 1029x1955 px, 461 KB, tinta 16.125%, 174ms
  render/<slug>/page2.png — 907x49 px, 9 KB, tinta 12.911%, 98ms
  ...
```

Atau `SAVE_PNG=true` agar gambar ikut ditulis ke `data/<sumber>/png/<slug>/`
selama pipeline berjalan.

### Catatan: bug pengulangan pada Ollama

Bila sebelumnya Anda memakai GLM-OCR lewat **Ollama** dan menemui keluaran yang
mengulang tanpa henti, itu bug pengemasan di Ollama
([ollama/ollama#16892](https://github.com/ollama/ollama/issues/16892), masih
terbuka) — bukan pada model maupun llama.cpp. Menjalankan model langsung lewat
llama.cpp/yzma tidak mengalaminya, sehingga sampler DRY tidak perlu diaktifkan.

Pengaman tetap dipasang untuk berjaga-jaga:

1. **Batas token keluaran** (`OCR_MAX_TOKENS`) memutus keluaran yang tidak
   berhenti sendiri.
2. **Halaman kosong dilewati** — tidak dikirim ke model sama sekali.
3. **Pemangkasan keluaran berulang** — baris/token identik yang berulang
   berlebihan dipangkas, dan halaman yang keluarannya didominasi pengulangan
   ditandai untuk ditinjau.
4. **Deteksi keluaran terpotong** — halaman yang berhenti karena batas token
   ditandai, karena teks yang terpotong diam-diam lebih berbahaya daripada
   kegagalan yang terlihat.

Semua penandaan tercatat di `data/<sumber>/ocr/<slug>/_page_notes.json` dan ikut
muncul pada `extraction_notes` di JSON akhir.

## Keluaran

`data/<sumber>/json/<slug>.json`:

```json
{
  "source":    { "sumber": "acehprov", "id_data": "...", "judul": "...", "slug": "...", "parsed_at": "..." },
  "report":    { "status": "SUCCESS|WARNING|FAIL", "issues": [...], "stats": {...} },
  "relations": [ { "type": "mencabut", "key": "peraturan_daerah|5|2010",
                   "jenis": "PERATURAN DAERAH", "instansi": "KABUPATEN ACEH BESAR",
                   "nomor": "5", "tahun": "2010", "tentang": "Izin Gangguan",
                   "confidence": "tinggi", "kutipan": "..." } ],
  "extraction_notes": [ "OCR menghasilkan teks kosong pada halaman 3 — ..." ],
  "result":    { "nodes": [ ... ] }
}
```

`result.nodes` = baris siap di-loop insert ke DB.

`_last_run.json` berisi ringkasan siklus terakhir per sumber (jumlah unduhan,
OK/WARNING/FAIL, ditolak, menyerah) untuk memantau kesehatan service tanpa
mengorek log.

### Relasi antar-peraturan

Kalimat pencabutan/perubahan diekstrak **deterministik** (pola + kata kerja),
bukan lewat model bahasa: kalimatnya sangat baku sehingga pola dapat diandalkan
dan hasilnya dapat diaudit lewat field `kutipan`. Model bahasa berisiko mengarang
nomor/tahun peraturan — kesalahan yang sulit terdeteksi.

| type | arti |
|---|---|
| `mencabut` | peraturan lain dicabut/dinyatakan tidak berlaku |
| `mengubah` | perubahan atas peraturan lain |
| `dasar_hukum` | disebut pada bagian Mengingat |
| `disebut` | dirujuk, maksud belum jelas → `confidence: perlu_review` |

`key` adalah kunci kanonik (`jenis|nomor|tahun`, tanpa nama instansi) agar bisa
langsung di-join ke baris peraturan lain di database. Bila satu peraturan sudah
punya hubungan kuat, entri `disebut` untuknya ditekan agar tidak jadi kebisingan.

## Diagnosa hasil parse

`report.status`: **SUCCESS**, **WARNING**, atau **FAIL**, dengan temuan spesifik:
`PASAL_GAP` (nomor pasal melompat), `AYAT_GAP`, `HURUF_GAP`, `EMPTY_PASAL`,
`PENJELASAN_ORPHAN`, `NO_PASAL`, `NODE_WARNINGS`, `DOC_WARNING`.

## Struktur

| Lokasi | Isi |
|---|---|
| `main.go` | service: loop siklus, orkestrasi per sumber, ringkasan |
| `internal/downloader/` | kode sumber dari endpoint, tarik `/integrasi`, unduh PDF |
| `internal/extractor/` | orkestrasi OCR: gate probe, lewati halaman kosong, tangani keluaran berulang |
| `internal/raster/` | rasterisasi PDF→PNG via go-fitz (MuPDF) |
| `internal/parser/` | parser struktur, diagnosa, ekstraksi relasi antar-peraturan |
| `internal/config/` | pemuat `.env` & seluruh pengaturan |
| `internal/localllm/` | inferensi di dalam proses lewat yzma/llama.cpp |
| `internal/state/` | pelacak kegagalan & batas percobaan |
| `internal/fsutil/` | penulisan berkas atomik |
