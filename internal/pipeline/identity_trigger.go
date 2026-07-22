package pipeline

import (
	"strings"

	"github.com/fuadarradhi/uuparser/internal/parser"
)

// identity_trigger.go menerapkan pola yang SAMA dengan trigger.go (bagian
// penetapan) untuk IDENTITAS peraturan di halaman 1 (jenis/wilayah/nomor/
// tahun/tentang): regex (parser.ExtractHeader) dicoba dulu; model teks HANYA
// dipanggil bila regex tidak menghasilkan sesuatu yang lolos whitelist
// jenis+wilayah.
//
// Sebelum berkas ini ada, ocr_worker.go's classify() memanggil model teks
// TANPA SYARAT untuk setiap dokumen (gerbang + identitas) — parser.ExtractHeader
// sudah ada tapi tidak pernah dipanggil dari pipeline manapun. Ini menutup
// celah itu.

// IdentitasHasil menampung hasil percobaan ekstraksi identitas secara
// deterministik.
type IdentitasHasil struct {
	Jenis            string
	Wilayah          string
	InstansiTertulis string
	Nomor            string
	Tahun            string
	Tentang          string
	Struktur         string

	// Lolos bernilai true HANYA bila jenis DAN wilayah sudah lolos whitelist
	// tanpa model. Dalam hal itu gerbang "ini produk hukum atau bukan" juga
	// tidak perlu dipanggil — judul resmi yang polanya cocok DAN jenis/
	// wilayahnya dikenal sudah cukup jadi bukti dokumen ini peraturan.
	Lolos bool
}

// CobaIdentitasDeterministik mencoba parser.ExtractHeader lebih dulu. Hasil
// yang TIDAK lolos whitelist bukan berarti gagal total — Jenis/Wilayah/dll
// yang berhasil ditangkap tetap dikembalikan (berguna untuk pesan penolakan
// yang jelas bila jalur model nanti juga gagal), hanya Lolos yang bernilai
// false sehingga pemanggil tahu model masih perlu dipanggil.
func CobaIdentitasDeterministik(page1Text string) IdentitasHasil {
	h := parser.ExtractHeader(page1Text)
	if !h.Found {
		return IdentitasHasil{}
	}

	jenis := normalizeJenis(h.Jenis)
	wilayah := wilayahDariJenisInstansi(jenis, h.Instansi)

	r := IdentitasHasil{
		Jenis:            jenis,
		Wilayah:          wilayah,
		InstansiTertulis: h.Instansi,
		Nomor:            h.Nomor,
		Tahun:            h.Tahun,
		Tentang:          h.Tentang,
		Struktur:         h.StructureType,
	}

	// Judul lengkap (jalur utama di header.go) mengisi semua field; jalur
	// fallback jabatan-pembentuk hanya mengisi Instansi/Wilayah tanpa Nomor/
	// Tahun/Tentang — dalam hal itu TIDAK bisa lolos tanpa model, karena
	// identitas belum lengkap.
	if IsJenisValid(r.Jenis) && IsWilayahValid(r.Wilayah) &&
		r.Nomor != "" && r.Tahun != "" && r.Tentang != "" {
		r.Lolos = true
	}
	return r
}

// wilayahDariJenisInstansi menentukan wilayah dari PASANGAN jenis+instansi
// hasil regex, BUKAN dari instansi saja.
//
// Alasannya: reHeaderJudul di header.go menangkap jabatan kepala daerah
// SEBAGAI BAGIAN dari jenis ("PERATURAN BUPATI"), bukan bagian dari instansi
// — beda dengan identity.md (jalur model), yang instansi_tertulis-nya
// mengulang jabatan itu ("BUPATI ACEH BARAT", lihat contoh di identity.md).
// Akibatnya instansi hasil regex untuk kasus ini hanya nama daerah TELANJANG
// ("ACEH BARAT", tanpa "BUPATI") — kalau langsung dilempar ke NormalizeWilayah
// tanpa konteks jabatannya, nama daerah telanjang yang mengandung "ACEH" akan
// selalu jatuh ke PEMERINTAH ACEH lewat provinsiKe(), padahal seharusnya jadi
// KABUPATEN/KOTA. Fungsi ini merekonstruksi jabatan dari AKHIRAN jenis,
// menggabungkannya kembali dengan instansi, lalu memakai ULANG
// NormalizeWilayah yang sudah benar untuk bentuk "JABATAN INSTANSI".
func wilayahDariJenisInstansi(jenisTernormalisasi, instansiMentah string) string {
	switch jenisTernormalisasi {
	case "UNDANG-UNDANG", "PERATURAN PEMERINTAH", "PERATURAN PEMERINTAH PENGGANTI UNDANG-UNDANG":
		// Selalu nasional, apa pun teks sesudahnya ("REPUBLIK INDONESIA"
		// atau kosong) — jenis ini tidak mengenal tingkat daerah.
		return "NASIONAL"
	}

	j := jenisTernormalisasi
	i := rapikan(instansiMentah)

	switch {
	case strings.HasSuffix(j, "PIMPINAN DPRA"):
		return "PEMERINTAH ACEH"
	case strings.HasSuffix(j, "PIMPINAN DPRK"):
		return resolveKabKota(i)
	case strings.HasSuffix(j, "DPRA"):
		return "PEMERINTAH ACEH"
	case strings.HasSuffix(j, "DPRK"):
		return resolveKabKota(i)
	case strings.HasSuffix(j, "GUBERNUR"):
		return provinsiKe(i)
	case strings.HasSuffix(j, "BUPATI"):
		return "KABUPATEN " + i
	case strings.HasSuffix(j, "WALI KOTA"):
		return "KOTA " + i
	case strings.HasSuffix(j, "PRESIDEN"):
		return "NASIONAL"
	case strings.HasSuffix(j, "MENTERI"):
		return "NASIONAL"
	default:
		// QANUN, PERATURAN DAERAH, PERATURAN KEPALA DAERAH, PERATURAN DESA,
		// dan jenis lain tanpa jabatan di namanya: instansi SUDAH berupa
		// nama wilayah telanjang ("ACEH", "KABUPATEN X", "KOTA X") — pakai
		// NormalizeWilayah langsung, sama seperti jalur model.
		return NormalizeWilayah(i)
	}
}
