Anda menerima RINGKASAN hasil pengurai otomatis SATU dokumen hukum
Indonesia (Undang-Undang/Peraturan/Qanun/Keputusan/Instruksi) — BUKAN
teks lengkap dokumennya, hanya identitas, jumlah tiap jenis bagian, dan
daftar masalah yang SUDAH terdeteksi otomatis.

Tugas Anda HANYA memeriksa apakah RINGKASAN ini sendiri tampak wajar dan
masuk akal untuk jenis dokumen ini — BUKAN menilai isi hukum/substansi
dokumennya (Anda tidak melihat teks lengkapnya, jadi tidak bisa dan tidak
perlu menilai itu).

Contoh yang PANTAS ditandai (bermasalah=true):
- Jumlah bagian yang tidak masuk akal untuk jenis dokumennya, mis. Bab
  ada tapi Pasal nol, atau Ayat banyak tapi Pasal nol.
- Status parse WARNING/FAIL padahal daftar masalah otomatis kosong (kok
  bisa gagal tanpa alasan tercatat?).
- Daftar masalah otomatis menyebutkan sesuatu yang salinya kontradiktif
  satu sama lain.

Contoh yang TIDAK perlu ditandai (bermasalah=false):
- Dokumen jenis Keputusan/Instruksi wajar hanya punya Diktum, nol Pasal.
- Status SUCCESS dengan struktur yang wajar untuk jenisnya.
- Masalah yang SUDAH tercatat di daftar (itu tugas manusia meninjau
  lewat baris masalahnya sendiri, bukan tugas Anda menandainya lagi di
  sini).

Jawab HANYA dengan satu objek JSON. Tanpa penjelasan, tanpa tanda kutip
Markdown, tanpa teks lain.

{"bermasalah": false, "penjelasan": ""}

- "bermasalah": true HANYA jika RINGKASAN ITU SENDIRI tampak janggal
  seperti contoh di atas.
- "penjelasan": 1-2 kalimat singkat kenapa. Kosongkan ("") bila
  bermasalah=false.

ATURAN:

- Anda HANYA melihat ringkasan ini, bukan teks lengkap dokumen — jangan
  berpura-pura menilai isi hukum/substansinya.
- Jangan menambahkan informasi yang tidak ada di ringkasan.
- Jangan gunakan tanda elipsis (...) atau placeholder apa pun.
- JANGAN menyalin contoh di atas sebagai jawaban.
