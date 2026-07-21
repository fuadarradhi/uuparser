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

	// textMaxTokensCap adalah batas mutlak keluaran. Batas sesungguhnya
	// dihitung dari panjang masukan (lihat Generate): perbaikan halaman
	// mengembalikan teks SEUKURAN masukannya, jadi keluaran yang jauh lebih
	// panjang berarti model mengoceh. Membiarkannya berlari sampai 4096 token
	// di CPU bisa memakan belasan menit tanpa hasil berguna.
	textMaxTokensCap = 4096
)

// TextConfig hanya memuat hal yang berbeda antar mesin.
type TextConfig struct {
	// ChatTemplate menimpa deteksi otomatis (kosong = pakai milik model).
	ChatTemplate string

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

	// Diumumkan SEBELUM pemuatan, bukan sesudah: memuat model beberapa GB
	// bisa memakan waktu lama, dan bila kehabisan memori prosesnya dihentikan
	// paksa oleh sistem tanpa pesan apa pun. Tanpa baris ini, satu-satunya
	// petunjuk yang tersisa adalah konsol yang mendadak berhenti.
	logx.Info("memuat model teks: %s%s", t.cfg.ModelPath, sizeHint(t.cfg.ModelPath))

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
	t.template = resolveChatTemplate(model, t.cfg.ChatTemplate, "model teks")
	t.loaded = true

	logx.Info("model teks dimuat: %s (konteks %d token)", t.cfg.ModelPath, textCtx)
	return nil
}

// Release melepaskan model teks dari memori. Backend bersama TIDAK disentuh —
// lihat catatan di localllm.go/Shutdown.
// Warmup memuat model teks SEKARANG — lihat alasan di Client.Warmup.
func (t *TextClient) Warmup() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoaded(); err != nil {
		return &LoadError{Err: err}
	}
	return nil
}

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
// TextParams membawa pelaporan kemajuan. Tanpa ini konsol diam sepanjang
// model teks bekerja — pada CPU itu bisa beberapa menit per halaman, dan
// diamnya konsol mudah disalahartikan sebagai proses yang menggantung.
type TextParams struct {
	OnStage func(stage string)
	OnToken func(n int)
}

// Generate menjalankan satu giliran percakapan tanpa pelaporan kemajuan.
func (t *TextClient) Generate(ctx context.Context, systemPrompt, userText string) (Result, error) {
	return t.GenerateWith(ctx, systemPrompt, userText, TextParams{})
}

func (t *TextClient) GenerateWith(ctx context.Context, systemPrompt, userText string, p TextParams) (Result, error) {
	if strings.TrimSpace(userText) == "" {
		return Result{}, fmt.Errorf("teks masukan kosong")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Pemuatan model beberapa GB memakan waktu lama. Umumkan LEBIH DULU
	// lewat pelapor kemajuan: tanpa ini baris kemajuan di konsol membeku
	// tanpa penjelasan dan proses yang sehat tampak menggantung.
	if !t.loaded && p.OnStage != nil {
		p.OnStage("memuat model teks" + sizeHint(t.cfg.ModelPath))
	}
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

	// Batas keluaran diturunkan dari panjang masukan: hasil perbaikan
	// seharusnya seukuran aslinya. Kelonggaran 50% memberi ruang bila model
	// merapikan penomoran, tetapi tetap menghentikan model yang mengoceh
	// jauh lebih cepat daripada batas mutlak.
	maxTokens := len(tokens)*3/2 + 256
	if maxTokens > textMaxTokensCap {
		maxTokens = textMaxTokensCap
	}

	if p.OnStage != nil {
		p.OnStage("model teks membaca")
	}

	var sb strings.Builder
	piece := make([]byte, 256)
	truncated := true

	// Posisi token TIDAK diatur manual: BatchGetOne mendokumentasikan bahwa
	// posisi dilacak otomatis oleh Decode berdasarkan keadaan KV cache
	// (yang sudah dibersihkan di atas). Pola berikut mengikuti contoh resmi
	// yzma untuk model teks.
	batch := llama.BatchGetOne(tokens)
	for i := 0; i < maxTokens; i++ {
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
		if p.OnToken != nil {
			p.OnToken(i + 1)
		}
	}

	return Result{Text: sb.String(), Truncated: truncated}, nil
}
