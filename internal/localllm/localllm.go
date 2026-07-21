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
	"os"
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

	// fallbackChatTemplate dipakai bila model tidak menyertakan chat template
	// sendiri. "chatml" adalah format yang selalu dikenali llama.cpp; ini
	// mengikuti contoh resmi yzma.
	fallbackChatTemplate = "chatml"
)

// Config hanya memuat hal yang memang berbeda di tiap mesin.
type Config struct {
	// ChatTemplate menimpa deteksi otomatis (kosong = pakai milik model).
	ChatTemplate string

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

// applyChatTemplate menerapkan chat template dengan buffer yang aman.
//
// llama.cpp mengembalikan UKURAN YANG DIBUTUHKAN, bukan jumlah byte yang
// tertulis: bila lebih besar dari buffer, isi buffer TIDAK lengkap dan
// pemanggil wajib memperbesar lalu mengulang. Tanpa penanganan ini,
// `buf[:n]` panic "slice out of range" — dan itu pasti terjadi pada jalur
// teks, karena satu halaman peraturan beserta prompt sistem dengan mudah
// melewati ukuran buffer awal.
func applyChatTemplate(template string, messages []llama.ChatMessage) (string, error) {
	// Coba template milik model dulu; bila ditolak llama.cpp (nilai balik
	// negatif), jatuh ke "chatml" yang selalu dikenali. Model GGUF tidak
	// selalu menyertakan chat template — atau menyertakannya dalam bentuk
	// yang belum dikenali versi llama.cpp yang terpasang — dan tanpa
	// cadangan ini seluruh dokumen gagal pada langkah pertama.
	candidates := []string{template}
	if template != fallbackChatTemplate {
		candidates = append(candidates, fallbackChatTemplate)
	}

	var lastErr error
	for _, tmpl := range candidates {
		if strings.TrimSpace(tmpl) == "" {
			continue
		}
		out, err := applyOneTemplate(tmpl, messages)
		if err == nil {
			return out, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("tidak ada chat template yang dapat dipakai")
	}
	return "", fmt.Errorf("menerapkan chat template (dicoba: %v): %w",
		nonEmpty(candidates), lastErr)
}

// applyOneTemplate menerapkan SATU template, memperbesar buffer bila perlu.
func applyOneTemplate(template string, messages []llama.ChatMessage) (string, error) {
	size := chatBufferLen
	for attempt := 0; attempt < 4; attempt++ {
		buf := make([]byte, size)
		n := llama.ChatApplyTemplate(template, messages, true, buf)
		switch {
		case n < 0:
			return "", fmt.Errorf("template %q tidak dikenali llama.cpp", template)
		case n == 0:
			return "", fmt.Errorf("template %q menghasilkan keluaran kosong", template)
		case int(n) <= len(buf):
			return string(buf[:n]), nil
		default:
			// llama.cpp memberi tahu ukuran yang diperlukan; perbesar & ulang.
			size = int(n) + 1024
		}
	}
	return "", fmt.Errorf("template %q tidak muat setelah beberapa kali perbesaran buffer", template)
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return []string{"(kosong)"}
	}
	return out
}

// resolveChatTemplate mengambil chat template bawaan model, dengan cadangan
// "chatml" bila model tidak menyertakannya — mengikuti contoh resmi yzma.
func resolveChatTemplate(model llama.Model, override, what string) string {
	if o := strings.TrimSpace(override); o != "" {
		logx.Info("%s: chat template dipaksa ke %q lewat CHAT_TEMPLATE", what, o)
		return o
	}
	if tmpl := llama.ModelChatTemplate(model, ""); strings.TrimSpace(tmpl) != "" {
		return tmpl
	}
	logx.Warn("%s tidak menyertakan chat template — memakai %q. "+
		"Bila mutu jawaban buruk, setel CHAT_TEMPLATE di .env (mis. gemma, llama3)",
		what, fallbackChatTemplate)
	return fallbackChatTemplate
}

// sizeHint melaporkan ukuran berkas model agar kebutuhan memori terlihat
// sebelum pemuatan dimulai. Mengembalikan string kosong bila tidak terbaca.
func sizeHint(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf(" (%.1f GB)", float64(fi.Size())/(1<<30))
}

// loadLibs memuat pustaka bersama (llama + mtmd) TEPAT SEKALI seumur proses.
//
// Dipakai bersama oleh klien visi DAN klien teks. Penting bahwa keduanya
// melewati fungsi yang sama: bila masing-masing punya blok sync.Once sendiri
// dengan isi berbeda, klien mana pun yang kebetulan memuat lebih dulu
// menentukan pustaka apa saja yang tersedia — dan model yang satunya gagal
// dengan galat yang menyesatkan.
func loadLibs(libPath string, verbose bool) error {
	libsOnce.Do(func() {
		if err := llama.Load(libPath); err != nil {
			libsErr = fmt.Errorf("memuat pustaka llama: %w", err)
			return
		}
		if err := mtmd.Load(libPath); err != nil {
			libsErr = fmt.Errorf("memuat pustaka mtmd: %w", err)
			return
		}
		if !verbose {
			llama.LogSet(llama.LogSilent())
			mtmd.LogSet(llama.LogSilent())
		}
	})
	return libsErr
}

// ensureLoaded memuat model bila belum termuat. Pemanggil sudah memegang kunci.
func (c *Client) ensureLoaded() error {
	if c.loaded {
		return nil
	}

	if err := loadLibs(c.cfg.LibPath, c.cfg.Verbose); err != nil {
		return err
	}

	llama.Init()

	logx.Info("memuat model OCR: %s%s", c.cfg.ModelPath, sizeHint(c.cfg.ModelPath))

	model, err := llama.ModelLoadFromFile(c.cfg.ModelPath, llama.ModelDefaultParams())
	if err != nil {
		return fmt.Errorf("memuat model %s: %w", c.cfg.ModelPath, err)
	}
	if model == 0 {
		return fmt.Errorf("gagal memuat model dari %s", c.cfg.ModelPath)
	}

	if c.curCtx == 0 {
		c.curCtx = initialCtx
	}
	lctx, err := newContext(model, c.curCtx)
	if err != nil {
		llama.ModelFree(model)
		return err
	}

	mctx, err := mtmd.InitFromFile(c.cfg.MMProjPath, model, mtmd.ContextParamsDefault())
	if err != nil {
		llama.Free(lctx)
		llama.ModelFree(model)
		return fmt.Errorf("memuat proyektor multimodal %s: %w", c.cfg.MMProjPath, err)
	}

	sp := llama.DefaultSamplerParams()
	sp.Temp = samplerTemp

	c.model = model
	c.lctx = lctx
	c.mctx = mctx
	c.vocab = llama.ModelGetVocab(model)
	c.sampler = llama.NewSampler(model, llama.DefaultSamplers, sp)
	c.template = resolveChatTemplate(model, c.cfg.ChatTemplate, "model OCR")
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
// Warmup memuat model SEKARANG, bukan menunggu permintaan pertama.
//
// Dipanggil sekali saat aplikasi mulai supaya seluruh waktu tunggu pemuatan
// terjadi di awal, ketika pemakai memang sedang menunggu — bukan mendadak di
// tengah pekerjaan, yang membuat proses tampak berhenti tanpa sebab.
// Sekaligus memastikan berkas model memang dapat dipakai sebelum satu
// dokumen pun tersentuh.
func (c *Client) Warmup() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLoaded(); err != nil {
		return &LoadError{Err: err}
	}
	return nil
}

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
	c.loaded = false
	logx.Info("model OCR dilepas dari memori")
}

// Shutdown membebaskan backend llama.cpp yang DIPAKAI BERSAMA oleh semua klien
// (visi maupun teks). Hanya boleh dipanggil saat proses benar-benar berakhir,
// setelah semua Client/TextClient di-Release.
//
// PENTING: llama.Close() sengaja TIDAK dipanggil di Release. Sejak ada dua
// model (OCR + thinking) yang hidup berdampingan, membebaskan backend saat
// salah satu dilepas akan merusak model yang satunya — padahal yang memakan
// memori besar adalah bobot model, yang sudah dibebaskan ModelFree/Free.
func Shutdown() {
	llama.Close()
}

// Vision menjalankan OCR atas satu gambar PNG.
func (c *Client) Vision(ctx context.Context, prompt string, png []byte, p Params) (Result, error) {
	if len(png) == 0 {
		return Result{}, fmt.Errorf("gambar kosong")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Sama seperti pada jalur teks: umumkan pemuatan SEBELUM dimulai,
	// supaya baris kemajuan tidak membeku tanpa penjelasan.
	if !c.loaded && p.OnStage != nil {
		p.OnStage("memuat model OCR" + sizeHint(c.cfg.ModelPath))
	}
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
	tmpl, err := applyChatTemplate(c.template, messages)
	if err != nil {
		return Result{}, err
	}

	chunks := mtmd.InputChunksInit()
	defer mtmd.InputChunksFree(chunks)

	// Gambar diserahkan langsung dari memori, tanpa berkas sementara.
	bw := mtmd.BitmapInitFromBuf(c.mctx, &png[0], uint64(len(png)), false)
	if bw.Bitmap == 0 {
		return Result{}, fmt.Errorf("gagal membaca gambar (%d byte)", len(png))
	}
	defer mtmd.BitmapFree(bw.Bitmap)

	input := mtmd.NewInputText(tmpl, true, true)
	if rc := mtmd.Tokenize(c.mctx, chunks, input, []mtmd.Bitmap{bw.Bitmap}); rc != 0 {
		return Result{}, fmt.Errorf("tokenisasi gagal (kode %d)", rc)
	}

	if p.OnStage != nil {
		p.OnStage("menyandikan gambar")
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

	if p.OnStage != nil {
		p.OnStage("menghasilkan teks")
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
		if l := llama.TokenToPiece(c.vocab, token, piece, 0, false); l > 0 {
			sb.Write(piece[:l])
		}
		if p.OnToken != nil {
			p.OnToken(i + 1)
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
