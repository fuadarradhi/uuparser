// Package prompts memuat berkas prompt dari disk (prompts/*.md) supaya dapat
// disunting tanpa membangun ulang binari.
//
// Prompt sengaja DIPECAH menjadi beberapa berkas kecil, satu pertanyaan per
// berkas. Model teks yang dipakai berukuran kecil; satu prompt panjang yang
// meminta banyak hal sekaligus membuatnya kehilangan sebagian instruksi —
// gejalanya antara lain menyalin contoh di dalam prompt sebagai jawaban.
// Pertanyaan yang sempit jauh lebih jarang meleset, dengan harga beberapa
// panggilan tambahan yang hanya terjadi sekali per dokumen.
package prompts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Set memuat seluruh prompt yang dipakai pipeline.
type Set struct {
	Gate      string // apakah halaman ini produk hukum?
	Identity  string // jenis, instansi, nomor, tahun, tentang
	Penetapan string // tempat/tanggal/penanda tangan penetapan & pengundangan
	// Tinjau (2026-07-23): dipakai HANYA saat parser sendiri mencurigai
	// hasilnya (lihat parser.AnchorLeakNodes + pipeline's tinjauan
	// mechanism) — model diminta menilai APAKAH satu potongan node hasil
	// parse benar-benar tercampur dua bagian berbeda, dan menjelaskan
	// singkat kalau ya. Beda dari Gate/Identity/Penetapan: prompt ini
	// TIDAK PERNAH dipakai untuk mengisi kolom data — jawabannya murni
	// catatan tinjauan untuk manusia, tidak pernah divalidasi/disimpan
	// sebagai nilai terstruktur.
	Tinjau string

	Hash string // sidik jari gabungan, disimpan bersama metadata dokumen
}

// Load membaca seluruh berkas prompt dari dir.
func Load(dir string) (Set, error) {
	var s Set
	var err error
	var hashes []string

	type item struct {
		name string
		dst  *string
	}
	for _, it := range []item{
		{"gate.md", &s.Gate},
		{"identity.md", &s.Identity},
		{"penetapan.md", &s.Penetapan},
		{"tinjau.md", &s.Tinjau},
	} {
		var h string
		if *it.dst, h, err = readOne(filepath.Join(dir, it.name)); err != nil {
			return s, err
		}
		hashes = append(hashes, h)
	}

	sum := sha256.Sum256([]byte(strings.Join(hashes, "|")))
	s.Hash = hex.EncodeToString(sum[:])[:16]
	return s, nil
}

func readOne(path string) (content, hash string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("membaca prompt %s: %w — pastikan folder prompts/ "+
			"ada di sebelah binari (atau tunjuk lewat PROMPT_DIR)", path, err)
	}
	text := strings.TrimSpace(string(b))
	if text == "" {
		return "", "", fmt.Errorf("prompt %s kosong", path)
	}
	sum := sha256.Sum256([]byte(text))
	return text, hex.EncodeToString(sum[:])[:16], nil
}
