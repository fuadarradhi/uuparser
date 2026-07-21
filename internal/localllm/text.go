package localllm

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/fuadarradhi/uuparser/internal/logx"
	"github.com/hybridgroup/yzma/pkg/llama"
)

// text.go menjalankan model TEKS (bukan visi) di dalam proses yang sama:
// dipakai untuk membaca hasil OCR halaman 1 (menentukan ini peraturan atau
// bukan + menarik metadata) dan untuk memperbaiki salah ketik/struktur tiap
// halaman.
//
// Berbagi pustaka bersama (libsOnce) dengan klien visi, tapi memegang model,
// konteks, dan sampler SENDIRI — keduanya hidup berdampingan sepanjang worker
// masih punya pekerjaan, lalu dilepas bersamaan saat antrian habis (tidak ada
// bongkar-pasang model per halaman).

const (
	// textCtx: satu halaman peraturan A4 padat ± 1200-1500 token; prompt
	// perbaikan mengirim halaman itu MASUK dan mengharap halaman itu KELUAR
	// lagi, jadi jendela harus memuat keduanya sekaligus plus prompt sistem.
	// 8192 memberi ruang aman tanpa memakan KV cache besar pada model 2B.
	textCtx = 8192

	// textMaxTokens membatasi keluaran. Perbaikan halaman mengembalikan teks
	// seukuran masukannya, jadi batas ini harus lebih besar dari satu halaman.
	textMaxTokens = 4096
)

// TextConfig hanya memuat hal yang berbeda antar mesin.
type TextConfig struct {
	ModelPath string // berkas model GGUF (model teks/thinking)
	LibPath   string // folder berisi libllama (kosong = jalur sistem)
	Verbose   bool
}

// TextClient memegang model teks yang sudah dimuat.
type TextClient struct {
	cfg TextConfig

	mu     sync.Mutex
	loaded bool

	model    llama.Model
	lctx     llama.Context
	vocab    llama.Vocab
	sampler  llama.Sampler
	template string
}

func NewText(cfg TextConfig) (*TextClient, error) {
	if strings.TrimSpace(cfg.ModelPath) == "" {
		return nil, fmt.Errorf("jalur model teks kosong")
	}
	return &TextClient{cfg: cfg}, nil
}

func (t *TextClient) ensureLoaded() error {
	if t.loaded {
		return nil
	}

	if err := loadLibs(t.cfg.LibPath, t.cfg.Verbose); err != nil {
		return err
	}

	llama.Init()

	model, err := llama.ModelLoadFromFile(t.cfg.ModelPath, llama.ModelDefaultParams())
	if err != nil {
		return fmt.Errorf("memuat model teks %s: %w", t.cfg.ModelPath, err)
	}
	if model == 0 {
		return fmt.Errorf("gagal memuat model teks dari %s", t.cfg.ModelPath)
	}

	p := llama.ContextDefaultParams()
	p.NCtx = textCtx
	p.NBatch = nBatch
	lctx, err := llama.InitFromModel(model, p)
	if err != nil {
		llama.ModelFree(model)
		return fmt.Errorf("menyiapkan konteks model teks: %w", err)
	}

	sp := llama.DefaultSamplerParams()
	sp.Temp = samplerTemp // 0 = greedy: hasil dapat diulang persis

	t.model = model
	t.lctx = lctx
	t.vocab = llama.ModelGetVocab(model)
	t.sampler = llama.NewSampler(model, llama.DefaultSamplers, sp)
	t.template = llama.ModelChatTemplate(model, "")
	t.loaded = true

	logx.Info("model teks dimuat: %s (konteks %d token)", t.cfg.ModelPath, textCtx)
	return nil
}

// Release melepaskan model teks dari memori. Backend bersama TIDAK disentuh —
// lihat catatan di localllm.go/Shutdown.
func (t *TextClient) Release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.loaded {
		return
	}
	llama.SamplerFree(t.sampler)
	llama.Free(t.lctx)
	llama.ModelFree(t.model)
	t.loaded = false
	logx.Info("model teks dilepas dari memori")
}

// Generate menjalankan satu giliran percakapan: systemPrompt sebagai peran
// "system", userText sebagai "user", lalu mengembalikan jawaban model.
//
// Konteks dibersihkan tiap panggilan sehingga halaman/dokumen sebelumnya tidak
// bocor ke hasil berikutnya — alasan yang sama seperti pada jalur visi.
func (t *TextClient) Generate(ctx context.Context, systemPrompt, userText string) (Result, error) {
	if strings.TrimSpace(userText) == "" {
		return Result{}, fmt.Errorf("teks masukan kosong")
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoaded(); err != nil {
		return Result{}, &LoadError{Err: err}
	}

	if mem, err := llama.GetMemory(t.lctx); err == nil {
		_ = llama.MemoryClear(mem, true)
	}
	llama.SamplerReset(t.sampler)

	messages := []llama.ChatMessage{
		llama.NewChatMessage("system", systemPrompt),
		llama.NewChatMessage("user", userText),
	}
	tmpl, err := applyChatTemplate(t.template, messages)
	if err != nil {
		return Result{}, err
	}

	// Tokenize mengembalikan []Token saja (tanpa galat) pada yzma v1.19.
	// addSpecial=true karena tiap panggilan adalah percakapan baru: konteks
	// sudah dibersihkan di atas, jadi token BOS memang harus ikut.
	tokens := llama.Tokenize(t.vocab, tmpl, true, true)
	if len(tokens) == 0 {
		return Result{}, fmt.Errorf("tokenisasi menghasilkan nol token")
	}
	if len(tokens) >= textCtx-64 {
		return Result{}, fmt.Errorf("masukan %d token melebihi jendela konteks %d — "+
			"halaman terlalu panjang untuk model teks", len(tokens), textCtx)
	}

	var sb strings.Builder
	piece := make([]byte, 256)
	truncated := true

	// Posisi token TIDAK diatur manual: BatchGetOne mendokumentasikan bahwa
	// posisi dilacak otomatis oleh Decode berdasarkan keadaan KV cache
	// (yang sudah dibersihkan di atas). Pola berikut mengikuti contoh resmi
	// yzma untuk model teks.
	batch := llama.BatchGetOne(tokens)
	for i := 0; i < textMaxTokens; i++ {
		if err := ctx.Err(); err != nil {
			return Result{Text: sb.String(), Truncated: true}, err
		}
		if _, err := llama.Decode(t.lctx, batch); err != nil {
			return Result{Text: sb.String(), Truncated: true}, fmt.Errorf("dekode: %w", err)
		}
		token := llama.SamplerSample(t.sampler, t.lctx, -1)
		if llama.VocabIsEOG(t.vocab, token) {
			truncated = false
			break
		}
		// special=false: token kontrol tidak dirender menjadi teks biasa
		if l := llama.TokenToPiece(t.vocab, token, piece, 0, false); l > 0 {
			sb.Write(piece[:l])
		}
		batch = llama.BatchGetOne([]llama.Token{token})
	}

	return Result{Text: sb.String(), Truncated: truncated}, nil
}
