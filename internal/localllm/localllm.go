// Package localllm menjalankan model langsung di dalam proses lewat yzma
// (pengikat Go untuk llama.cpp). Tidak ada server inferensi dan tidak ada
// permintaan keluar — satu-satunya lalu lintas jaringan aplikasi ini adalah
// pengunduhan PDF.
//
// Model dimuat SEKALI lalu dipakai untuk seluruh dokumen dan seluruh halaman;
// tidak ada buka-tutup per dokumen. Pemuatan bersifat malas (baru terjadi saat
// halaman pertama benar-benar perlu di-OCR), dan Release dapat dipanggil ketika
// tidak ada lagi pekerjaan agar memori tidak tertahan selama service menunggu
// siklus berikutnya.
//
// Yang dibutuhkan: pustaka llama.cpp (libllama, libmtmd), berkas model GGUF,
// dan berkas proyektor multimodal (mmproj).
package localllm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/hybridgroup/yzma/pkg/llama"
	"github.com/hybridgroup/yzma/pkg/mtmd"
)

// Nilai-nilai berikut sengaja tetap, bukan pengaturan.
//
// Suhu 0 berarti pengambilan token greedy: keluaran OCR dapat diulang persis,
// dan TopK/TopP/MinP tidak berpengaruh sama sekali sehingga tidak ada gunanya
// disediakan sebagai konfigurasi. Sampler DRY dibiarkan nonaktif (bawaan
// llama.cpp) karena pengulangan tak berujung yang pernah ditemui berasal dari
// Ollama, bukan dari llama.cpp.
const (
	samplerTemp = 0.0

	// initialCtx dipilih dengan perhitungan, bukan angka bawaan.
	//
	// Satu halaman A4 pada 200 DPI menjadi kira-kira 4100 token visi. Jendela
	// konteks harus memuat token gambar DAN seluruh keluaran (OCR_MAX_TOKENS,
	// baku 2048) sekaligus, sehingga 4096 pasti kurang — inilah galat
	// "exceeds the available context size" yang khas. 8192 memberi ruang untuk
	// halaman penuh beserta keluarannya.
	//
	// Biayanya kecil: KV cache model sekelas GLM-OCR (0,9B) pada 8192 token
	// hanya beberapa megabita.
	initialCtx = 8192

	// maxCtx membatasi pertumbuhan otomatis (lihat growContext).
	maxCtx = 32768

	nBatch        = 2048
	chatBufferLen = 8192
)

// Config hanya memuat hal yang memang berbeda di tiap mesin.
type Config struct {
	ModelPath  string // berkas model GGUF
	MMProjPath string // berkas proyektor multimodal (mmproj)
	LibPath    string // folder berisi libllama/libmtmd (kosong = jalur sistem)
	Verbose    bool   // tampilkan log llama.cpp
}

// Client memegang model yang sudah dimuat. Aman dipakai berulang; pemakaian
// diserialkan karena satu konteks llama.cpp tidak boleh dipakai bersamaan.
type Client struct {
	cfg Config

	mu     sync.Mutex
	loaded bool

	model    llama.Model
	lctx     llama.Context
	curCtx   uint32 // ukuran jendela konteks yang sedang dipakai
	mctx     mtmd.Context
	vocab    llama.Vocab
	sampler  llama.Sampler
	template string
	marker   string
}

// libsOnce menjaga agar pustaka bersama hanya dimuat sekali seumur proses,
// meski model dilepas lalu dimuat ulang di antara siklus.
var (
	libsOnce sync.Once
	libsErr  error
)

// LoadError menandai kegagalan menyiapkan model: pustaka tidak ditemukan, berkas
// model tidak terbaca, dan sejenisnya.
//
// Kegagalan seperti ini bersifat lingkungan, bukan milik satu dokumen. Pemanggil
// sebaiknya menghentikan siklus alih-alih menandai tiap dokumen gagal — bila
// tidak, satu pustaka yang belum terpasang akan menghabiskan jatah percobaan
// semua dokumen.
type LoadError struct{ Err error }

func (e *LoadError) Error() string { return e.Err.Error() }
func (e *LoadError) Unwrap() error { return e.Err }

// IsLoadError melaporkan apakah galat berasal dari penyiapan model.
func IsLoadError(err error) bool {
	var le *LoadError
	return errors.As(err, &le)
}

// New menyiapkan klien TANPA memuat model. Model baru dimuat ketika halaman
// pertama perlu di-OCR, sehingga siklus yang tidak menemukan pekerjaan tidak
// menyita memori sama sekali.
func New(c Config) (*Client, error) {
	if strings.TrimSpace(c.ModelPath) == "" {
		return nil, fmt.Errorf("MODEL_PATH kosong")
	}
	if strings.TrimSpace(c.MMProjPath) == "" {
		return nil, fmt.Errorf("MMPROJ_PATH kosong — model visi memerlukan berkas proyektor multimodal (mmproj)")
	}
	return &Client{cfg: c}, nil
}

// Model mengembalikan path model, untuk keperluan log.
func (c *Client) Model() string { return c.cfg.ModelPath }

// Loaded melaporkan apakah model sedang berada di memori.
func (c *Client) Loaded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loaded
}

// ensureLoaded memuat model bila belum termuat. Pemanggil sudah memegang kunci.
func (c *Client) ensureLoaded() error {
	if c.loaded {
		return nil
	}

	libsOnce.Do(func() {
		if err := llama.Load(c.cfg.LibPath); err != nil {
			libsErr = fmt.Errorf("memuat pustaka llama: %w", err)
			return
		}
		if err := mtmd.Load(c.cfg.LibPath); err != nil {
			libsErr = fmt.Errorf("memuat pustaka mtmd: %w", err)
			return
		}
		if !c.cfg.Verbose {
			llama.LogSet(llama.LogSilent())
			mtmd.LogSet(llama.LogSilent())
		}
	})
	if libsErr != nil {
		return libsErr
	}

	llama.Init()

	model, err := llama.ModelLoadFromFile(c.cfg.ModelPath, llama.ModelDefaultParams())
	if err != nil {
		llama.Close()
		return fmt.Errorf("memuat model %s: %w", c.cfg.ModelPath, err)
	}
	if model == 0 {
		llama.Close()
		return fmt.Errorf("gagal memuat model dari %s", c.cfg.ModelPath)
	}

	if c.curCtx == 0 {
		c.curCtx = initialCtx
	}
	lctx, err := newContext(model, c.curCtx)
	if err != nil {
		llama.ModelFree(model)
		llama.Close()
		return err
	}

	mctx, err := mtmd.InitFromFile(c.cfg.MMProjPath, model, mtmd.ContextParamsDefault())
	if err != nil {
		llama.Free(lctx)
		llama.ModelFree(model)
		llama.Close()
		return fmt.Errorf("memuat proyektor multimodal %s: %w", c.cfg.MMProjPath, err)
	}

	sp := llama.DefaultSamplerParams()
	sp.Temp = samplerTemp

	c.model = model
	c.lctx = lctx
	c.mctx = mctx
	c.vocab = llama.ModelGetVocab(model)
	c.sampler = llama.NewSampler(model, llama.DefaultSamplers, sp)
	c.template = llama.ModelChatTemplate(model, "")
	c.marker = mtmd.DefaultMarker()
	c.loaded = true

	logx.Info("model dimuat: %s (konteks %d token)", c.cfg.ModelPath, c.curCtx)
	return nil
}

// growContext menggandakan jendela konteks tanpa memuat ulang model. Model tetap
// di memori; hanya konteks (KV cache) yang dibuat ulang, sehingga jauh lebih
// murah daripada memuat ulang seluruh model.
func (c *Client) growContext() error {
	next := c.curCtx * 2
	if next > maxCtx {
		next = maxCtx
	}
	lctx, err := newContext(c.model, next)
	if err != nil {
		return err
	}
	llama.Free(c.lctx)
	c.lctx = lctx
	c.curCtx = next
	logx.Info("konteks diperbesar menjadi %d token", next)
	return nil
}

// newContext membuat konteks llama dengan ukuran jendela tertentu.
func newContext(model llama.Model, nCtx uint32) (llama.Context, error) {
	p := llama.ContextDefaultParams()
	p.NCtx = nCtx
	p.NBatch = nBatch
	lctx, err := llama.InitFromModel(model, p)
	if err != nil {
		return 0, fmt.Errorf("menyiapkan konteks %d token: %w", nCtx, err)
	}
	return lctx, nil
}

// Release melepaskan model dari memori. Dipanggil ketika tidak ada lagi yang
// perlu di-OCR, agar service tidak menahan memori selama menunggu siklus
// berikutnya. Model dimuat ulang sendiri ketika kembali dibutuhkan.
func (c *Client) Release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		return
	}
	llama.SamplerFree(c.sampler)
	mtmd.Free(c.mctx)
	llama.Free(c.lctx)
	llama.ModelFree(c.model)
	llama.Close()
	c.loaded = false
	logx.Info("model dilepas dari memori")
}

// Vision menjalankan OCR atas satu gambar PNG.
func (c *Client) Vision(ctx context.Context, prompt string, png []byte, p Params) (Result, error) {
	if len(png) == 0 {
		return Result{}, fmt.Errorf("gambar kosong")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLoaded(); err != nil {
		return Result{}, &LoadError{Err: err}
	}

	// Bersihkan sisa halaman sebelumnya. Tanpa ini konteks halaman lama ikut
	// terbawa dan model dapat melanjutkan teks dari halaman yang keliru.
	if mem, err := llama.GetMemory(c.lctx); err == nil {
		_ = llama.MemoryClear(mem, true)
	}
	llama.SamplerReset(c.sampler)

	messages := []llama.ChatMessage{
		llama.NewChatMessage("user", c.marker+prompt),
	}
	buf := make([]byte, chatBufferLen)
	n := llama.ChatApplyTemplate(c.template, messages, true, buf)
	if n <= 0 {
		return Result{}, fmt.Errorf("gagal menerapkan chat template")
	}

	chunks := mtmd.InputChunksInit()
	defer mtmd.InputChunksFree(chunks)

	// Gambar diserahkan langsung dari memori, tanpa berkas sementara.
	bw := mtmd.BitmapInitFromBuf(c.mctx, &png[0], uint64(len(png)), false)
	if bw.Bitmap == 0 {
		return Result{}, fmt.Errorf("gagal membaca gambar (%d byte)", len(png))
	}
	defer mtmd.BitmapFree(bw.Bitmap)

	input := mtmd.NewInputText(string(buf[:n]), true, true)
	if rc := mtmd.Tokenize(c.mctx, chunks, input, []mtmd.Bitmap{bw.Bitmap}); rc != 0 {
		return Result{}, fmt.Errorf("tokenisasi gagal (kode %d)", rc)
	}

	var pos llama.Pos
	if rc := mtmd.HelperEvalChunks(c.mctx, c.lctx, chunks, 0, 0, nBatch, true, &pos); rc != 0 {
		// Penyebab tersering: jendela konteks tidak cukup menampung token gambar.
		// Perbesar lalu ulangi sekali; ukuran baru dipakai untuk halaman
		// berikutnya sehingga kegagalan yang sama tidak berulang.
		if c.curCtx >= maxCtx {
			return Result{}, fmt.Errorf("evaluasi gambar gagal (kode %d) pada konteks %d — "+
				"halaman terlalu besar; kecilkan DPI atau pastikan AUTO_CROP aktif", rc, c.curCtx)
		}
		if err := c.growContext(); err != nil {
			return Result{}, err
		}
		pos = 0
		if rc := mtmd.HelperEvalChunks(c.mctx, c.lctx, chunks, 0, 0, nBatch, true, &pos); rc != 0 {
			return Result{}, fmt.Errorf("evaluasi gambar gagal (kode %d) pada konteks %d — "+
				"kecilkan DPI atau pastikan AUTO_CROP aktif", rc, c.curCtx)
		}
	}

	maxTokens := p.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 2048
	}

	var sb strings.Builder
	piece := make([]byte, 256)
	truncated := true // menjadi false bila model berhenti sendiri (EOG)

	for i := 0; i < maxTokens; i++ {
		if err := ctx.Err(); err != nil {
			return Result{Text: sb.String(), Truncated: true}, err
		}
		token := llama.SamplerSample(c.sampler, c.lctx, -1)
		if llama.VocabIsEOG(c.vocab, token) {
			truncated = false
			break
		}
		if l := llama.TokenToPiece(c.vocab, token, piece, 0, true); l > 0 {
			sb.Write(piece[:l])
		}

		batch := llama.BatchGetOne([]llama.Token{token})
		batch.Pos = &pos
		if _, err := llama.Decode(c.lctx, batch); err != nil {
			return Result{Text: sb.String(), Truncated: true}, fmt.Errorf("dekode token: %w", err)
		}
		pos++
	}

	return Result{Text: sb.String(), Truncated: truncated}, nil
}
