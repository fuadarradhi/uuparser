package parser

// Line adalah satu baris teks OCR beserta nomor halaman asalnya (1-indexed).
// Menggantikan bare string di seluruh pipeline internal (stitch -> segment ->
// sub-parser -> builder) supaya tiap Node bisa melaporkan StartPage/EndPage —
// dipakai UI nanti untuk "klik node -> lihat halaman PDF aslinya".
type Line struct {
	Text string
	Page int
}
