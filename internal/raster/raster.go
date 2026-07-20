// Package raster merasterisasi halaman PDF menjadi PNG memakai go-fitz (MuPDF).
//
// Dokumen dibuka SEKALI lalu halaman dirender berulang lewat handle yang sama,
// sehingga PDF tebal tidak perlu diurai ulang setiap halaman.
//
// Catatan build: go-fitz memakai CGo, jadi diperlukan compiler C saat membangun.
// Di Linux cukup `build-essential`. (Di Windows go-fitz rewel karena pustaka
// MuPDF pra-bangunnya harus cocok persis dengan versi mingw-w64 — lihat README.)
package raster

import (
	"bytes"
	"fmt"
	"image"
	"image/png"

	"github.com/gen2brain/go-fitz"
)

// Doc adalah handle dokumen PDF yang terbuka.
type Doc struct {
	doc  *fitz.Document
	path string
}

// Open membuka PDF. Pemanggil wajib memanggil Close.
func Open(path string) (*Doc, error) {
	d, err := fitz.New(path)
	if err != nil {
		return nil, fmt.Errorf("buka pdf: %w", err)
	}
	return &Doc{doc: d, path: path}, nil
}

// NumPages mengembalikan jumlah halaman.
func (d *Doc) NumPages() int { return d.doc.NumPage() }

// Page adalah hasil render satu halaman.
type Page struct {
	PNG []byte
	W   int
	H   int
	// InkRatio adalah proporsi piksel gelap (0..1). Halaman kosong bernilai ~0.
	// Dipakai untuk melewati halaman hampa tanpa memanggil model OCR.
	InkRatio float64
	// CroppedFrom adalah jumlah piksel halaman sebelum pemotongan/pengecilan,
	// untuk melaporkan berapa banyak yang dihemat.
	CroppedFrom int
}

// Opts mengatur pengolahan gambar halaman.
type Opts struct {
	DPI int
	// AutoCrop memotong margin kosong halaman. Ini memangkas banyak piksel TANPA
	// mengecilkan huruf sama sekali — ukuran karakter tetap sama tajamnya —
	// sehingga OCR lebih cepat tanpa mengorbankan ketelitian.
	//
	// Pengecilan gambar sengaja tidak disediakan: mengecilkan huruf membuat angka
	// pasal/ayat rawan salah baca, dan satu digit keliru merusak struktur dokumen.
	AutoCrop bool
}

// PagePNG merender satu halaman tanpa pengolahan tambahan.
func (d *Doc) PagePNG(page, dpi int) ([]byte, error) {
	p, err := d.Render(page, Opts{DPI: dpi})
	if err != nil {
		return nil, err
	}
	return p.PNG, nil
}

// PagePNGMax merender satu halaman (1-based) pada DPI tertentu, lalu MENGECILKAN
// gambar bila sisi terpanjangnya melebihi maxPx (0 = tanpa batas).
//
// Ini pengungkit kecepatan utama: waktu proses model visi kira-kira sebanding
// dengan luas piksel gambar. Halaman A4 pada 200 DPI berukuran 1654x2339 (≈3,9
// juta piksel); membatasinya ke sisi 1600 px memangkas luasnya lebih dari
// setengah, dengan dampak kecil pada keterbacaan teks badan peraturan.
//
// Render merender satu halaman (1-based) lalu menerapkan pemotongan margin
// dan/atau pembatasan ukuran sesuai Opts.
//
// Mengembalikan PNG beserta dimensi akhir dan proporsi tinta halaman.
func (d *Doc) Render(page int, o Opts) (Page, error) {
	dpi := o.DPI
	if dpi <= 0 {
		dpi = 200
	}
	if page < 1 || page > d.NumPages() {
		return Page{}, fmt.Errorf("halaman %d di luar jangkauan (1..%d)", page, d.NumPages())
	}
	img, err := d.doc.ImageDPI(page-1, float64(dpi)) // go-fitz memakai indeks 0-based
	if err != nil {
		return Page{}, fmt.Errorf("render hal %d: %w", page, err)
	}
	full := img.Bounds()
	if o.AutoCrop {
		img = cropMargins(img, cropPadding)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return Page{}, err
	}
	b := img.Bounds()
	pg := Page{PNG: buf.Bytes(), W: b.Dx(), H: b.Dy(), InkRatio: inkRatio(img)}
	if fullPx := full.Dx() * full.Dy(); fullPx > 0 {
		pg.CroppedFrom = fullPx
	}
	return pg, nil
}

// inkRatio menghitung proporsi piksel gelap terhadap seluruh piksel. Dipakai untuk
// mengenali halaman kosong (atau nyaris kosong) sebelum dikirim ke model OCR.
// Piksel dicuplik (tidak semua diperiksa) agar tetap murah pada gambar besar.
func inkRatio(img image.Image) float64 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return 0
	}
	step := 1
	if w*h > 400_000 { // cuplik agar biaya tetap kecil pada halaman besar
		step = 3
	}
	var dark, total uint64
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			r, g, bl, _ := img.At(x, y).RGBA()
			// luminansi kasar pada skala 16-bit
			lum := (uint64(r)*299 + uint64(g)*587 + uint64(bl)*114) / 1000
			if lum < 40000 { // ambang gelap (~61% dari putih penuh)
				dark++
			}
			total++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(dark) / float64(total)
}

// Close melepaskan sumber daya dokumen.
func (d *Doc) Close() error { return d.doc.Close() }

// cropMargins memotong margin kosong di sekeliling isi halaman.
//
// Ini pengungkit kecepatan yang tidak mengorbankan ketelitian: jumlah piksel
// berkurang banyak, tetapi ukuran huruf sama sekali tidak berubah — berbeda dengan
// pengecilan gambar yang membuat angka pasal/ayat lebih rawan salah baca.
//
// Pemotongan sengaja AGRESIF: halaman A4 yang hanya berisi satu atau dua baris
// akan terpotong menjadi seukuran baris itu saja, sehingga tidak ada piksel putih
// yang dikirim ke model.
//
// Pengamannya bukan batas luas minimum — halaman satu baris memang seharusnya
// menghasilkan potongan yang sangat kecil. Yang dijaga adalah keutuhan isi:
// potongan harus menahan hampir seluruh piksel bertinta halaman. Bila tidak
// (misalnya ada baris samar yang kepadatannya di bawah ambang), deteksi diulang
// dengan ambang paling ketat sehingga tidak ada isi yang terbuang.
// cropPadding adalah ruang tepi (piksel) yang disisakan di sekeliling isi.
const cropPadding = 12

func cropMargins(img image.Image, pad int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 40 || h < 40 {
		return img
	}

	dark := func(x, y int) bool {
		r, g, bl, _ := img.At(x, y).RGBA()
		lum := (uint64(r)*299 + uint64(g)*587 + uint64(bl)*114) / 1000
		return lum < 40000
	}

	// [BUGFIX] Hilangkan step untuk akurasi piksel persis. Loop Go sangat cepat
	// untuk ukuran w*h standar (2-4 juta piksel), dan ini menghindari bug indeks
	// array meleset akibat step > 1.
	rowInk := make([]int, h)
	colInk := make([]int, w)
	total := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if dark(b.Min.X+x, b.Min.Y+y) {
				rowInk[y]++
				colInk[x]++
				total++
			}
		}
	}
	if total == 0 {
		return img // halaman kosong: tidak ada yang bisa dijadikan acuan
	}

	// Percobaan pertama memakai ambang kecil agar bintik noda dan bayangan tepi
	// hasil pindai tidak menggagalkan pemotongan.
	rowMin := maxInt(2, w/500)
	colMin := maxInt(2, h/500)
	top, bottom, left, right, ok := inkBox(rowInk, colInk, rowMin, colMin)

	// Verifikasi keutuhan: bila ada tinta di luar kotak (mis. baris samar), ulangi
	// dengan ambang paling ketat sehingga seluruh isi pasti tercakup.
	if ok && !keepsAllInk(rowInk, colInk, top, bottom, left, right, total) {
		top, bottom, left, right, ok = inkBox(rowInk, colInk, 1, 1)
	}
	if !ok {
		return img
	}

	if pad <= 0 {
		pad = 12
	}
	top = maxInt(0, top-pad)
	left = maxInt(0, left-pad)
	bottom = minInt(h-1, bottom+pad)
	right = minInt(w-1, right+pad)

	newW, newH := right-left+1, bottom-top+1
	if newW < 16 || newH < 8 {
		return img // hasil tak masuk akal: kemungkinan deteksi gagal
	}
	// Tidak ada gunanya memotong bila hasilnya nyaris sama dengan aslinya.
	if float64(newW*newH)/float64(w*h) > 0.98 {
		return img
	}

	rect := image.Rect(b.Min.X+left, b.Min.Y+top, b.Min.X+right+1, b.Min.Y+bottom+1)
	type subImager interface {
		SubImage(r image.Rectangle) image.Image
	}
	if si, ok := img.(subImager); ok {
		return si.SubImage(rect)
	}
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	for y := 0; y < newH; y++ {
		for x := 0; x < newW; x++ {
			dst.Set(x, y, img.At(rect.Min.X+x, rect.Min.Y+y))
		}
	}
	return dst
}

// inkBox mencari kotak pembatas isi berdasarkan ambang kepadatan yang diberikan.
// [BUGFIX] Parameter step dihilangkan agar firstAbove mengecek setiap indeks.
func inkBox(rowInk, colInk []int, rowMin, colMin int) (top, bottom, left, right int, ok bool) {
	top = firstAbove(rowInk, rowMin, false)
	bottom = firstAbove(rowInk, rowMin, true)
	left = firstAbove(colInk, colMin, false)
	right = firstAbove(colInk, colMin, true)
	ok = top >= 0 && bottom >= top && left >= 0 && right >= left
	return
}

// keepsAllInk memeriksa apakah kotak menahan hampir seluruh tinta halaman.
// Inilah pengaman sesungguhnya: yang penting bukan luas sisa halaman, melainkan
// tidak ada isi yang terbuang.
func keepsAllInk(rowInk, colInk []int, top, bottom, left, right, total int) bool {
	const minRetained = 0.995

	inRows := 0
	for i := top; i <= bottom && i < len(rowInk); i++ {
		inRows += rowInk[i]
	}
	inCols := 0
	for i := left; i <= right && i < len(colInk); i++ {
		inCols += colInk[i]
	}
	return float64(inRows) >= float64(total)*minRetained &&
		float64(inCols) >= float64(total)*minRetained
}

// firstAbove mencari indeks pertama (atau terakhir bila fromEnd) yang nilainya
// melampaui ambang.
// [BUGFIX] Menghapus parameter step agar tidak melewatkan indeks ganjil (misal y=1).
func firstAbove(vals []int, min int, fromEnd bool) int {
	if fromEnd {
		for i := len(vals) - 1; i >= 0; i-- {
			if vals[i] >= min {
				return i
			}
		}
		return -1
	}
	for i := 0; i < len(vals); i++ {
		if vals[i] >= min {
			return i
		}
	}
	return -1
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
