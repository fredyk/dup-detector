package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Headless mode = the interactive "auto keep-first" policy, but with no prompts
// or stdin, for orchestrated/non-interactive runs. These lock its two contracts:
// (1) real mode deletes every copy but the first; (2) dry-run touches nothing on
// disk yet still reports what it WOULD delete.

func TestHeadlessDeleteRealDeletesAllButFirst(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "keep.txt"),
		filepath.Join(dir, "dup1.txt"),
		filepath.Join(dir, "dup2.txt"),
	}
	for _, p := range paths {
		writeFile(t, p, "same content")
	}
	files := make([]ScannedFile, len(paths))
	for i, p := range paths {
		files[i] = ScannedFile{Path: p, Size: 12}
	}
	groups := []DupGroup{{Size: 12, Files: files}}

	deleted, err := HeadlessDelete(nil, nil, groups, func(string) []ScannedFile { return nil }, &Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 {
		t.Fatalf("expected 2 deleted, got %d", len(deleted))
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Errorf("kept file [0] should still exist: %v", err)
	}
	for _, p := range paths[1:] {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("dup %s should be deleted from disk", p)
		}
	}
}

func TestHeadlessDeleteDryRunDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "keep.txt"),
		filepath.Join(dir, "dup1.txt"),
		filepath.Join(dir, "dup2.txt"),
	}
	for _, p := range paths {
		writeFile(t, p, "same content")
	}
	files := make([]ScannedFile, len(paths))
	for i, p := range paths {
		files[i] = ScannedFile{Path: p, Size: 12}
	}
	groups := []DupGroup{{Size: 12, Files: files}}

	deleted, err := HeadlessDelete(nil, nil, groups, func(string) []ScannedFile { return nil }, &Config{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 {
		t.Fatalf("dry-run should still report 2 would-delete, got %d", len(deleted))
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("dry-run must NOT delete %s: %v", p, err)
		}
	}
}

func TestHeadlessDeleteDryRunTreeKeepsDisk(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "w1/x"), "xx")
	writeFile(t, filepath.Join(dir, "w1/y"), "yy")
	writeFile(t, filepath.Join(dir, "w2/x"), "xx")
	writeFile(t, filepath.Join(dir, "w2/y"), "yy")

	lookup := func(d string) []ScannedFile {
		var out []ScannedFile
		_ = filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				out = append(out, ScannedFile{Path: p, Size: 2})
			}
			return nil
		})
		return out
	}
	pair := TreeDupPair{
		DirA:      filepath.Join(dir, "w1"),
		DirB:      filepath.Join(dir, "w2"),
		FileCount: 2,
		TotalSize: 4,
	}

	deleted, err := HeadlessDelete([]TreeDupPair{pair}, nil, nil, lookup, &Config{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 {
		t.Fatalf("expected 2 would-delete files in tree, got %d", len(deleted))
	}
	for _, sub := range []string{"w1/x", "w1/y", "w2/x", "w2/y"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
			t.Errorf("dry-run tree must NOT delete %s: %v", sub, err)
		}
	}
}
