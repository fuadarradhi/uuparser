package localllm

// Params mengatur satu permintaan OCR.
type Params struct {
	// MaxTokens membatasi panjang keluaran satu halaman. Ini pengaman terakhir
	// bila model tidak berhenti sendiri.
	MaxTokens int
}

// Result adalah hasil satu permintaan.
type Result struct {
	Text string
	// Truncated: model berhenti karena batas token, bukan karena selesai sendiri.
	// Teks halaman kemungkinan tidak lengkap, jadi selalu ditandai.
	Truncated bool
}
