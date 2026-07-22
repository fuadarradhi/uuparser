Anda membaca bagian penutup sebuah produk hukum Indonesia — bagian yang memuat
tempat dan tanggal penetapan serta pejabat yang menandatangani.

Jawab HANYA dengan satu objek JSON. Tanpa penjelasan, tanpa tanda kutip
Markdown, tanpa teks lain.

{"ditetapkan_di": "", "ditetapkan_tanggal": "", "ditetapkan_oleh": "", "diundangkan_di": "", "diundangkan_tanggal": "", "diundangkan_oleh": ""}

Keterangan tiap isian:

- "ditetapkan_di": nama kota setelah kata "Ditetapkan di".
  Contoh: "Banda Aceh", "Meulaboh".
- "ditetapkan_tanggal": tanggal setelah "pada tanggal", APA ADANYA.
  Contoh: "1 Januari 2015", "15 Desember 2020".
- "ditetapkan_oleh": jabatan penanda tangan penetapan, tanpa nama orang.
  Contoh: "GUBERNUR ACEH", "BUPATI ACEH BARAT".
- Tiga isian "diundangkan_*" diisi dengan cara yang sama untuk bagian
  "Diundangkan di", bila ada. Bagian ini biasanya ditandatangani Sekretaris
  Daerah.

ATURAN:

- Salin apa adanya. Jangan mengubah bentuk tanggal, jangan menerjemahkan nama
  bulan, jangan melengkapi yang tidak tertulis.
- Bagian yang tidak ada di teks diisi string kosong "".
- JANGAN menyertakan nama orang, cukup jabatannya.
- JANGAN menyalin contoh di atas sebagai jawaban.

Contoh jawaban yang benar:

{"ditetapkan_di": "Banda Aceh", "ditetapkan_tanggal": "5 Maret 2026", "ditetapkan_oleh": "GUBERNUR ACEH", "diundangkan_di": "Banda Aceh", "diundangkan_tanggal": "6 Maret 2026", "diundangkan_oleh": "SEKRETARIS DAERAH ACEH"}
