Anda menerima SATU potongan teks hasil pengurai otomatis dokumen hukum
Indonesia (Undang-Undang/Peraturan/Qanun/Keputusan/Instruksi).

Pengurai otomatis MENCURIGAI teks ini mencampur ISI DARI DUA BAGIAN DOKUMEN
YANG BERBEDA menjadi satu — biasanya karena satu bagian (misalnya
"Memperhatikan", "Lampiran", "Menimbang", "Mengingat") gagal terdeteksi
batasnya, sehingga isinya malah tersedot ke bagian lain.

Jawab HANYA dengan satu objek JSON. Tanpa penjelasan, tanpa tanda kutip
Markdown, tanpa teks lain.

{"bermasalah": false, "penjelasan": ""}

Keterangan tiap isian:

- "bermasalah": true HANYA jika Anda benar-benar melihat dua topik/bagian
  berbeda tercampur dalam satu teks ini. false jika teksnya memang satu
  kesatuan yang wajar — mis. kata kunci itu cuma disebut sebagai rujukan
  biasa di tengah kalimat ("sebagaimana tercantum dalam Lampiran"), BUKAN
  pembuka bagian baru.
- "penjelasan": 1-2 kalimat singkat menjelaskan APA yang tercampur dan
  kira-kira DI MANA batas yang seharusnya — kutip beberapa kata dari
  teksnya sebagai penanda titik potong. Kosongkan ("") bila
  bermasalah=false.

ATURAN:

- Jangan mengubah, memperbaiki, atau menulis ulang teksnya. Anda HANYA
  meninjau dan menjelaskan, bukan memperbaiki.
- Jangan menambahkan informasi yang tidak ada di teks.
- Jangan gunakan tanda elipsis (...) atau placeholder apa pun dalam jawaban.
- JANGAN menyalin contoh di atas sebagai jawaban.

Contoh jawaban yang benar (teks yang dilihat mencampur "Memperhatikan"
dengan item Mengingat sebelumnya):

{"bermasalah": true, "penjelasan": "Teks ini mencampur item Mengingat terakhir dengan bagian Memperhatikan yang berbeda; batas yang tepat ada tepat sebelum kata 'Memperhatikan :' yang kedua."}
