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
		batangStart = idxMenetapkan + 1
	case idxMemutuskan >= 0:
		batangStart = idxMemutuskan + 1
	default:
		batangStart = firstStructuralIndex(lines)
	}

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
	if batangStart >= 0 && batangStart < batangEnd {
		segs = append(segs, segment{SectionBatangTubuh, sliceLines(lines, batangStart, batangEnd)})
	} else {
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

// firstStructuralIndex mencari baris struktural pertama (BAB atau Pasal).
func firstStructuralIndex(lines []Line) int {
	for i, ln := range lines {
		m := detectStructural(ln.Text)
		if m.kind == mkBab || m.kind == mkPasal {
			return i
		}
	}
	return -1
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
