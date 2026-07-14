package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f.bin")
	writeFile(t, target, "payload")

	meta := FileMetadata{
		Size:      7,
		ModTime:   1234567890,
		MD5:       "abc123",
		ChunkSize: 64 << 20,
		ChunkMD5:  []string{"c1", "c2"},
	}
	if err := writeSidecar(target, meta); err != nil {
		t.Fatal(err)
	}
	got, err := readSidecar(target)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != meta.Size || got.ModTime != meta.ModTime || got.MD5 != meta.MD5 ||
		got.ChunkSize != meta.ChunkSize || len(got.ChunkMD5) != 2 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// sidecar sits next to the file, with the agreed name
	if _, err := os.Stat(target + ".dup-detector-metadata.json"); err != nil {
		t.Errorf("sidecar not at expected path: %v", err)
	}
}

func TestHashPrefersFreshSidecar(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.bin")
	writeFile(t, p, "real content")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// A sidecar with a SENTINEL md5 that is deliberately NOT the real content's
	// md5. If Hash trusts the sidecar it returns the sentinel; if it reads the
	// file it returns the real md5. That distinguishes the two.
	sentinel := "0000000000000000000000000000dead"
	if err := writeSidecar(p, FileMetadata{Size: info.Size(), ModTime: info.ModTime().Unix(), MD5: sentinel}); err != nil {
		t.Fatal(err)
	}
	var c *HashCache // nil = uncached path

	fresh := ScannedFile{Path: p, Size: info.Size(), ModTime: info.ModTime().Unix()}
	got, err := c.Hash(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if got != sentinel {
		t.Fatalf("Hash must trust a fresh sidecar; got %q want sentinel %q", got, sentinel)
	}

	// Stale sidecar (size no longer matches) must be ignored → real md5.
	stale := ScannedFile{Path: p, Size: info.Size() + 1, ModTime: info.ModTime().Unix()}
	got2, err := c.Hash(stale)
	if err != nil {
		t.Fatal(err)
	}
	real, _ := md5File(p)
	if got2 != real {
		t.Fatalf("stale sidecar must be ignored; got %q want real %q", got2, real)
	}
}

func TestScanSkipsSidecarFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "data.bin")
	writeFile(t, p, "hello")
	if err := writeSidecar(p, FileMetadata{Size: 5, ModTime: 1, MD5: "x"}); err != nil {
		t.Fatal(err)
	}
	files := scanDir(t, dir)
	for _, f := range files {
		if filepath.Base(f.Path) == "data.bin.dup-detector-metadata.json" {
			t.Fatalf("sidecar file must be excluded from the scan, got %s", f.Path)
		}
	}
	if len(files) != 1 {
		t.Fatalf("expected only data.bin, got %d files", len(files))
	}
}
