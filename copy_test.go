package main

import (
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// chunkHashes computes the per-chunk md5 hex list for content, matching what
// copyFileWithMetadata records, so tests can build expected verification data.
func chunkHashes(content string, chunk int) []string {
	var out []string
	b := []byte(content)
	for i := 0; i < len(b); i += chunk {
		end := i + chunk
		if end > len(b) {
			end = len(b)
		}
		out = append(out, fmt.Sprintf("%x", md5.Sum(b[i:end])))
	}
	return out
}

func TestCopyFileWritesVerifiedSidecar(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	content := "0123456789abcdefghij" // 20 bytes; chunk 8 → 3 chunks (8,8,4)
	writeFile(t, src, content)
	mt := time.Unix(1600000000, 0)
	if err := os.Chtimes(src, mt, mt); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out", "dst.bin")

	meta, err := copyFileWithMetadata(src, dst, 8, false)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != content {
		t.Fatalf("copied content mismatch: %q", got)
	}
	sc, err := readSidecar(dst)
	if err != nil {
		t.Fatal(err)
	}
	realMD5, _ := md5File(src)
	if sc.MD5 != realMD5 {
		t.Errorf("sidecar global md5 %q != real %q", sc.MD5, realMD5)
	}
	if len(sc.ChunkMD5) != 3 {
		t.Errorf("expected 3 chunk hashes, got %d", len(sc.ChunkMD5))
	}
	if sc.ChunkSize != 8 || meta.Size != 20 {
		t.Errorf("chunkSize=%d size=%d", sc.ChunkSize, meta.Size)
	}
	di, _ := os.Stat(dst)
	if di.ModTime().Unix() != mt.Unix() || sc.ModTime != mt.Unix() {
		t.Errorf("mtime not preserved: dst=%v sidecar=%d src=%d", di.ModTime().Unix(), sc.ModTime, mt.Unix())
	}
}

func TestVerifyReadbackDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.bin")
	content := "AAAABBBBCC" // 10 bytes; chunk 4 → 3 chunks
	writeFile(t, p, content)
	good := chunkHashes(content, 4)

	if err := verifyReadback(p, 4, good, true); err != nil {
		t.Fatalf("valid file must verify (full): %v", err)
	}
	bad := append([]string{}, good...)
	bad[len(bad)-1] = "deadbeefdeadbeefdeadbeefdeadbeef"
	if err := verifyReadback(p, 4, bad, true); err == nil {
		t.Fatal("tampered last-chunk hash must fail full verification")
	}
	// tail mode only re-reads the LAST chunk, so a wrong FIRST-chunk hash passes.
	badFirst := append([]string{}, good...)
	badFirst[0] = "deadbeefdeadbeefdeadbeefdeadbeef"
	if err := verifyReadback(p, 4, badFirst, false); err != nil {
		t.Fatalf("tail mode must not check the first chunk: %v", err)
	}
}
