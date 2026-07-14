package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trashFile moves a file into the freedesktop trash of its own filesystem and
// records a .trashinfo. These tests are hermetic: they don't assume WHERE the
// trash lands (that depends on the mount layout), only that the file left its
// original spot, its bytes are preserved, and a matching info record exists.

func TestTrashFileMovesFileAndWritesInfo(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "sub", "f.txt")
	writeFile(t, orig, "trash me")

	dest, err := trashFile(orig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orig); !os.IsNotExist(err) {
		t.Errorf("original should be gone, stat err = %v", err)
	}
	b, err := os.ReadFile(dest)
	if err != nil || string(b) != "trash me" {
		t.Fatalf("dest content wrong: err=%v content=%q", err, b)
	}
	// dest = <trash>/files/<name>; info sits at <trash>/info/<name>.trashinfo
	trashDir := filepath.Dir(filepath.Dir(dest))
	info := filepath.Join(trashDir, "info", filepath.Base(dest)+".trashinfo")
	ib, err := os.ReadFile(info)
	if err != nil {
		t.Fatalf("missing trashinfo: %v", err)
	}
	if !strings.Contains(string(ib), "[Trash Info]") ||
		!strings.Contains(string(ib), "Path=") ||
		!strings.Contains(string(ib), "DeletionDate=") {
		t.Errorf("malformed trashinfo:\n%s", ib)
	}
}

func TestTrashFileCollisionKeepsBoth(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a", "same.txt")
	b := filepath.Join(dir, "b", "same.txt")
	writeFile(t, a, "AAA")
	writeFile(t, b, "BBB")

	d1, err := trashFile(a)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := trashFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Fatalf("colliding basenames must get distinct destinations, both = %s", d1)
	}
	c1, _ := os.ReadFile(d1)
	c2, _ := os.ReadFile(d2)
	if string(c1) != "AAA" || string(c2) != "BBB" {
		t.Errorf("contents lost/swapped: %q / %q", c1, c2)
	}
}

func TestHeadlessTrashMovesInsteadOfDelete(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "keep.txt"), filepath.Join(dir, "dup.txt")}
	for _, p := range paths {
		writeFile(t, p, "dup")
	}
	files := []ScannedFile{{Path: paths[0], Size: 3}, {Path: paths[1], Size: 3}}
	groups := []DupGroup{{Size: 3, Files: files}}

	deleted, err := HeadlessDelete(nil, nil, groups, func(string) []ScannedFile { return nil }, &Config{Trash: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 {
		t.Fatalf("want 1 disposed, got %d", len(deleted))
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Errorf("kept file should still exist: %v", err)
	}
	if _, err := os.Stat(paths[1]); !os.IsNotExist(err) {
		t.Errorf("dup should have been moved to trash, stat err = %v", err)
	}
}
