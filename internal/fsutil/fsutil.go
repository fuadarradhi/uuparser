// Package fsutil berisi bantuan berkas yang dipakai semua tahap.
//
// Penulisan atomik penting justru KARENA deteksi "sudah selesai" pada aplikasi ini
// berbasis keberadaan berkas: bila service mati saat menulis pageN.txt, berkas
// separuh akan dikira selesai dan kerusakan itu permanen. Menulis ke berkas
// sementara lalu rename membuat berkas hanya muncul ketika isinya sudah utuh.
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic menulis data ke path secara atomik (tulis .tmp lalu rename).
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // tak berpengaruh bila rename berhasil

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Sync agar isi benar-benar sampai ke disk sebelum rename.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Exists melaporkan apakah path ada.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
