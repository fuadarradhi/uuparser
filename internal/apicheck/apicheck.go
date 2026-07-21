// Package apicheck memastikan permukaan API localllm yang DIPAKAI main.go dan
// internal/pipeline benar-benar ada pada kode SUNGGUHAN (yang dibangun
// terhadap yzma asli), bukan hanya pada tiruan yang dipakai saat pengujian.
//
// Alasannya konkret: pernah terjadi sebuah method hanya ditambahkan ke tiruan,
// sementara kode aslinya tidak — seluruh pengujian lulus, tetapi pemakai
// mendapat galat kompilasi "has no field or method". Berkas ini membuat
// penyimpangan seperti itu gagal saat dibangun, bukan saat dipakai.
package apicheck

import (
	"context"

	"github.com/fuadarradhi/uuparser/internal/localllm"
)

// visionAPI dan textAPI menyalin persis pemanggilan yang dilakukan
// main.go/pipeline. Menghapus atau mengubah tanda tangan salah satunya
// membuat paket ini gagal dibangun.
type visionAPI interface {
	Warmup() error
	Release()
	Vision(ctx context.Context, prompt string, png []byte, p localllm.Params) (localllm.Result, error)
}

type textAPI interface {
	Warmup() error
	Release()
	Generate(ctx context.Context, systemPrompt, userText string) (localllm.Result, error)
	GenerateWith(ctx context.Context, systemPrompt, userText string, p localllm.TextParams) (localllm.Result, error)
}

var (
	_ visionAPI = (*localllm.Client)(nil)
	_ textAPI   = (*localllm.TextClient)(nil)
)
