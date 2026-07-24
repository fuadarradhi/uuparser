Anda menerima potongan hasil pengurai otomatis dokumen hukum Indonesia
(Undang-Undang/Peraturan/Qanun/Keputusan/Instruksi).

Pengurai TIDAK BISA mencocokkan sepotong teks ("TEKS YATIM") ke struktur
apa pun (bukan Pasal, bukan Ayat, bukan Diktum, bukan bagian resmi
lainnya), sehingga teks itu hanya ditempelkan sebagai catatan di sebelah
node tetangga terdekat ("TEKS NODE") — BUKAN menjadi bagian resmi node
itu maupun node manapun.

Jawab HANYA dengan satu objek JSON. Tanpa penjelasan, tanpa tanda kutip
Markdown, tanpa teks lain.

{"bermasalah": false, "penjelasan": ""}

Keterangan tiap isian:

- "bermasalah": true HANYA jika TEKS YATIM tampak seperti ISI SUNGGUHAN
  dokumen yang seharusnya punya bagian/node sendiri (mis. lanjutan
  kalimat yang terpotong, atau pasal/ayat/diktum yang terlewat
  pengurai). false bila TEKS YATIM tampak seperti artefak halaman yang
  aman diabaikan — pratinjau kata alih ke halaman berikutnya, header/
  footer/judul yang kebetulan tercetak berulang, atau serpihan tak
  bermakna.
- "penjelasan": 1-2 kalimat singkat menjelaskan penilaian Anda, dan bila
  bermasalah=true, ke bagian/node mana sebaiknya teks itu sebenarnya
  melekat. Kosongkan ("") bila bermasalah=false.

ATURAN:

- Jangan mengubah, memperbaiki, atau menulis ulang teksnya. Anda HANYA
  meninjau dan menjelaskan, bukan memperbaiki.
- Jangan menambahkan informasi yang tidak ada di teks.
- Jangan gunakan tanda elipsis (...) atau placeholder apa pun dalam jawaban.
- JANGAN menyalin contoh di bawah ini sebagai jawaban.

Contoh jawaban yang benar (TEKS YATIM adalah judul dokumen lain yang
kebetulan tercetak ulang sebagai header halaman):

{"bermasalah": false, "penjelasan": "Teks yatim adalah pengulangan judul dokumen sebagai header halaman, bukan isi baru."}
