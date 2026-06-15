package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pinMtime sets identical mtimes so the tree hash groups the dirs.
func pinMtime(t *testing.T, paths ...string) {
	t.Helper()
	mt := time.Unix(1_000_000_000, 0)
	for _, p := range paths {
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTreeDupGuardWithMinSize: A and B share two identical large files (so they
// pass the minFileCount=2 tree rule), but A also holds a small file (below
// --min-size) that B lacks — the trees are NOT identical. The size filter hides
// the small file from the store, so without the soundness guard A and B would be
// wrongly reported as a tree dup.
func TestTreeDupGuardWithMinSize(t *testing.T) {
	root := t.TempDir()
	big1 := strings.Repeat("X", 2000)
	big2 := strings.Repeat("Z", 3000)
	for _, d := range []string{"A", "B"} {
		writeFile(t, filepath.Join(root, d, "big1.bin"), big1)
		writeFile(t, filepath.Join(root, d, "big2.bin"), big2)
	}
	writeFile(t, filepath.Join(root, "A", "note.txt"), "secret") // 6 B, only in A
	pinMtime(t,
		filepath.Join(root, "A/big1.bin"), filepath.Join(root, "A/big2.bin"),
		filepath.Join(root, "B/big1.bin"), filepath.Join(root, "B/big2.bin"))

	cfg := &Config{Recursive: true, MinSize: 1000}
	fs := mustStore(t, root, cfg)
	defer fs.Close()

	pairs, err := FindTreeDupsByHashStore(fs, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 0 {
		t.Fatalf("guard failed: A and B differ in a sub-min-size file but were reported as a tree dup: %+v", pairs)
	}
}

// TestTreeDupNoGuardWhenComplete: same two large files in both dirs, no hidden
// small files — the guard must NOT suppress a genuine tree dup.
func TestTreeDupNoGuardWhenComplete(t *testing.T) {
	root := t.TempDir()
	big1 := strings.Repeat("Y", 2000)
	big2 := strings.Repeat("W", 3000)
	for _, d := range []string{"A", "B"} {
		writeFile(t, filepath.Join(root, d, "big1.bin"), big1)
		writeFile(t, filepath.Join(root, d, "big2.bin"), big2)
	}
	pinMtime(t,
		filepath.Join(root, "A/big1.bin"), filepath.Join(root, "A/big2.bin"),
		filepath.Join(root, "B/big1.bin"), filepath.Join(root, "B/big2.bin"))

	cfg := &Config{Recursive: true, MinSize: 1000}
	fs := mustStore(t, root, cfg)
	defer fs.Close()

	pairs, err := FindTreeDupsByHashStore(fs, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 {
		t.Fatalf("expected 1 tree dup (both dirs complete & identical), got %d", len(pairs))
	}
}

// mustStore scans root/A and root/B into a fresh store with the given cfg.
func mustStore(t *testing.T, root string, cfg *Config) *FileStore {
	t.Helper()
	fs, err := NewFileStore(filepath.Join(t.TempDir(), "scan.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ScanToStore(fs, filepath.Join(root, "A"), cfg, nil, nil, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := ScanToStore(fs, filepath.Join(root, "B"), cfg, nil, nil, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := fs.Finalize(); err != nil {
		t.Fatal(err)
	}
	return fs
}
