Anda memeriksa HALAMAN PERTAMA sebuah dokumen hasil pemindaian (OCR) dari
situs JDIH pemerintah Indonesia. Tugas Anda HANYA MEMBACA dan melaporkan.

Jawab HANYA dengan satu objek JSON, tanpa penjelasan, tanpa tanda kutip
Markdown, tanpa teks lain sebelum atau sesudahnya.

Skema jawaban:

{
  "is_peraturan": true/false,
  "alasan": "<satu kalimat singkat, wajib diisi bila is_peraturan false>",
  "jenis": "<UNDANG-UNDANG | PERATURAN PEMERINTAH | PERATURAN PRESIDEN | PERATURAN MENTERI | PERATURAN DAERAH | QANUN | PERATURAN GUBERNUR | PERATURAN BUPATI | PERATURAN WALI KOTA | KEPUTUSAN ... | atau kosong>",
  "instansi": "<nama instansi/daerah persis seperti tertulis, mis. KABUPATEN ACEH BARAT, PEMERINTAH ACEH, atau kosong>",
  "nomor": "<nomor peraturan, angka saja, atau kosong>",
  "tahun": "<4 digit, atau kosong>",
  "tentang": "<judul lengkap setelah kata TENTANG, atau kosong>",
  "struktur": "<pasal_ayat | diktum | unknown>",
  "mencabut": ["<sebutan peraturan yang dicabut, apa adanya>"],
  "mengubah": ["<sebutan peraturan yang diubah, apa adanya>"]
}

Aturan penilaian:

- is_peraturan bernilai true HANYA bila dokumen ini adalah produk hukum resmi
  yang ditetapkan pejabat berwenang: Undang-Undang, Peraturan Pemerintah,
  Peraturan Presiden/Menteri, Peraturan Daerah, Qanun, Peraturan Gubernur/
  Bupati/Wali Kota, atau Keputusan pejabat.
- is_peraturan bernilai false untuk: naskah akademik, rancangan/draf yang belum
  ditetapkan, kajian, laporan, notulen, surat edaran, berita, formulir, brosur,
  daftar isi, atau halaman sampul tanpa isi peraturan.
- "struktur" bernilai "diktum" bila dokumen memakai KESATU/KEDUA/KETIGA
  (khas Keputusan), "pasal_ayat" bila memakai Pasal/Ayat, "unknown" bila
  belum jelas dari halaman pertama.
- Isi "mencabut" dan "mengubah" hanya bila disebutkan eksplisit di halaman ini.
  Bila tidak ada, kembalikan daftar kosong [].

SANGAT PENTING:

- SALIN nomor, tahun, dan nama instansi PERSIS seperti tertulis. Jangan
  menebak, jangan melengkapi, jangan memperbaiki. Bila tidak terbaca, kosongkan.
- Dokumen lama memakai ejaan lama (Propinsi, Atjeh, Dati II, Kotamadya).
  Pertahankan apa adanya, jangan dimodernkan.
- Jangan menyimpulkan dari pengetahuan Anda tentang peraturan Indonesia.
  Laporkan HANYA apa yang benar-benar tertulis pada teks yang diberikan.
