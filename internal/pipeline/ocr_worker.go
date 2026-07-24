package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fuadarradhi/uuparser/internal/extractor"
	"github.com/fuadarradhi/uuparser/internal/localllm"
	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/fuadarradhi/uuparser/internal/parser"
	"github.com/fuadarradhi/uuparser/internal/prompts"
	"github.com/fuadarradhi/uuparser/internal/store"
)

// ocr_worker.go menjalankan tahap kedua: OCR + perbaikan + klasifikasi.
//
// Urutannya SENGAJA berurutan per halaman (OCR hal-N -> perbaiki hal-N ->
// OCR hal-N+1 -> ...) sehingga kemajuan terpantau per halaman di UI dan
// dokumen yang gagal di halaman 1 langsung ditinggalkan tanpa membuang waktu
// meng-OCR sisanya.
//
// Kedua model (visi + teks) dimuat SEKALI (lewat warmup() di main.go) dan
// tetap menempel SEPANJANG UMUR SERVICE secara bawaan. Tidak ada
// bongkar-pasang model per halaman ataupun per dokumen.
//
// [Diperbaiki 2026-07-23, permintaan user] Sebelumnya KEDUA model dilepas
// setiap kali antrian OCR kosong, TANPA syarat LOW_MEMORY — jadi kalau
// dokumen tiba satu-satu (mis. sedang uji coba dengan -ids, atau downloader
// belum sempat mendaftarkan dokumen berikutnya), model dilepas lalu harus
// dimuat ulang dari nol untuk SETIAP dokumen, walau LOW_MEMORY=false
// (bawaan) — bertentangan dengan niat "dimuat sekali" yang justru tertulis
// di komentar ini sendiri. Sekarang pelepasan-saat-idle HANYA terjadi bila
// LOW_MEMORY=true — itulah satu-satunya alasan sah untuk mau melepas
// memori demi ruang, dan itu sudah eksplisit diminta lewat env itu sendiri.
// Bawaan (LOW_MEMORY=false): model tetap dimuat menunggu dokumen
// berikutnya, seberapa pun lama jedanya.

const (
	ocrIdleInterval = 30 * time.Second
	ocrAutoCrop     = true
	ocrMaxTokens    = 2048
)

func ocrWorker(ctx context.Context, deps Deps) {
	for {
		if ctx.Err() != nil {
			return
		}
		worked := drainOCR(ctx, deps)
		if !worked {
			// Lihat catatan di atas: hanya lepas kedua model saat memang
			// diminta lewat LOW_MEMORY=true. Bawaan: biarkan tetap dimuat.
			if deps.LowMemory {
				deps.Vision.Release()
				deps.Text.Release()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(ocrIdleInterval):
			}
		}
	}
}

func drainOCR(ctx context.Context, deps Deps) (workedAny bool) {
	for {
		if ctx.Err() != nil {
			return workedAny
		}
		job, err := deps.Store.ClaimForOCR(ctx, deps.PageCountRange.Min, deps.PageCountRange.Max, deps.PageCountOrder)
		if err == store.ErrNoWork {
			return workedAny
		}
		if err != nil {
			logx.Warn("ocr: klaim gagal: %v", err)
			return workedAny
		}
		workedAny = true
		processDocument(ctx, deps, job)
		// [Ditambahkan 2026-07-24, permintaan user] SEBELUMNYA tidak ada
		// panggilan ini di sini sama sekali — logx.Progress (baris \r yang
		// menimpa dirinya sendiri) terus menimpa lintas BATAS ANTAR DOKUMEN,
		// bukan cuma antar halaman dalam satu dokumen. Akibatnya seluruh
		// siklus hanya menyisakan SATU baris kemajuan yang terus berubah di
		// konsol — dokumen yang selesai tanpa peringatan apa pun tidak
		// meninggalkan jejak permanen sama sekali (baris permanen yang
		// sempat terlihat sebelumnya ternyata cuma kebocoran stderr MuPDF
		// yang memaksa newline, sudah diperbaiki terpisah di raster.Close).
		// FinishProgress mengubah baris \r yang sedang aktif jadi baris
		// permanen (newline sungguhan) sebelum dokumen berikutnya mulai
		// menimpa lagi — menjamin SETIAP dokumen selalu meninggalkan
		// setidaknya satu baris di riwayat terminal, sesuai permintaan user.
		logx.FinishProgress()
	}
}

// docSink menjalankan aturan per halaman: simpan OCR mentah, perbaiki dengan
// model teks, dan — khusus halaman 1 — klasifikasikan dokumen lalu putuskan
// apakah proses diteruskan.
type docSink struct {
	deps    Deps
	docID   int64
	prompts prompts.Set
	stopped string // alasan berhenti, kosong bila lanjut
	ocred   int    // halaman yang sudah selesai di-OCR
	total   int    // jumlah halaman dokumen (0 bila belum diketahui)

	// classified menandai bahwa pemeriksaan "ini peraturan atau bukan" sudah
	// dijalankan untuk dokumen ini, sehingga tidak diulang tiap halaman.
	classified bool

	// tier (2026-07-24): "" (batang tubuh, wajib GLM-OCR) | "penjelasan" |
	// "lampiran" — lihat OnPage di bawah dan internal/pipeline/tier.go.
	// Naik satu arah saja: begitu "lampiran", tidak pernah turun lagi
	// (LAMPIRAN selalu bagian PALING AKHIR dokumen).
	tier string
	// tierSwitch: true PERSIS pada panggilan OnPage yang MENAIKKAN tier
	// untuk PERTAMA kalinya (dipakai sebagai alasan `stop=true` yang BUKAN
	// penolakan dokumen — lihat processDocument/resolveTextSource, yang
	// harus membedakannya dari sink.stopped).
	tierSwitch bool

	// classifyTried (2026-07-24, koreksi user): jumlah halaman BERISI yang
	// sudah dicoba untuk klasifikasi legal-atau-bukan dan GAGAL (gerbang
	// model bilang bukan produk hukum, tanpa penanda konsiderans kuat).
	// Dipakai classify() sebagai jendela: selama masih ada halaman lain DAN
	// jendela (config.MinPage) belum habis, penolakan DITUNDA — coba
	// halaman berikutnya dulu — alih-alih langsung menolak dari 1 halaman
	// saja. Dokumen yang memang cuma sependek jendelanya (atau cuma 1
	// halaman) otomatis tidak menunggu halaman yang tidak ada.
	classifyTried int

	// debug menulis keluaran mode-debug (DEBUG_RESULT=true) — nil bila mode
	// itu tidak aktif. Aman dipanggil methodnya meski nil (lihat debug_writer.go).
	debug *debugWriter
}

// OnProgress mencetak baris kemajuan [diperbaiki/di-OCR/total]. Dipanggil
// juga SEBELUM halaman pertama di-OCR, sehingga konsol langsung menunjukkan
// [0/0/N] alih-alih diam sampai halaman pertama selesai.
// OnProgress mencetak baris kemajuan [halaman-sedang-dikerjakan/total].
// Dipanggil BERKALI-KALI selama satu halaman dikerjakan, termasuk SEBELUM
// halaman itu selesai — jadi angka pertama SENGAJA memakai `page` (halaman
// yang SEDANG diproses), bukan d.ocred (halaman yang sudah SELESAI).
//
// [Diperbaiki 2026-07-23, permintaan user] Sebelumnya baris ini memakai
// d.ocred untuk angka pertama, yang masih menyimpan hitungan SEBELUM
// halaman ini — sehingga baris kemajuan tampil "[0/5] hal 1" (bukan
// "[1/5] hal 1") selama seluruh proses halaman 1 berlangsung, baru
// menjadi "[1/5]" setelah hal 1 SELESAI. Sinkron dengan "hal N" di
// belakangnya sekarang: [1/5] tampil SEJAK halaman 1 mulai dikerjakan.
func (d *docSink) OnProgress(page, total int, detail string) {
	logx.Progress(page, total, "hal %d · %s", page, detail)
}

func (d *docSink) HasPage(ctx context.Context, page int) (bool, error) {
	return d.deps.Store.HasPage(ctx, d.docID, page)
}

func (d *docSink) OnPage(ctx context.Context, r extractor.PageResult) (bool, error) {
	// 1) Simpan hasil OCR MENTAH lebih dulu — apa pun yang terjadi setelah
	//    ini, teks asli sudah aman dan tidak akan pernah ditimpa.
	if err := d.deps.Store.SavePage(ctx, d.docID, r.Page, r.Text, r.IsEmpty, r.IsTruncated,
		r.InkRatio, r.CroppedPct, r.DPI, r.DurationMS, r.Notes); err != nil {
		return false, err
	}
	d.debug.tambahHalaman(r)

	// Baris kemajuan SETELAH OCR, SEBELUM perbaikan: angka pertama (sudah
	// diperbaiki) memang tertinggal satu dari angka kedua (sudah di-OCR).
	// DPI ikut ditampilkan (permintaan user, 2026-07-22) supaya gampang
	// dipantau langsung dari konsol, bukan cuma dari log/info.log.
	//
	// r.DPI > 0 HANYA benar utk halaman yang BENAR-BENAR dirender (GLM-OCR,
	// atau Tesseract di tier.go — keduanya butuh gambar). Halaman PDF2Text
	// (tier.go/textsource.go) TIDAK PERNAH dirender sama sekali — tanpa
	// percabangan ini baris di sini akan menimpa tag "PDF2Text · ..." yang
	// sudah benar dengan "DPI 0 · 0x0 px · 0s" yang menyesatkan (bug nyata,
	// dilaporkan user 2026-07-24: baris kedua tampilan konsol menimpa baris
	// pertama yang sudah benar).
	d.ocred = r.Page
	d.total = r.Total
	if r.DPI > 0 {
		logx.Progress(d.ocred, r.Total, "hal %d · DPI %d · %dx%d px%s · %s",
			r.Page, r.DPI, r.W, r.H, cropNote(r.CroppedPct), durText(r.DurationMS))
	} else {
		logx.Progress(d.ocred, r.Total, "hal %d · selesai", r.Page)
	}

	if r.IsEmpty {
		// Halaman kosong tidak bisa diklasifikasi. Klasifikasi TIDAK
		// dilewatkan begitu saja: ia menunggu halaman berisi pertama (lihat
		// blok di bawah). Tanpa itu, dokumen yang halaman pertamanya kosong
		// karena artefak pindaian akan diproses sampai selesai tanpa pernah
		// diperiksa.
		return false, nil
	}

	// Bagian penutup: bila penandanya ada, uraikan. Model teks hanya
	// dipanggil untuk bagian yang penandanya ada tetapi tak dapat diuraikan
	// pola baku — lihat trigger.go.
	if err := d.uraiPenetapan(ctx, r); err != nil {
		return false, err
	}

	// 3) Halaman BERISI PERTAMA menentukan nasib dokumen. Biasanya halaman 1;
	//    bila halaman 1 kosong, pemeriksaan bergeser ke halaman berisi
	//    berikutnya alih-alih hilang sama sekali.
	if !d.classified {
		if done, derr := d.deps.Store.IsClassified(ctx, d.docID); derr == nil && done {
			d.classified = true
			return false, nil
		}
		return d.classify(ctx, r.Text, r.Page)
	}

	// Deteksi tier hemat (2026-07-24, permintaan user): begitu SATU halaman
	// memuat awal blok LAMPIRAN atau PENJELASAN, halaman SELANJUTNYA
	// diproses via jalur lebih murah (pdftotext -> Tesseract/GLM-OCR sesuai
	// tier — lihat tier.go), bukan GLM-OCR penuh seperti batang tubuh.
	// Berhenti DI SINI (stop=true) HANYA supaya Document() menyerahkan
	// kendali ke processDocument/resolveTextSource — d.tierSwitch (bukan
	// d.stopped) memberi tahu pemanggil ini BUKAN penolakan dokumen.
	// Halaman r.Page ITU SENDIRI sudah tersimpan lewat GLM-OCR biasa di
	// atas — perubahan tier baru berlaku dari halaman SETELAHNYA.
	//
	// CATATAN keterbatasan yang disengaja: bila anchor ini kebetulan muncul
	// pada probe resolveTextSource (halaman 1-2), stop=true di sana
	// diperlakukan sebagai "lanjutkan probe seperti biasa" (lihat
	// resolveTextSource) — d.tier tetap tersimpan untuk dipakai nanti, TAPI
	// jalur OCR utama (ex.Document tanpa MaxPage) tidak mengecek d.tier di
	// awal, hanya bereaksi saat anchor MUNCUL LAGI. Kasus ini secara
	// praktis mustahil (Penjelasan/Lampiran tidak pernah ada di halaman
	// 1-2 dokumen legal sungguhan), jadi sengaja tidak ditangani lebih jauh.
	if d.deps.CheapTier && d.tier != "lampiran" {
		switch {
		case parser.HasLampiranAnchor(r.Text):
			d.tier = "lampiran"
			d.tierSwitch = true
			return true, nil
		case d.tier == "" && parser.HasPenjelasanAnchor(r.Text):
			d.tier = "penjelasan"
			d.tierSwitch = true
			return true, nil
		}
	}
	return false, nil
}

// classify membaca halaman 1 dan memutuskan: tolak (bukan peraturan / jenis
// atau wilayah tak dikenal), tandai duplikat, atau lanjutkan ke halaman
// berikutnya.
//
// Tahap 0 — regex dulu (parser.ExtractHeader lewat CobaIdentitasDeterministik,
// lihat identity_trigger.go). Bila jenis DAN wilayah sudah lolos whitelist,
// model TIDAK DIPANGGIL SAMA SEKALI — baik gerbang maupun identitas — karena
// judul resmi yang polanya cocok dan jenis/wilayahnya dikenal sudah cukup
// jadi bukti dokumen ini peraturan. Model hanya jadi jalan belakang untuk
// dokumen yang formatnya menyimpang.
func (d *docSink) classify(ctx context.Context, halaman string, page int) (bool, error) {
	if det := CobaIdentitasDeterministik(halaman); det.Lolos {
		meta := store.DocMeta{
			IsPeraturan:      true,
			Jenis:            det.Jenis,
			Wilayah:          det.Wilayah,
			InstansiTertulis: det.InstansiTertulis,
			Nomor:            det.Nomor,
			Tahun:            det.Tahun,
			Tentang:          det.Tentang,
			Struktur:         det.Struktur,
		}
		meta.NomorUrut = NomorUrut(meta.Nomor)
		return d.finishClassify(ctx, meta, "regex (deterministik, tanpa model)")
	}

	// Tahap 1 — gerbang. Pertanyaan tunggal yang sempit: produk hukum atau
	// bukan. Dipisah dari pembacaan identitas karena model kecil lebih andal
	// menjawab satu hal daripada banyak hal sekaligus.
	if d.deps.LowMemory {
		d.deps.Vision.Release()
	}
	produkHukum, alasan, rawGate, err := AskIsRegulation(ctx, d.deps.Text, d.prompts.Gate, halaman,
		d.textProgress(d.ocred, d.total, "memeriksa jenis dokumen"))
	d.debug.catatModel("GERBANG (gate.md)", d.prompts.Gate, halaman, rawGate, err)
	if err != nil {
		if d.deps.LowMemory {
			d.deps.Text.Release()
		}
		return true, fmt.Errorf("pemeriksaan produk hukum: %w", err)
	}

	if !produkHukum {
		if d.deps.LowMemory {
			d.deps.Text.Release()
		}
		// [Ditambahkan 2026-07-24, permintaan user] Model kecil bisa keliru
		// menilai dokumen yang formatnya menyimpang (mis. Surat Edaran tanpa
		// "MEMUTUSKAN" formal) sebagai "bukan produk hukum". Bila halaman
		// SUDAH memuat salah satu penanda konsiderans kuat (Menimbang/
		// Mengingat/Memutuskan/Menetapkan), sinyal deterministik itu MENANG
		// atas kata model — konsisten dengan prinsip "model membaca, kode
		// menyimpulkan" — dan dokumen dilanjutkan ke tahap identitas alih-
		// alih ditolak. Permintaan eksplisit user: jangan ada dokumen
		// berformat begitu yang tertolak.
		if parser.HasKonsideransAnchor(halaman) {
			logx.Info("gerbang model menilai bukan produk hukum (%s), tapi halaman memuat penanda konsiderans (Menimbang/Mengingat/Memutuskan/Menetapkan) — diterima, lanjut ke identitas", alasan)
		} else {
			// CLASSIFY_PAGE_WINDOW (field Go: deps.MinPage; env diganti
			// nama 2026-07-24 dari MIN_PAGE): BUKAN syarat jumlah
			// halaman minimum dokumen (itu salah, sudah dihapus) — ini
			// jendela BERAPA HALAMAN yang boleh dicoba sebelum benar-benar
			// menyerah menolak dokumen sebagai "bukan peraturan". Halaman
			// pertama yang gagal (mis. cuma sampul/judul tanpa konsiderans)
			// BUKAN berarti dokumennya bukan peraturan — Menimbang/
			// Mengingat bisa saja ada di halaman 2. Selama masih ada
			// halaman lain DAN jendela belum habis, coba dulu halaman
			// berikutnya. Dokumen yang memang cuma sependek jendelanya
			// (termasuk yang cuma 1 halaman) otomatis TIDAK menunggu
			// halaman yang tidak ada — window <= d.total selalu, jadi
			// r.Page < d.total langsung false dan keputusan diambil dari
			// apa yang tersedia, persis permintaan user.
			d.classifyTried++
			window := d.deps.MinPage
			if window <= 0 {
				window = 1 // CLASSIFY_PAGE_WINDOW=0/nonaktif: perilaku minimal, putuskan dari halaman pertama yang dicoba, tanpa menunggu halaman lain
			}
			if d.classifyTried < window && page < d.total {
				logx.Info("gerbang model menilai bukan produk hukum di hal %d (%s) — masih dalam jendela CLASSIFY_PAGE_WINDOW=%d, coba halaman berikutnya sebelum menyerah",
					page, alasan, window)
				return false, nil
			}

			if alasan == "" {
				alasan = "model menilai dokumen ini bukan produk hukum"
			}
			d.debug.catatIdentitas("DITOLAK — bukan produk hukum: "+alasan, store.DocMeta{})
			if err := d.deps.Store.RejectNotRegulation(ctx, d.docID, alasan); err != nil {
				return true, err
			}
			d.stopped = "bukan peraturan: " + alasan
			return true, nil
		}
	}

	// Tahap 2 — identitas. Model hanya menyalin apa yang tertulis; pemetaan
	// wilayah dan penurunan angka urut dikerjakan kode (lihat normalize.go).
	meta, rawIdentity, err := AskIdentity(ctx, d.deps.Text, d.prompts.Identity, halaman,
		d.textProgress(d.ocred, d.total, "membaca identitas"))
	d.debug.catatModel("IDENTITAS (identity.md)", d.prompts.Identity, halaman, rawIdentity, err)
	if d.deps.LowMemory {
		d.deps.Text.Release()
	}
	if err != nil {
		return true, fmt.Errorf("membaca identitas: %w", err)
	}
	meta.IsPeraturan = true

	// Jenis/wilayah hasil MODEL juga wajib lolos whitelist yang sama —
	// model boleh salah menyalin, basis data tidak boleh menyimpan jenis/
	// wilayah yang bentuknya tidak terjamin konsisten.
	if !IsJenisValid(meta.Jenis) || !IsWilayahValid(meta.Wilayah) {
		if d.deps.LowMemory {
			d.deps.Text.Release()
		}
		// [Ditambahkan 2026-07-24, permintaan user] Sama seperti gerbang di
		// atas: whitelist jenis/wilayah ada supaya kolom itu tetap konsisten
		// untuk query, tapi TIDAK boleh sampai membuang dokumen yang jelas-
		// jelas berformat produk hukum hanya karena jabatan penandatangannya
		// tidak ada di daftar (mis. "SURAT EDARAN SEKRETARIS DAERAH ..." —
		// whitelist hanya mendaftar MENTERI/GUBERNUR/BUPATI/WALI KOTA).
		// Jenis/wilayah TETAP disimpan apa adanya (tidak dipaksa cocok
		// whitelist) — hanya gerbang tolak-nya yang dilewati; pembersihan
		// taksonomi jenis/wilayah yang lebih luas tetap tugas lain, bukan
		// alasan membuang datanya sekarang.
		if parser.HasKonsideransAnchor(halaman) {
			logx.Info("jenis/wilayah di luar whitelist (jenis=%q wilayah=%q) tapi halaman memuat penanda konsiderans kuat — diterima apa adanya, perlu tinjauan taksonomi jenis/wilayah nanti",
				meta.Jenis, meta.Wilayah)
			return d.finishClassify(ctx, meta, "model teks (jenis/wilayah di luar whitelist, diterima karena penanda konsiderans kuat)")
		}
		alasanTolak := fmt.Sprintf("jenis atau wilayah tidak dikenal: jenis=%q wilayah=%q",
			meta.Jenis, meta.Wilayah)
		d.debug.catatIdentitas("DITOLAK — "+alasanTolak, meta)
		if err := d.deps.Store.RejectNotRegulation(ctx, d.docID, alasanTolak); err != nil {
			return true, err
		}
		d.stopped = alasanTolak
		return true, nil
	}

	return d.finishClassify(ctx, meta, "model teks (gate.md + identity.md)")
}

// finishClassify menyimpan metadata & memeriksa duplikat — dipakai baik oleh
// jalur deterministik maupun jalur model, sehingga logika duplikat-checking
// tidak dobel. sumber menjelaskan MANA yang memutuskan (dicatat ke mode
// debug — lihat DEBUG_RESULT — supaya kalau jenis/wilayah keliru, jelas dulu
// apakah salahnya di regex atau di model).
func (d *docSink) finishClassify(ctx context.Context, meta store.DocMeta, sumber string) (bool, error) {
	d.debug.catatIdentitas(sumber, meta)
	dup, err := d.deps.Store.ApplyMetaAndCheckDuplicate(ctx, d.docID, meta, canonicalKey(meta))
	if err != nil {
		return true, err
	}
	if dup {
		d.stopped = "duplikat peraturan yang sudah ada"
		return true, nil
	}

	d.classified = true
	logx.Info("dokumen dikenali: %s %s Nomor %s Tahun %s — %s",
		meta.Jenis, meta.Wilayah, meta.Nomor, meta.Tahun, meta.Tentang)
	if meta.InstansiTertulis != meta.Wilayah {
		logx.Info("wilayah dipetakan: %q -> %q", meta.InstansiTertulis, meta.Wilayah)
	}
	return false, nil
}

// uraiPenetapan menangani bagian penutup dokumen.
//
// Model teks hanya dipanggil bila penandanya ADA tetapi isinya TIDAK dapat
// diuraikan pola baku — pemakaian yang sangat jarang, sehingga biayanya kecil
// sementara cakupannya tetap luas untuk dokumen yang formatnya menyimpang.
func (d *docSink) uraiPenetapan(ctx context.Context, r extractor.PageResult) error {
	h := UraiPenetapan(r.Text)
	if !h.AdaPenanda {
		return nil
	}

	p := store.Penetapan{
		DitetapkanDi: h.DitetapkanDi, DitetapkanTanggal: h.DitetapkanTanggal,
		DitetapkanOleh: h.DitetapkanOleh, DitetapkanOlehNama: h.DitetapkanOlehNama,
		DiundangkanDi: h.DiundangkanDi, DiundangkanTanggal: h.DiundangkanTanggal,
		DiundangkanOleh: h.DiundangkanOleh, DiundangkanOlehNama: h.DiundangkanOlehNama,
	}

	if h.PerluModel {
		logx.Info("hal %d: bagian penetapan tidak sesuai pola baku — model teks dipanggil", r.Page)
		if d.deps.LowMemory {
			d.deps.Vision.Release()
		}
		mp, rawPenetapan, err := AskPenetapan(ctx, d.deps.Text, d.prompts.Penetapan, r.Text,
			d.textProgress(r.Page, r.Total, "membaca bagian penetapan"))
		d.debug.catatModel("PENETAPAN (penetapan.md)", d.prompts.Penetapan, r.Text, rawPenetapan, err)
		if d.deps.LowMemory {
			d.deps.Text.Release()
		}
		if err != nil {
			return fmt.Errorf("membaca bagian penetapan halaman %d: %w", r.Page, err)
		}
		// Hasil deterministik didahulukan; model hanya mengisi yang kosong.
		p = gabungPenetapan(p, mp)
	}

	return d.deps.Store.SavePenetapan(ctx, d.docID, p)
}

// gabungPenetapan memakai nilai dari penguraian pola baku bila ada, dan nilai
// dari model hanya untuk bagian yang masih kosong. Urutan ini disengaja:
// aturan yang pasti selalu lebih dipercaya daripada pembacaan model.
func gabungPenetapan(pasti, model store.Penetapan) store.Penetapan {
	pilih := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	return store.Penetapan{
		DitetapkanDi:        pilih(pasti.DitetapkanDi, model.DitetapkanDi),
		DitetapkanTanggal:   pilih(pasti.DitetapkanTanggal, model.DitetapkanTanggal),
		DitetapkanOleh:      pilih(pasti.DitetapkanOleh, model.DitetapkanOleh),
		DitetapkanOlehNama:  pilih(pasti.DitetapkanOlehNama, model.DitetapkanOlehNama),
		DiundangkanDi:       pilih(pasti.DiundangkanDi, model.DiundangkanDi),
		DiundangkanTanggal:  pilih(pasti.DiundangkanTanggal, model.DiundangkanTanggal),
		DiundangkanOleh:     pilih(pasti.DiundangkanOleh, model.DiundangkanOleh),
		DiundangkanOlehNama: pilih(pasti.DiundangkanOlehNama, model.DiundangkanOlehNama),
	}
}

// documentPageCount mengembalikan jumlah halaman ASLI dokumen, diutamakan
// dari documents.total_pages yang sudah dicatat SEKALI saat unduh (lihat
// downloader_worker.go/MarkDownloaded) — jauh lebih murah daripada membuka
// PDF-nya lagi. Jatuh ke extractor.PageCount HANYA bila belum tercatat
// (dokumen dari sebelum fitur ini ada, atau penghitungan saat unduh gagal).
func documentPageCount(ctx context.Context, deps Deps, job store.OCRJob) (int, error) {
	if n, ok, err := deps.Store.GetTotalPages(ctx, job.ID); err == nil && ok {
		return n, nil
	}
	return extractor.PageCount(job.PDFPath)
}

func processDocument(ctx context.Context, deps Deps, job store.OCRJob) {
	sink := &docSink{deps: deps, docID: job.ID, prompts: deps.Prompts}
	if deps.DebugResult {
		sink.debug = newDebugWriter(deps.DebugDir, job.ID, job.PDFPath)
		// Isi ulang dengan halaman yang SUDAH tersimpan dari penjalanan
		// sebelumnya SEBELUM halaman baru diproses — tanpa ini, ocr.txt
		// cuma menunjukkan sisa halaman yang diproses run ini, seolah
		// dokumennya cuma sepanjang itu (lihat catatan di
		// store.ListSavedPages).
		if saved, serr := deps.Store.ListSavedPages(ctx, job.ID); serr != nil {
			logx.Warn("debug: baca halaman tersimpan: %v", serr)
		} else {
			for _, p := range saved {
				sink.debug.tambahHalaman(extractor.PageResult{
					Page: p.Page, Text: p.Text, IsEmpty: p.IsEmpty, IsTruncated: p.IsTruncated,
					InkRatio: p.InkRatio, CroppedPct: p.CroppedPct, DPI: p.DPI,
					DurationMS: p.DurationMS, Notes: p.Notes,
				})
			}
		}
	}
	defer sink.debug.tutup()

	// Jumlah halaman: dibaca dari documents.total_pages (dicatat SEKALI saat
	// unduh — lihat downloader_worker.go/MarkDownloaded), bukan dihitung
	// ulang dengan membuka PDF lagi di sini. Jatuh ke extractor.PageCount
	// HANYA sebagai jaga-jaga (dokumen dari sebelum fitur ini ada, atau
	// penghitungan saat unduh gagal). Angka ASLI ini diteruskan ke
	// resolveTextSource — TIDAK ADA lagi pemotongan per-halaman di sini:
	// dokumen yang lolos ClaimForOCR (sudah disaring di sana berdasar
	// PAGE_COUNT_RANGE) selalu diproses UTUH sampai halaman terakhirnya.
	nAsli, perr := documentPageCount(ctx, deps, job)
	if perr == nil {
		sink.total = nAsli
	}

	// MinPage DIHAPUS (2026-07-24, koreksi user): sebelumnya dokumen yang
	// secara ASLI kurang dari MIN_PAGE halaman ditolak langsung sebagai
	// "bukan peraturan" TANPA di-OCR sama sekali. User mengoreksi: itu
	// SALAH — dokumen yang memang cuma 1 halaman (mis. Keputusan pendek)
	// tetap harus diproses APA ADANYA, bukan ditolak cuma karena pendek.
	// Klasifikasi legal-atau-bukan tetap jalan seperti biasa lewat classify
	// di OnPage (berdasarkan ISI halaman, bukan JUMLAH halaman) — itu sudah
	// cukup untuk menyaring sampul/pengumuman yang sungguh bukan naskah
	// peraturan, tanpa salah tolak dokumen pendek yang sah.
	//
	// Dokumen yang SEBELUMNYA terlanjur ditolak oleh pengecekan lama ini
	// bisa dikembalikan lewat SQL (reject_reason='bukan_peraturan' DAN
	// last_error mengandung 'MIN_PAGE') — lihat catatan di .env.example.

	// Rapikan sisa pekerjaan dari penjalanan sebelumnya SEBELUM meng-OCR
	// halaman baru: memulihkan hitungan kemajuan, menyelesaikan perbaikan
	// yang tertunda, dan — yang paling penting — memastikan dokumen sudah
	// diklasifikasi.
	stop, err := sink.resume(ctx)
	if err != nil {
		finishInterrupted(deps, job.ID, "melanjutkan dokumen", err)
		return
	}
	if stop {
		logx.Skip("dokumen dihentikan — %s", sink.stopped)
		return
	}

	// textcheck (2026-07-24): coba dulu halaman 1 (lalu 2) — bila lapisan
	// teks PDF (`pdftotext`) terbukti sama-akurat dengan OCR, sisa dokumen
	// dituntaskan lewat pdftotext (jauh lebih murah, TANPA batas PageCountRange)
	// dan fungsi ini SUDAH SELESAI menangani dokumennya. Lihat textsource.go.
	handled, useOCR, terr := resolveTextSource(ctx, deps, sink, job, sink.total)
	if terr != nil {
		finishInterrupted(deps, job.ID, "menentukan sumber teks", terr)
		return
	}
	if handled {
		if sink.stopped != "" {
			logx.Skip("dokumen dihentikan — %s", sink.stopped)
		} else {
			logx.OK("ocr selesai (pdftotext) · %d halaman", nAsli)
		}
		return
	}
	_ = useOCR // selalu true di titik ini — dijaga tetap eksplisit agar niatnya jelas dibaca.

	// dpiPaksa: bila halaman 1 SUDAH diproses (dokumen ini dilanjutkan
	// setelah terhenti di tengah jalan), pakai DPI yang sama untuk sisa
	// halaman — jangan menghitung ulang skor ketajaman seolah halaman
	// pertama yang diproses sekarang adalah "halaman pertama" dokumen.
	dpiPaksa, err := deps.Store.DPIPage1(ctx, job.ID)
	if err != nil {
		logx.Warn("ocr: baca DPI halaman 1: %v", err)
	}

	ex := extractor.New(extractor.Config{
		DPIJelas: deps.DPIJelas, DPISedang: deps.DPISedang, DPIBlur: deps.DPIBlur,
		AmbangJelas: deps.AmbangJelas, AmbangSedang: deps.AmbangSedang,
		DPIPaksa:  dpiPaksa,
		AutoCrop:  ocrAutoCrop,
		OCRClient: deps.Vision, OCRMaxTokens: ocrMaxTokens,
		// MaxPage sengaja TIDAK diisi (nol = tanpa batas): pemotongan
		// per-dokumen sudah digantikan skema baru — dokumen yang
		// KESELURUHAN halamannya di luar PAGE_COUNT_RANGE tidak pernah
		// sampai ke titik ini sama sekali (disaring di ClaimForOCR).
		// Begitu sebuah dokumen diambil, ia SELALU diproses utuh sampai
		// halaman terakhir.
	}, sink)

	total, stopped, err := ex.Document(ctx, job.PDFPath)
	switch {
	case err != nil:
		finishInterrupted(deps, job.ID, "proses dokumen", err)
	case stopped && sink.tierSwitch:
		// BUKAN dokumen ditolak — cuma masuk tier hemat (PENJELASAN/
		// LAMPIRAN, lihat OnPage). Halaman terakhir (sink.ocred) sudah
		// tersimpan lewat GLM-OCR biasa; lanjutkan SISA halaman lewat
		// tier.go, bukan GLM-OCR penuh.
		lastPage := sink.ocred
		if terr := runTieredMode(ctx, deps, sink, job, nAsli); terr != nil {
			finishInterrupted(deps, job.ID, "tier hemat (penjelasan/lampiran)", terr)
			return
		}
		logx.OK("ocr+perbaikan selesai (tier hemat sejak hal %d) · %d halaman", lastPage+1, nAsli)
	case stopped:
		// Status sudah disetel oleh classify (rejected/duplicate).
		logx.Skip("dokumen dihentikan — %s", sink.stopped)
	default:
		if err := deps.Store.MarkOCRDone(ctx, job.ID, total); err != nil {
			logx.Warn("ocr: tandai selesai: %v", err)
			return
		}
		logx.OK("ocr+perbaikan selesai · %d halaman", total)
	}
}

// finishInterrupted memutuskan apakah sebuah galat merupakan kegagalan DATA
// (dokumen memang bermasalah — hitung percobaan) atau gangguan SEMENTARA
// (pembatalan, model gagal dimuat, database sesaat tak terjangkau).
//
// Pembedaan ini penting: menekan Ctrl+C beberapa kali seharusnya tidak boleh
// membuat dokumen yang sehat berakhir berstatus 'failed' dan tak pernah
// diproses lagi.
func finishInterrupted(deps Deps, docID int64, what string, err error) {
	if isTransient(err) {
		// Kembalikan ke antrian apa adanya; dilanjutkan pada penjalanan
		// berikutnya, dari halaman tempatnya berhenti.
		if rerr := deps.Store.RequeueDocument(docID, "downloaded"); rerr != nil {
			logx.Warn("mengembalikan dokumen ke antrian: %v", rerr)
		}
		logx.Skip("%s dihentikan (%v) — dokumen dikembalikan ke antrian, akan dilanjutkan", what, err)
		return
	}
	logx.Fail(fmt.Sprintf("dokumen %d", docID), "%s gagal: %v", what, err)
	_ = deps.Store.MarkOCRFailed(context.Background(), docID, err.Error(), maxAttempts)
}

// isTransient membedakan gangguan sementara dari kerusakan data.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return localllm.IsLoadError(err)
}

// resume merapikan sisa pekerjaan dokumen yang sebelumnya terhenti. Urutannya
// disengaja:
//
//  1. Pulihkan hitungan kemajuan dari basis data, supaya baris kemajuan
//     meneruskan angka sebelumnya alih-alih mulai dari [0/0/N] padahal
//     sebagian halaman sudah selesai.
//  2. Selesaikan perbaikan yang tertunda (halaman punya teks OCR tetapi
//     belum diperbaiki).
//  3. Pastikan dokumen SUDAH DIKLASIFIKASI. Ini yang paling penting:
//     pemeriksaan "ini peraturan atau bukan" hanya berjalan saat halaman 1
//     diproses. Bila halaman 1 sudah ada dari penjalanan sebelumnya, tahap
//     OCR melewatinya — sehingga tanpa langkah ini dokumen diteruskan sampai
//     selesai TANPA PERNAH diperiksa, dan dokumen bukan-peraturan pun lolos
//     ke tahap parse.
//
// Mengembalikan stop=true bila klasifikasi memutuskan dokumen ini tidak
// dilanjutkan (bukan peraturan, atau duplikat).
func (d *docSink) resume(ctx context.Context) (stop bool, err error) {
	if ocred, _, cerr := d.deps.Store.CountPagesDone(ctx, d.docID); cerr == nil {
		d.ocred = ocred
	}

	done, err := d.deps.Store.IsClassified(ctx, d.docID)
	if err != nil {
		return false, err
	}
	if done {
		d.classified = true
		return false, nil
	}

	// Klasifikasi dari halaman BERISI pertama yang sudah tersimpan — tanpa
	// OCR ulang. Bila belum ada satu pun halaman berisi, klasifikasi berjalan
	// seperti biasa saat halaman pertama di-OCR nanti.
	page, text, ok, err := d.deps.Store.FirstNonEmptyPage(ctx, d.docID)
	if err != nil || !ok {
		return false, err
	}
	return d.classify(ctx, text, page)
}

// textProgress membuat pelapor kemajuan untuk pekerjaan model teks, sehingga
// konsol tetap bergerak selama tahap yang paling lama diam.
func (d *docSink) textProgress(page, total int, what string) localllm.TextParams {
	return localllm.TextParams{
		OnStage: func(stage string) {
			logx.Progress(d.ocred, total, "hal %d · %s: %s", page, what, stage)
		},
		OnToken: func(n int) {
			if n%32 == 0 {
				logx.Progress(d.ocred, total, "hal %d · %s: %d token", page, what, n)
			}
		},
	}
}

// cropNote merangkum penghematan piksel hasil pemotongan margin, atau string
// kosong bila pemotongannya tidak berarti.
func cropNote(pct float64) string {
	if pct < 1 {
		return ""
	}
	return fmt.Sprintf(" · dipotong -%.0f%%", pct)
}

func durText(ms int) string {
	return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
}
