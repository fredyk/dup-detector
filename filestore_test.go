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
