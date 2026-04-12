package reflink

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func fileHash(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func TestAuto_SparseFile_TrailingHole(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "sparse-src")
	dst := filepath.Join(dir, "sparse-dst")

	// Create a sparse file: 4 KB of data followed by a ~4 MB trailing hole.
	// This is the exact pattern produced by Firecracker diff snapshots (vm.mem)
	// and broken by copy_file_range on ext4.
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 4096)
	for i := range data {
		data[i] = 0xAB
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(4 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := Auto(src, dst); err != nil {
		t.Fatalf("Auto: %v", err)
	}

	srcInfo, _ := os.Stat(src)
	dstInfo, _ := os.Stat(dst)
	if srcInfo.Size() != dstInfo.Size() {
		t.Fatalf("logical size mismatch: src=%d dst=%d", srcInfo.Size(), dstInfo.Size())
	}

	srcHash := fileHash(t, src)
	dstHash := fileHash(t, dst)
	if srcHash != dstHash {
		t.Fatalf("content hash mismatch:\n  src=%s\n  dst=%s", srcHash, dstHash)
	}
}

func TestAuto_SparseFile_DataInMiddle(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "mid-src")
	dst := filepath.Join(dir, "mid-dst")

	// Sparse file with hole → data → hole.
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 199)
	}
	if _, err := f.WriteAt(data, 512*1024); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(2 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := Auto(src, dst); err != nil {
		t.Fatalf("Auto: %v", err)
	}

	srcInfo, _ := os.Stat(src)
	dstInfo, _ := os.Stat(dst)
	if srcInfo.Size() != dstInfo.Size() {
		t.Fatalf("logical size mismatch: src=%d dst=%d", srcInfo.Size(), dstInfo.Size())
	}

	if fileHash(t, src) != fileHash(t, dst) {
		t.Fatal("content hash mismatch")
	}
}

func TestReflink_SparseFile(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "ref-src")
	dstPath := filepath.Join(dir, "ref-dst")

	// Sparse file with trailing hole.
	f, err := os.Create(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("hello"))
	f.Truncate(1 << 20)
	f.Close()

	src, _ := os.Open(srcPath)
	defer src.Close()
	dst, _ := os.Create(dstPath)
	defer dst.Close()

	if err := Reflink(dst, src, true); err != nil {
		t.Fatalf("Reflink: %v", err)
	}
	dst.Close()

	srcInfo, _ := os.Stat(srcPath)
	dstInfo, _ := os.Stat(dstPath)
	if srcInfo.Size() != dstInfo.Size() {
		t.Fatalf("logical size mismatch: src=%d dst=%d", srcInfo.Size(), dstInfo.Size())
	}

	if fileHash(t, srcPath) != fileHash(t, dstPath) {
		t.Fatal("content hash mismatch")
	}
}

func TestAuto_RegularFile_HashMatch(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "reg-src")
	dst := filepath.Join(dir, "reg-dst")

	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Auto(src, dst); err != nil {
		t.Fatalf("Auto: %v", err)
	}

	if fileHash(t, src) != fileHash(t, dst) {
		t.Fatal("content hash mismatch on regular file")
	}

	srcInfo, _ := os.Stat(src)
	dstInfo, _ := os.Stat(dst)
	if srcInfo.Size() != dstInfo.Size() {
		t.Fatalf("size mismatch: src=%d dst=%d", srcInfo.Size(), dstInfo.Size())
	}
}
