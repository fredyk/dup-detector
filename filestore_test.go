package main

import (
	"path/filepath"
	"testing"
)

func buildStore(t *testing.T, root string) *FileStore {
	t.Helper()
	fs, err := NewFileStore(filepath.Join(t.TempDir(), "scan.db"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	c := &Config{Recursive: true}
	if err := ScanToStore(fs, root, c, nil, nil, 0, nil); err != nil {
		t.Fatalf("ScanToStore: %v", err)
	}
	if err := fs.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	return fs
}

// TestCoverageAndSizeRejectsUncovered locks the #16 streaming coverage check:
// a dir with a file that has NO duplicate under the target dir must report
// covered=false. (The first cut of CoverageAndSize always returned true — the
// closure's early-false only broke iteration without recording non-coverage —
// which silently disabled tree-dup verification and could confirm non-duplicate
// trees for deletion. This test exercises the store-backed path the unit tests
// otherwise skip.)
func TestCoverageAndSizeRejectsUncovered(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "X/a"), "shared-content")
	writeFile(t, filepath.Join(root, "X/b"), "unique-to-X-content-longer")
	writeFile(t, filepath.Join(root, "Y/a"), "shared-content")
	fs := buildStore(t, root)
	defer fs.Close()

	xa := filepath.Join(root, "X", "a")
	ya := filepath.Join(root, "Y", "a")
	// dupIndex (as #15 builds it): shared group {X/a, Y/a}; X/b is unique → absent.
	index := map[string][]string{xa: {xa, ya}, ya: {xa, ya}}
	dirX := filepath.Join(root, "X")
	dirY := filepath.Join(root, "Y")

	// X is NOT fully covered by Y: X/b has no duplicate under Y.
	covered, _, err := fs.CoverageAndSize(dirX, dirY, index)
	if err != nil {
		t.Fatal(err)
	}
	if covered {
		t.Fatal("X must NOT be covered by Y (X/b has no duplicate under Y)")
	}

	// Y IS fully covered by X (Y/a's duplicate X/a lives under X).
	covered, total, err := fs.CoverageAndSize(dirY, dirX, index)
	if err != nil {
		t.Fatal(err)
	}
	if !covered {
		t.Fatal("Y must be fully covered by X")
	}
	if total <= 0 {
		t.Fatalf("expected positive total size for Y, got %d", total)
	}
}

func TestFileStoreMatchesSlice(t *testing.T) {
	root := buildFixture(t)
	files := scanDir(t, root)
	SortFilesByPath(files)

	fs := buildStore(t, root)
	defer fs.Close()

	// Count
	n, err := fs.Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != len(files) {
		t.Fatalf("store Count=%d, slice=%d", n, len(files))
	}

	// CollisionSizes == keys of GroupBySize
	bySize := GroupBySize(files)
	cs, err := fs.CollisionSizes()
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != len(bySize) {
		t.Fatalf("CollisionSizes=%d, GroupBySize=%d", len(cs), len(bySize))
	}
	for _, s := range cs {
		g, ok := bySize[s]
		if !ok {
			t.Fatalf("store reports collision size %d not in slice map", s)
		}
		fws, err := fs.FilesWithSize(s)
		if err != nil {
			t.Fatal(err)
		}
		if len(fws) != len(g) {
			t.Fatalf("FilesWithSize(%d)=%d, slice=%d", s, len(fws), len(g))
		}
	}
	// largest-first ordering
	for i := 1; i < len(cs); i++ {
		if cs[i-1] < cs[i] {
			t.Fatalf("CollisionSizes not descending: %v", cs)
		}
	}

	// FilesUnderDir == filesUnderDir
	dir := filepath.Join(root, "A", "tree")
	a, err := fs.FilesUnderDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := filesUnderDir(dir, files)
	if len(a) != len(b) {
		t.Fatalf("FilesUnderDir(%s)=%d, slice=%d", dir, len(a), len(b))
	}
	if len(a) != 2 {
		t.Fatalf("expected 2 files under A/tree, got %d", len(a))
	}
}
