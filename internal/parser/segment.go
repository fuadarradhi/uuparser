package parser

import "strings"

// segment.go memotong daftar baris (dengan halaman asal masing-masing) menjadi
// beberapa macro-section berdasarkan anchor kata kunci besar. Satu kali scan
// linear; tidak peduli hierarki Bab/Pasal.

type segment struct {
	section Section
	lines   []Line
}

// segmentize membagi baris menjadi urutan segmen sesuai kemunculan anchor.
// Juga mengembalikan daftar warning level-dokumen (mis. section standar hilang).
func segmentize(lines []Line, cls classifyResult) ([]segment, []Warning) {
	// LAMPIRAN (2026-07-23): selalu jadi bagian PALING AKHIR dokumen —
	// dokumennya sendiri, dengan identitas ulang dan sub-bagian A/B/C/dst
	// sendiri. Dipotong PALING DULU dari sisa analisis di bawah supaya
	// tidak ada anchor Menimbang/Mengingat/dst di dalamnya yang salah
	// tertangkap sebagai bagian dokumen utama, dan supaya reClosing di
	// parseBatangTubuh (lihat parse_batangtubuh.go) tidak tersedot terus
	// menelan Lampiran sebagai node penutup.
	var lampiranLines []Line
	if idxLampiran := findLineIndex(lines, reLampiran); idxLampiran >= 0 {
		lampiranLines = sliceLines(lines, idxLampiran, len(lines))
		lines = sliceLines(lines, 0, idxLampiran)
	}

	// Tentukan indeks anchor utama.
	idxMenimbang := findLineIndex(lines, reMenimbang)
	idxMengingat := findLineIndex(lines, reMengingat)
	// idxMemperhatikan: section OPSIONAL setelah Mengingat, sebelum
	// MEMUTUSKAN — lihat catatan reMemperhatikan di patterns.go.
	idxMemperhatikan := findLineIndex(lines, reMemperhatikan)
	idxMemutuskan := findLineIndex(lines, reMemutuskan)
	idxMenetapkan := findLineIndexFrom(lines, reMenetapkan, maxInt(idxMemutuskan, 0))
	idxPenjelasan := findLineIndex(lines, rePenjelasan)

	// Awal batang tubuh: setelah "Menetapkan :" (baris penetapan) bila ada,
	// jika tidak, setelah MEMUTUSKAN, jika tidak, dari Pasal/BAB pertama.
	batangStart := -1
	switch {
	case idxMenetapkan >= 0:
		batangStart = afterWrappedTitle(lines, idxMenetapkan)
	case idxMemutuskan >= 0:
		batangStart = afterWrappedTitle(lines, idxMemutuskan)
	default:
		batangStart = firstStructuralIndex(lines)
		if batangStart < 0 {
			// [Diperbaiki 2026-07-24, permintaan user] Dokumen tanpa Bab/
			// Pasal/Diktum DAN tanpa MEMUTUSKAN/Menetapkan formal (mis.
			// Surat Edaran naratif biasa) SEBELUMNYA membiarkan batangStart
			// tetap -1 di sini. Akibatnya minPositiveAfter (dipakai di
			// bawah untuk menentukan akhir segmen Menimbang/Mengingat/
			// Memperhatikan) jatuh ke fallback-nya (len(lines)) — section
			// pembuka TERAKHIR yang ada menyedot SISA DOKUMEN SAMPAI AKHIR
			// (termasuk paragraf penutup & tanda tangan) ke dalam
			// segmennya sendiri. Bug NYATA yang ditemukan lewat pengujian:
			// penutup Surat Edaran tergabung jadi SATU KALIMAT dengan
			// kutipan Mengingat terakhir — bukan sekadar hilang, tapi
			// mengotori data yang justru harus akurat.
			//
			// Sekarang: cari baris kosong pertama SETELAH section pembuka
			// terakhir yang ada, jadikan itu batas awal batang_tubuh —
			// memisahkan sisa teks (penutup/ttd) dari konsiderans, alih-
			// alih tersedot ke dalamnya. parseBatangTubuh (lihat file
			// terpisah) tetap menanganinya apa adanya sebagai teks datar
			// (minimal jadi DOC_WARNING dengan teks orphan yang TERLIHAT,
			// bukan lenyap/tercampur diam-diam ke section lain).
			lastOpening := idxMenimbang
			if idxMengingat > lastOpening {
				lastOpening = idxMengingat
			}
			if idxMemperhatikan > lastOpening {
				lastOpening = idxMemperhatikan
			}
			if lastOpening >= 0 {
				batangStart = firstBlankLineAfter(lines, lastOpening)
			}
		}
	}

	// fullyNarrative (2026-07-24, permintaan user): TIDAK ADA anchor apa
	// pun ditemukan — bukan cuma tanpa Pasal, tapi juga tanpa Menimbang/
	// Mengingat/Memperhatikan/Memutuskan/Menetapkan/BAB/Diktum sama
	// sekali. Ditemukan nyata pada Surat Edaran yang langsung membuka
	// paragraf naratif ("Dalam rangka ...") lalu poin bernomor, tanpa kata
	// kunci konsiderans apa pun. Peraturan/Qanun/Keputusan asli TIDAK
	// PERNAH masuk kondisi ini (selalu punya salah satu anchor di atas).
	fullyNarrative := batangStart < 0 && idxMenimbang < 0 && idxMengingat < 0 &&
		idxMemperhatikan < 0 && idxMemutuskan < 0 && idxMenetapkan < 0

	// Akhir batang tubuh: sebelum PENJELASAN bila ada, jika tidak sampai akhir.
	batangEnd := len(lines)
	if idxPenjelasan >= 0 {
		batangEnd = idxPenjelasan
	}

	var segs []segment
	var warns []Warning

	// --- Judul: dari awal sampai anchor pembuka pertama. ---
	firstAnchor := minPositive(idxMenimbang, idxMengingat, idxMemperhatikan, idxMemutuskan)
	if firstAnchor < 0 {
		firstAnchor = batangStart
	}
	if firstAnchor > 0 {
		segs = append(segs, segment{SectionJudul, sliceLines(lines, 0, firstAnchor)})
	}

	// --- Menimbang ---
	if idxMenimbang >= 0 {
		end := minPositiveAfter(len(lines), idxMenimbang, idxMengingat, idxMemutuskan, batangStart)
		segs = append(segs, segment{SectionMenimbang, sliceLines(lines, idxMenimbang, end)})
	} else if cls.hasPasal {
		warns = append(warns, Warning{SeverityNeedsReview, "Section 'Menimbang' tidak ditemukan", nil, ""})
	}

	// --- Mengingat ---
	if idxMengingat >= 0 {
		end := minPositiveAfter(len(lines), idxMengingat, idxMemperhatikan, idxMemutuskan, batangStart)
		segs = append(segs, segment{SectionMengingat, sliceLines(lines, idxMengingat, end)})
	} else if cls.hasPasal {
		warns = append(warns, Warning{SeverityNeedsReview, "Section 'Mengingat' tidak ditemukan", nil, ""})
	}

	// --- Memperhatikan (OPSIONAL — banyak dokumen tidak punya section ini
	// sama sekali, jadi TIDAK ada warning saat absen). ---
	if idxMemperhatikan >= 0 {
		end := minPositiveAfter(len(lines), idxMemperhatikan, idxMemutuskan, batangStart)
		segs = append(segs, segment{SectionMemperhatikan, sliceLines(lines, idxMemperhatikan, end)})
	}

	// --- Penetapan (MEMUTUSKAN..Menetapkan) ---
	if idxMemutuskan >= 0 {
		end := batangStart
		if end < 0 || end <= idxMemutuskan {
			end = idxMemutuskan + 1
		}
		segs = append(segs, segment{SectionPenetapan, sliceLines(lines, idxMemutuskan, end)})
	} else if cls.hasPasal {
		warns = append(warns, Warning{SeverityNeedsReview, "Section 'MEMUTUSKAN/Menetapkan' tidak ditemukan", nil, ""})
	}

	// --- Batang tubuh ---
	switch {
	case batangStart >= 0 && batangStart < batangEnd:
		segs = append(segs, segment{SectionBatangTubuh, sliceLines(lines, batangStart, batangEnd)})
	case fullyNarrative:
		// [Ditambahkan 2026-07-24, permintaan user] TIDAK ada anchor apa
		// pun (lihat catatan fullyNarrative di atas) — jadikan SELURUH isi
		// (0..batangEnd) satu section narasi, diurai parseNarasi (poin
		// bernomor/berhuruf jadi NodeItem, sisanya paragraf biasa) alih-
		// alih parseBatangTubuh yang menuntut Pasal/Ayat. Tanpa ini,
		// dokumen begitu berakhir 0 node sama sekali walau sudah lolos
		// gerbang lewat ParseAllowNonRegulation.
		if batangEnd > 0 {
			segs = append(segs, segment{SectionNarasi, sliceLines(lines, 0, batangEnd)})
		}
	default:
		warns = append(warns, Warning{SeverityNeedsReview, "Batang tubuh (Pasal-pasal) tidak terdeteksi", nil, ""})
	}

	// --- Penjelasan: pisah Umum vs Pasal Demi Pasal. ---
	if idxPenjelasan >= 0 {
		penjLines := sliceLines(lines, idxPenjelasan, len(lines))
		umumSegs := splitPenjelasan(penjLines)
		segs = append(segs, umumSegs...)
	}

	// --- Lampiran: SELALU paling akhir (lihat pemotongan di awal fungsi). ---
	if len(lampiranLines) > 0 {
		segs = append(segs, segment{SectionLampiran, lampiranLines})
	}

	return segs, warns
}

// splitPenjelasan memisah blok PENJELASAN menjadi penjelasan_umum & penjelasan_pasal.
func splitPenjelasan(lines []Line) []segment {
	idxUmum := findLineIndex(lines, reUmumHead)
	idxPasalDemi := findLineIndex(lines, rePasalDemi)

	var out []segment
	switch {
	case idxPasalDemi >= 0:
		// Umum = dari setelah header sampai sebelum "PASAL DEMI PASAL".
		umumStart := 0
		if idxUmum >= 0 && idxUmum < idxPasalDemi {
			umumStart = idxUmum
		}
		if idxPasalDemi > umumStart {
			out = append(out, segment{SectionPenjelasanUmum, sliceLines(lines, umumStart, idxPasalDemi)})
		}
		out = append(out, segment{SectionPenjelasanPasal, sliceLines(lines, idxPasalDemi, len(lines))})
	case idxUmum >= 0:
		out = append(out, segment{SectionPenjelasanUmum, sliceLines(lines, idxUmum, len(lines))})
	default:
		// Tidak ada sub-header jelas: perlakukan seluruhnya sebagai penjelasan_umum,
		// namun bila mengandung pola Pasal, arahkan ke penjelasan_pasal.
		joined := joinLineText(lines)
		if rePasalAnywhere.MatchString(joined) {
			out = append(out, segment{SectionPenjelasanPasal, lines})
		} else {
			out = append(out, segment{SectionPenjelasanUmum, lines})
		}
	}
	return out
}

func joinLineText(lines []Line) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = l.Text
	}
	return strings.Join(parts, "\n")
}

// ---- util indeks ----

func findLineIndex(lines []Line, re interface{ MatchString(string) bool }) int {
	return findLineIndexFrom(lines, re, 0)
}

func findLineIndexFrom(lines []Line, re interface{ MatchString(string) bool }, from int) int {
	if from < 0 {
		from = 0
	}
	for i := from; i < len(lines); i++ {
		if re.MatchString(lines[i].Text) {
			return i
		}
	}
	return -1
}

// firstStructuralIndex mencari baris struktural pertama (BAB, Pasal, atau
// Diktum). [Diperbaiki 2026-07-24, ditemukan nyata di debug/34] SEBELUMNYA
// hanya mengenali BAB/Pasal — dokumen Instruksi/Keputusan yang langsung
// membuka diktum (KESATU/KEDUA/dst) TANPA Menimbang/Mengingat/Memutuskan
// SAMA SEKALI (format sah, lihat classify.go/hasDiktumAnchor) tidak punya
// titik awal batang_tubuh sama sekali — segmentize() tidak pernah membuat
// segmen batang_tubuh, dan SELURUH isi diktum (justru substansi
// keputusannya) hilang total (0 node).
func firstStructuralIndex(lines []Line) int {
	for i, ln := range lines {
		m := detectStructural(ln.Text)
		if m.kind == mkBab || m.kind == mkPasal || m.kind == mkDiktum {
			return i
		}
	}
	return -1
}

// afterWrappedTitle mengembalikan indeks baris SETELAH baris anchor
// (Menetapkan/MEMUTUSKAN) di `anchorIdx`, dengan aman menangani judul
// peraturan yang MELEBAR ke beberapa baris cetak.
//
// [Diperbaiki 2026-07-24, ditemukan nyata di debug/39 & debug/47] Sebelumnya
// SELALU `anchorIdx + 1` — asumsi diam-diam bahwa "Menetapkan : <judul>"
// selalu satu baris cetak. Untuk peraturan berjudul panjang, kalimat itu
// MELEBAR ke 2-3 baris cetak tanpa baris kosong di antaranya (baru ada
// baris kosong SETELAH kalimat selesai) — mis. "Menetapkan : PERATURAN
// GUBERNUR TENTANG PERUBAHAN PENETAPAN\nRENCANA KERJA SATUAN KERJA
// PERANGKAT ACEH TAHUN\n2025." (debug/39, 3 baris cetak). `anchorIdx + 1`
// jatuh PERSIS di tengah kalimat yang melebar itu ("RENCANA KERJA..."),
// bukan di "Pasal 1" yang sebenarnya — akibatnya SectionPenetapan
// kehilangan sisa judulnya sendiri, DAN batang_tubuh dimulai dari
// potongan judul itu (bukan Pasal 1), yang lalu nyasar jadi orphan
// tertempel di Pasal 1 sebagai warning "before" (data tetap ada, tapi
// salah tempat dan memicu NODE_WARNINGS tanpa perlu).
//
// Fix: HANYA lanjut mencari baris kosong berikutnya (firstBlankLineAfter)
// bila baris tepat setelah anchor BUKAN baris kosong maupun baris
// struktural (Bab/Pasal/Ayat/Diktum) — yakni benar-benar potongan
// kalimat biasa yang melebar. Bila baris berikutnya SUDAH kosong atau
// SUDAH struktural (kasus umum, judul muat satu baris), perilaku PERSIS
// sama seperti sebelumnya (`anchorIdx + 1`) — tidak ada regresi untuk
// dokumen yang sudah benar.
func afterWrappedTitle(lines []Line, anchorIdx int) int {
	next := anchorIdx + 1
	if next >= len(lines) {
		return next
	}
	t := strings.TrimSpace(lines[next].Text)
	if t == "" || detectStructural(t).kind != mkNone {
		return next
	}
	return firstBlankLineAfter(lines, anchorIdx)
}

// firstBlankLineAfter mencari indeks baris kosong PERTAMA setelah `after`
// (batas paragraf asli, sudah disimpan stitch.go) — dipakai sebagai batas
// darurat awal batang_tubuh saat dokumen tidak punya Bab/Pasal/Diktum maupun
// MEMUTUSKAN/Menetapkan formal (lihat catatan di segmentize). Mengembalikan
// len(lines) bila tak ada baris kosong lagi (sisa dokumen jadi kosong,
// bukan error).
func firstBlankLineAfter(lines []Line, after int) int {
	for i := after + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i].Text) == "" {
			return i
		}
	}
	return len(lines)
}

func sliceLines(lines []Line, a, b int) []Line {
	if a < 0 {
		a = 0
	}
	if b > len(lines) {
		b = len(lines)
	}
	if a >= b {
		return nil
	}
	cp := make([]Line, b-a)
	copy(cp, lines[a:b])
	return cp
}

func minPositive(vals ...int) int {
	best := -1
	for _, v := range vals {
		if v >= 0 && (best < 0 || v < best) {
			best = v
		}
	}
	return best
}

// minPositiveAfter: nilai positif terkecil yang > after, di antara vals.
// Bila tidak ada, kembalikan fallback.
func minPositiveAfter(fallback int, vals ...int) int {
	after := vals[0]
	best := -1
	for _, v := range vals[1:] {
		if v > after && (best < 0 || v < best) {
			best = v
		}
	}
	if best < 0 {
		return fallback
	}
	return best
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
