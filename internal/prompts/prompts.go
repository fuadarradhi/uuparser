// Package prompts memuat berkas prompt dari disk (prompts/*.md) supaya dapat
// disunting tanpa membangun ulang binari, sekaligus menghitung sidik jarinya.
//
// Sidik jari (hash) disimpan bersama tiap halaman di document_pages: bila
// kelak prompt diubah, Anda dapat menemukan halaman mana saja yang masih
// diproses memakai prompt versi lama, tanpa harus menebak.
package prompts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Set memuat seluruh prompt yang dipakai pipeline, dibaca sekali saat start.
type Set struct {
	Classify     string
	ClassifyHash string

	FixPage     string
	FixPageHash string
}

// Load membaca prompts/classify.md dan prompts/fix_page.md dari dir.
func Load(dir string) (Set, error) {
	var s Set
	var err error

	if s.Classify, s.ClassifyHash, err = readOne(filepath.Join(dir, "classify.md")); err != nil {
		return s, err
	}
	if s.FixPage, s.FixPageHash, err = readOne(filepath.Join(dir, "fix_page.md")); err != nil {
		return s, err
	}
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
