package pipeline

import (
	"regexp"
	"strings"
)

// trigger.go menentukan KAPAN model teks perlu dijalankan.
//
// Gagasannya: model teks itu mahal (hitungan detik sampai menit per
// panggilan) dan bisa keliru, jadi ia tidak dijalankan untuk semua hal —
// hanya untuk bagian yang penandanya JELAS ADA tetapi isinya TIDAK dapat
// diuraikan secara deterministik.
//
// Contoh yang ditangani di sini: halaman memuat "Ditetapkan di", yang berarti
// tempat, tanggal, dan pejabat penanda tangan pasti ada di sekitarnya. Bila
// pola bakunya cocok, semuanya diambil dengan regex tanpa melibatkan model
// sama sekali. Model baru dipanggil ketika penandanya ada tetapi bentuknya
// menyimpang — persis keadaan yang membuat aturan kaku gagal.

var (
	// rePenandaPenetapan: penanda bahwa bagian penetapan ada di teks ini.
	rePenandaPenetapan = regexp.MustCompile(`(?i)ditetapkan\s+di`)
	rePenandaUndang    = regexp.MustCompile(`(?i)diundangkan\s+di`)

	// Pola baku menurut Lampiran II UU 12/2011. Bila cocok, tidak perlu model.
	// Sesudah "pada tanggal" TOLERAN titik-dua ATAU koma ("pada tanggal,
	// 7 Januari 2026") — bug nyata ditemukan dari dokumen sungguhan
	// (2026-07-22): sebagian Keputusan Gubernur Aceh menulis koma di situ,
	// bukan titik dua, dan pola lama menolaknya sehingga jatuh ke model
	// padahal seharusnya bisa deterministik.
	reDitetapkanBaku = regexp.MustCompile(
		`(?i)ditetapkan\s+di\s*:?\s*([A-Za-z][A-Za-z\s.']{1,40}?)\s*\n\s*pada\s+tanggal\s*[:,]?\s*([0-9]{1,2}\s+\p{L}+\s+[0-9]{4})`)
	reDiundangkanBaku = regexp.MustCompile(
		`(?i)diundangkan\s+di\s*:?\s*([A-Za-z][A-Za-z\s.']{1,40}?)\s*\n\s*pada\s+tanggal\s*[:,]?\s*([0-9]{1,2}\s+\p{L}+\s+[0-9]{4})`)

	// Jabatan penanda tangan: baris kapital yang diakhiri koma.
	reJabatanTtd = regexp.MustCompile(`(?m)^\s*((?:GUBERNUR|BUPATI|WALI\s*KOTA|SEKRETARIS\s+DAERAH|PJ\.?\s+GUBERNUR|PENJABAT\s+GUBERNUR)[A-Z\s.]*),\s*$`)

	// reBarisTtd menandai baris "Ttd." (dengan/tanpa titik) yang lazim
	// disisipkan di antara baris jabatan dan nama penanda tangan.
	reBarisTtd = regexp.MustCompile(`(?im)^\s*ttd\.?\s*$`)

	// reBarisNama menangkap SATU baris yang bentuknya seperti nama orang:
	// huruf besar semua (boleh diselingi titik untuk singkatan nama tengah,
	// apostrof, spasi), TIDAK diakhiri koma — itu ciri baris jabatan
	// (reJabatanTtd), bukan nama.
	reBarisNama = regexp.MustCompile(`^[A-Z][A-Z.'\s]{2,60}[A-Z.]$`)

	// reBukanNama menyaring baris yang BENTUKNYA seperti reBarisNama tapi
	// sebenarnya bukan nama orang — kata baku yang sering muncul persis
	// setelah blok tanda tangan (mis. "SALINAN – dari Keputusan ini...").
	reBukanNama = regexp.MustCompile(`(?i)^(salinan|tembusan|distribusi)\b`)
)

// PenetapanHasil menampung hasil penguraian bagian penetapan beserta
// keterangan apakah model masih diperlukan.
type PenetapanHasil struct {
	DitetapkanDi      string
	DitetapkanTanggal string
	DitetapkanOleh    string
	// DitetapkanOlehNama adalah NAMA orang penanda tangan (bukan jabatan) —
	// permintaan user, 2026-07-22: "GUBERNUR ACEH" saja tidak cukup, perlu
	// juga "MUZAKIR MANAF"-nya. Diambil dari baris nama yang biasanya
	// muncul beberapa baris setelah jabatan (kadang didahului "Ttd.").
	DitetapkanOlehNama string

	DiundangkanDi       string
	DiundangkanTanggal  string
	DiundangkanOleh     string
	DiundangkanOlehNama string

	// PerluModel bernilai true bila penanda ditemukan tetapi isinya tidak
	// dapat diuraikan dengan pola baku.
	PerluModel bool
	// AdaPenanda bernilai false bila teks ini memang bukan bagian penutup;
	// dalam hal itu model TIDAK dipanggil sama sekali.
	AdaPenanda bool
}

// UraiPenetapan mencoba menguraikan bagian penetapan secara deterministik.
//
// Alur keputusannya:
//
//	tidak ada penanda        -> AdaPenanda=false, model tidak dipanggil
//	penanda ada & pola cocok -> hasil terisi, model tidak dipanggil
//	penanda ada & pola gagal -> PerluModel=true, model dipanggil untuk teks ini
func UraiPenetapan(teks string) PenetapanHasil {
	var h PenetapanHasil

	adaTetap := rePenandaPenetapan.MatchString(teks)
	adaUndang := rePenandaUndang.MatchString(teks)
	h.AdaPenanda = adaTetap || adaUndang
	if !h.AdaPenanda {
		return h
	}

	if m := reDitetapkanBaku.FindStringSubmatch(teks); m != nil {
		h.DitetapkanDi = rapikanKota(m[1])
		// rapikanKota, BUKAN rapikan(): rapikan() ada di normalize.go dan
		// menjadikan semuanya HURUF BESAR (untuk wilayah) — dipakai keliru
		// di sini sebelumnya, memutarbalikkan "7 Januari 2026" jadi
		// "7 JANUARI 2026". Tanggal harus APA ADANYA (sama seperti aturan
		// di penetapan.md), bukan dinormalisasi kapitalnya.
		h.DitetapkanTanggal = rapikanKota(m[2])
	}
	if m := reDiundangkanBaku.FindStringSubmatch(teks); m != nil {
		h.DiundangkanDi = rapikanKota(m[1])
		h.DiundangkanTanggal = rapikanKota(m[2])
	}

	// Jabatan penanda tangan: yang pertama untuk penetapan, yang berikutnya
	// (bila ada) untuk pengundangan — urutan itu baku dalam dokumen. Nama
	// penanda tangan dicari BEBERAPA BARIS setelah baris jabatan yang
	// bersangkutan (lihat cariNamaSetelah) — posisinya diambil dari indeks
	// pertandingan (FindAllStringSubmatchIndex), bukan cuma teksnya, karena
	// pencarian nama perlu tahu DI MANA jabatan itu berakhir di dalam teks.
	if jab := reJabatanTtd.FindAllStringSubmatchIndex(teks, -1); len(jab) > 0 {
		h.DitetapkanOleh = rapikan(teks[jab[0][2]:jab[0][3]])
		h.DitetapkanOlehNama = cariNamaSetelah(teks, jab[0][1])
		if len(jab) > 1 {
			h.DiundangkanOleh = rapikan(teks[jab[1][2]:jab[1][3]])
			h.DiundangkanOlehNama = cariNamaSetelah(teks, jab[1][1])
		}
	}

	// Model dipanggil hanya bila bagian yang penandanya ada ternyata kosong.
	// Nama penanda tangan IKUT disyaratkan (permintaan user, 2026-07-22):
	// jabatan saja tidak cukup, dan format baris nama lebih beragam
	// (kadang tanpa "Ttd." literal) daripada jabatan/tempat/tanggal,
	// sehingga lebih sering butuh model sebagai jalan belakang.
	if adaTetap && (h.DitetapkanDi == "" || h.DitetapkanTanggal == "" || h.DitetapkanOlehNama == "") {
		h.PerluModel = true
	}
	if adaUndang && (h.DiundangkanDi == "" || h.DiundangkanTanggal == "" || h.DiundangkanOlehNama == "") {
		h.PerluModel = true
	}
	return h
}

// cariNamaSetelah mencari nama penanda tangan pada beberapa baris SETELAH
// posisi akhir baris jabatan (dariIndex, dari indeks akhir pertandingan
// reJabatanTtd). Melompati baris kosong dan baris "Ttd." bila ada; berhenti
// pada baris pertama yang bukan kosong/Ttd./nama (format menyimpang — biar
// model yang tangani lewat PerluModel), dan tidak mencari lebih dari
// maxBarisNama baris supaya tidak ikut menelan bagian SALINAN/TEMBUSAN yang
// letaknya jauh di bawah blok tanda tangan.
const maxBarisNama = 6

func cariNamaSetelah(teks string, dariIndex int) string {
	if dariIndex < 0 || dariIndex > len(teks) {
		return ""
	}
	baris := strings.Split(teks[dariIndex:], "\n")
	for i, b := range baris {
		if i >= maxBarisNama {
			break
		}
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		if reBarisTtd.MatchString(b) {
			continue
		}
		if reBarisNama.MatchString(b) && !reBukanNama.MatchString(b) {
			return rapikan(b)
		}
		break
	}
	return ""
}

// rapikanKota merapikan teks tanpa mengubah huruf besar-kecilnya menjadi
// kapital semua — dipakai untuk nama kota MAUPUN tanggal, keduanya lazim
// ditulis dalam huruf campuran ("Banda Aceh", "7 Januari 2026") dan
// mengubahnya akan menyimpang dari yang tertulis. Namanya masih menyebut
// "Kota" karena itu pemakaian aslinya; dipakai juga untuk tanggal sejak bug
// rapikan()-menguppercase-tanggal ditemukan (2026-07-22).
func rapikanKota(s string) string {
	return reSpasi.ReplaceAllString(trimRuang(s), " ")
}

func trimRuang(s string) string {
	const ruang = " \t\r\n,.:"
	start, end := 0, len(s)
	for start < end && containsByte(ruang, s[start]) {
		start++
	}
	for end > start && containsByte(ruang, s[end-1]) {
		end--
	}
	return s[start:end]
}

func containsByte(set string, b byte) bool {
	for i := 0; i < len(set); i++ {
		if set[i] == b {
			return true
		}
	}
	return false
}
