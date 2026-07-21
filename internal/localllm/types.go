package localllm

// Params mengatur satu permintaan OCR.
type Params struct {
	// MaxTokens membatasi panjang keluaran satu halaman. Ini pengaman terakhir
	// bila model tidak berhenti sendiri.
	MaxTokens int

	// OnStage dan OnToken melaporkan kemajuan SELAMA satu halaman diproses.
	//
	// Tanpa keduanya, konsol diam total selama beberapa menit: penyandian
	// gambar (bagian paling lambat) terjadi sebelum token pertama keluar,
	// sehingga proses tampak macet padahal berjalan normal. Keduanya boleh
	// nil bila pemanggil tidak membutuhkannya.
	//
	// OnStage menandai peralihan tahap ("menyandikan gambar", "menghasilkan
	// teks"); OnToken dipanggil tiap token dengan jumlah token sejauh ini —
	// pemanggil sendiri yang menentukan seberapa sering menampilkannya.
	OnStage func(stage string)
	OnToken func(n int)
}

// Result adalah hasil satu permintaan.
type Result struct {
	Text string
	// Truncated: model berhenti karena batas token, bukan karena selesai sendiri.
	// Teks halaman kemungkinan tidak lengkap, jadi selalu ditandai.
	Truncated bool
}
