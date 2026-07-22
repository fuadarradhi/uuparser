package pipeline

import "regexp"

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
	reDitetapkanBaku = regexp.MustCompile(
		`(?i)ditetapkan\s+di\s*:?\s*([A-Za-z][A-Za-z\s.']{1,40}?)\s*\n\s*pada\s+tanggal\s*:?\s*([0-9]{1,2}\s+\p{L}+\s+[0-9]{4})`)
	reDiundangkanBaku = regexp.MustCompile(
		`(?i)diundangkan\s+di\s*:?\s*([A-Za-z][A-Za-z\s.']{1,40}?)\s*\n\s*pada\s+tanggal\s*:?\s*([0-9]{1,2}\s+\p{L}+\s+[0-9]{4})`)

	// Jabatan penanda tangan: baris kapital yang diakhiri koma.
	reJabatanTtd = regexp.MustCompile(`(?m)^\s*((?:GUBERNUR|BUPATI|WALI\s*KOTA|SEKRETARIS\s+DAERAH|PJ\.?\s+GUBERNUR|PENJABAT\s+GUBERNUR)[A-Z\s.]*),\s*$`)
)

// PenetapanHasil menampung hasil penguraian bagian penetapan beserta
// keterangan apakah model masih diperlukan.
type PenetapanHasil struct {
	DitetapkanDi      string
	DitetapkanTanggal string
	DitetapkanOleh    string

	DiundangkanDi      string
	DiundangkanTanggal string
	DiundangkanOleh    string

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
		h.DitetapkanTanggal = rapikan(m[2])
	}
	if m := reDiundangkanBaku.FindStringSubmatch(teks); m != nil {
		h.DiundangkanDi = rapikanKota(m[1])
		h.DiundangkanTanggal = rapikan(m[2])
	}

	// Jabatan penanda tangan: yang pertama untuk penetapan, yang berikutnya
	// (bila ada) untuk pengundangan — urutan itu baku dalam dokumen.
	if jab := reJabatanTtd.FindAllStringSubmatch(teks, -1); len(jab) > 0 {
		h.DitetapkanOleh = rapikan(jab[0][1])
		if len(jab) > 1 {
			h.DiundangkanOleh = rapikan(jab[1][1])
		}
	}

	// Model dipanggil hanya bila bagian yang penandanya ada ternyata kosong.
	if adaTetap && (h.DitetapkanDi == "" || h.DitetapkanTanggal == "") {
		h.PerluModel = true
	}
	if adaUndang && (h.DiundangkanDi == "" || h.DiundangkanTanggal == "") {
		h.PerluModel = true
	}
	return h
}

// rapikanKota merapikan nama kota tanpa mengubah huruf besar-kecilnya menjadi
// kapital semua — nama tempat lazim ditulis dalam huruf campuran
// ("Banda Aceh"), dan mengubahnya akan menyimpang dari yang tertulis.
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
