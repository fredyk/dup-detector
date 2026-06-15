package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func storeLookup(fs *FileStore) DirLookup {
	return func(dir string) []ScannedFile {
		f, _ := fs.FilesUnderDir(dir)
		return f
	}
}

func chtimesAll(t *testing.T, root string, rel ...string) {
	t.Helper()
	mt := time.Unix(1_000_000_000, 0)
	for _, p := range rel {
		if err := os.Chtimes(filepath.Join(root, p), mt, mt); err != nil {
			t.Fatal(err)
		}
	}
}

// #1: A and B have files with identical sizes AND mtimes, but one file's CONTENT
// differs. The fast mtime tree pass declares them identical; content verification
// must reject (this is the backup-rotation footgun: rsync -a preserves mtime).
func TestTreeContentVerifyRejectsMtimeCollision(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "A/f1"), "AAAA")
	writeFile(t, filepath.Join(root, "A/f2"), "BBBB")
	writeFile(t, filepath.Join(root, "B/f1"), "AAAA")
	writeFile(t, filepath.Join(root, "B/f2"), "XXXX") // same size+mtime, different bytes
	chtimesAll(t, root, "A/f1", "A/f2", "B/f1", "B/f2")

	cfg := &Config{Recursive: true}
	fs := mustStore(t, root, cfg)
	defer fs.Close()

	pairs, err := FindTreeDupsByHashStore(fs, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 {
		t.Fatalf("mtime pass should report 1 tree pair (size+mtime identical), got %d", len(pairs))
	}
	verified := VerifyTreePairsByContent(pairs, storeLookup(fs), nil)
	if len(verified) != 0 {
		t.Fatalf("content verify must REJECT trees that collide on size+mtime but differ in content, got %d", len(verified))
	}
}

// Counterpart: genuinely identical trees survive content verification (Verified=true).
func TestTreeContentVerifyKeepsIdentical(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "A/f1"), "AAAA")
	writeFile(t, filepath.Join(root, "A/f2"), "BBBB")
	writeFile(t, filepath.Join(root, "B/f1"), "AAAA")
	writeFile(t, filepath.Join(root, "B/f2"), "BBBB")
	chtimesAll(t, root, "A/f1", "A/f2", "B/f1", "B/f2")

	cfg := &Config{Recursive: true}
	fs := mustStore(t, root, cfg)
	defer fs.Close()

	pairs, _ := FindTreeDupsByHashStore(fs, cfg, nil)
	verified := VerifyTreePairsByContent(pairs, storeLookup(fs), nil)
	if len(verified) != 1 {
		t.Fatalf("content verify must KEEP genuinely identical trees, got %d", len(verified))
	}
	if !verified[0].Verified {
		t.Errorf("Verified flag should be set true after content verification")
	}
}
