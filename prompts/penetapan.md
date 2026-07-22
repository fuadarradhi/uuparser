Anda membaca bagian penutup sebuah produk hukum Indonesia — bagian yang memuat
tempat dan tanggal penetapan serta pejabat yang menandatangani.

Jawab HANYA dengan satu objek JSON. Tanpa penjelasan, tanpa tanda kutip
Markdown, tanpa teks lain.

{"ditetapkan_di": "", "ditetapkan_tanggal": "", "ditetapkan_oleh": "", "ditetapkan_oleh_nama": "", "diundangkan_di": "", "diundangkan_tanggal": "", "diundangkan_oleh": "", "diundangkan_oleh_nama": ""}

Keterangan tiap isian:

- "ditetapkan_di": nama kota setelah kata "Ditetapkan di".
  Contoh: "Banda Aceh", "Meulaboh".
- "ditetapkan_tanggal": tanggal setelah "pada tanggal", APA ADANYA.
  Contoh: "1 Januari 2015", "15 Desember 2020".
- "ditetapkan_oleh": JABATAN penanda tangan penetapan saja, TANPA nama orang.
  Contoh: "GUBERNUR ACEH", "BUPATI ACEH BARAT".
- "ditetapkan_oleh_nama": NAMA ORANG penanda tangan penetapan — biasanya
  tertulis beberapa baris di bawah jabatan, kadang didahului kata "Ttd.".
  Contoh: "MUZAKIR MANAF", "Drs. H. Ahmad, M.Si.".
- Empat isian "diundangkan_*" diisi dengan cara yang sama (jabatan dan nama
  terpisah) untuk bagian "Diundangkan di", bila ada. Bagian ini biasanya
  ditandatangani Sekretaris Daerah.

ATURAN:

- Salin apa adanya. Jangan mengubah bentuk tanggal, jangan menerjemahkan nama
  bulan, jangan mengubah huruf besar/kecil, jangan melengkapi yang tidak
  tertulis.
- Bagian yang tidak ada di teks diisi string kosong "".
- "ditetapkan_oleh"/"diundangkan_oleh" HANYA jabatan — jangan sisipkan nama
  orang di situ. Nama orang HANYA masuk ke "..._oleh_nama".
- JANGAN menyalin contoh di atas sebagai jawaban.

Contoh jawaban yang benar:

{"ditetapkan_di": "Banda Aceh", "ditetapkan_tanggal": "5 Maret 2026", "ditetapkan_oleh": "GUBERNUR ACEH", "ditetapkan_oleh_nama": "MUZAKIR MANAF", "diundangkan_di": "Banda Aceh", "diundangkan_tanggal": "6 Maret 2026", "diundangkan_oleh": "SEKRETARIS DAERAH ACEH", "diundangkan_oleh_nama": "BUSTAMI HAMZAH"}
