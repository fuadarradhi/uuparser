Anda membaca halaman pertama sebuah produk hukum Indonesia. Salin identitasnya
APA ADANYA dari teks.

Jawab HANYA dengan satu objek JSON. Tanpa penjelasan, tanpa tanda kutip
Markdown, tanpa teks lain.

{"jenis": "", "instansi_tertulis": "", "nomor": "", "tahun": "", "tentang": ""}

Keterangan tiap isian:

- "jenis": nama jenis peraturannya saja, TANPA nama daerah atau pejabat.
  Contoh: "PERATURAN DAERAH", "QANUN", "PERATURAN GUBERNUR",
  "KEPUTUSAN GUBERNUR", "PERATURAN BUPATI", "KEPUTUSAN BUPATI",
  "PERATURAN WALI KOTA", "UNDANG-UNDANG", "PERATURAN PEMERINTAH".

- "instansi_tertulis": nama daerah ATAU jabatan PERSIS seperti tertulis di
  dokumen. Jangan diubah, jangan diterjemahkan, jangan dilengkapi.
  Contoh yang benar: "ACEH", "GUBERNUR ACEH", "KABUPATEN ACEH BARAT",
  "BUPATI ACEH BARAT", "KOTA BANDA ACEH", "PROPINSI DAERAH ISTIMEWA ACEH".

- "nomor": nomor peraturan PERSIS seperti tertulis, termasuk garis miring,
  titik, dan spasi. JANGAN disederhanakan menjadi angka saja.
  Contoh yang benar: "5", "12A", "300.2/ 69 /2026", "188.44/123/2020".

- "tahun": empat angka saja. Contoh: "2015", "1979".

- "tentang": judul lengkap setelah kata TENTANG, apa adanya.

ATURAN YANG TIDAK BOLEH DILANGGAR:

- Salin apa yang tertulis. Jangan menebak, jangan melengkapi, jangan
  memperbaiki. Bila suatu bagian tidak terbaca, isi dengan string kosong "".
- JANGAN menyalin contoh di atas sebagai jawaban.
- JANGAN menulis tanda titik-titik ("...") di dalam jawaban.
- Dokumen lama memakai ejaan lama (Propinsi, Atjeh, Dati II, Kotamadya).
  Pertahankan apa adanya, jangan dimodernkan.
- Jangan memakai pengetahuan Anda tentang peraturan Indonesia. Laporkan HANYA
  yang benar-benar tertulis pada teks yang diberikan.

Contoh jawaban yang benar untuk halaman berjudul
"KEPUTUSAN GUBERNUR ACEH NOMOR 300.2/ 69 /2026 TENTANG PENETAPAN PANITIA":

{"jenis": "KEPUTUSAN GUBERNUR", "instansi_tertulis": "GUBERNUR ACEH", "nomor": "300.2/ 69 /2026", "tahun": "2026", "tentang": "PENETAPAN PANITIA"}
