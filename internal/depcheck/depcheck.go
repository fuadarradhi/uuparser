// Package depcheck memeriksa keberadaan biner EKSTERNAL opsional yang
// dipakai TEXT_CHECK dan CHEAP_TIER (lihat internal/textcheck,
// internal/pipeline/textsource.go, internal/pipeline/tier.go) — pdftotext,
// tesseract, dan paket bahasa tesseract-nya — lalu mencetak instruksi
// instalasi (AlmaLinux) ke konsol bila ada yang belum terpasang.
//
// SENGAJA tidak pernah mengembalikan error / menghentikan proses: kedua
// fitur itu sendiri sudah dirancang gagal-lembut (fallback ke GLM-OCR)
// bila alat ini tak ada — package ini murni supaya operator LANGSUNG tahu
// di awal lewat log startup, bukan menebak-nebak belakangan dari
// peringatan yang baru muncul saat dokumen pertama diproses.
package depcheck

import (
	"os/exec"
	"strings"

	"github.com/fuadarradhi/uuparser/internal/config"
	"github.com/fuadarradhi/uuparser/internal/logx"
)

// Run memeriksa dependensi opsional sesuai fitur yang AKTIF di cfg. Dipanggil
// SEKALI di awal main.go, sebelum koneksi database dibuka — supaya
// peringatannya jadi hal pertama yang terlihat operator, bukan tenggelam di
// tengah log setelah service berjalan.
func Run(cfg config.Config) {
	if !cfg.TextCheck && !cfg.CheapTier {
		return // kedua fitur mati — tidak ada alat eksternal yang relevan sama sekali
	}

	pdftotextOK := checkBinary("pdftotext",
		"TEXT_CHECK & CHEAP_TIER (pengambilan/pembanding lapisan teks PDF)",
		"sudo dnf install poppler-utils")

	if cfg.TextCheck && !cfg.CheapTier {
		_ = pdftotextOK
		return // CHEAP_TIER mati — tesseract tidak relevan
	}

	tesseractOK := checkBinary("tesseract",
		"CHEAP_TIER (tier PENJELASAN — dilewati untuk tier LAMPIRAN, yang tetap pakai GLM-OCR)",
		"sudo dnf install epel-release && sudo dnf install tesseract")

	if tesseractOK {
		checkTesseractLang(cfg.TesseractLang)
	}
}

// checkBinary melaporkan lewat logx.Banner (tampil di konsol, bukan cuma
// file log — lihat catatan di Run) bila biner `name` tidak ada di PATH,
// sekalian instruksi pasangnya. Bukan galat fatal.
func checkBinary(name, dipakaiUntuk, caraPasang string) bool {
	if _, err := exec.LookPath(name); err == nil {
		return true
	}
	logx.Banner("dependensi opsional TIDAK ditemukan: %s (dipakai untuk %s). "+
		"Fitur ini otomatis nonaktif/fallback ke GLM-OCR sampai terpasang — "+
		"cara pasang di AlmaLinux: %s", name, dipakaiUntuk, caraPasang)
	return false
}

// checkTesseractLang memeriksa apakah paket bahasa `lang` sudah terpasang
// (lewat `tesseract --list-langs`), cocok baris-per-baris (bukan substring
// mentah — supaya "ind" tidak salah cocok dengan kode bahasa lain yang
// kebetulan mengandung huruf yang sama).
func checkTesseractLang(lang string) {
	if lang == "" {
		lang = "ind"
	}
	out, err := exec.Command("tesseract", "--list-langs").CombinedOutput()
	if err != nil {
		logx.Banner("dependensi opsional: gagal memeriksa daftar bahasa tesseract (%v) — "+
			"lewati pemeriksaan paket bahasa TESSERACT_LANG=%s", err, lang)
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == lang {
			return
		}
	}
	logx.Banner("dependensi opsional TIDAK ditemukan: paket bahasa tesseract %q "+
		"(TESSERACT_LANG=%s) — tier PENJELASAN akan jatuh ke GLM-OCR sebagai jaring "+
		"pengaman (bukan kehilangan data, tapi tidak hemat) sampai terpasang. "+
		"Cara pasang di AlmaLinux (EPEL harus aktif — lihat peringatan tesseract di "+
		"atas bila belum): sudo dnf install tesseract-langpack-%s (kalau paket itu "+
		"tidak ketemu, jalankan `dnf search tesseract-langpack-%s` dulu — nama paket "+
		"mengikuti kode bahasa ISO 639, lihat daftar lengkap di "+
		"https://github.com/tesseract-ocr/tessdata)",
		lang, lang, lang, lang)
}
