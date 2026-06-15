package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// ── fixture helpers ───────────────────────────────────────────────────────────

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildFixture creates a known tree under a temp dir and returns its root.
//
//   root/
//     A/
//       a.txt        = "hello world"        (content-dup of B/b.txt)
//       uniq.txt     = "i am unique-sized"  (unique size → never a dup)
//       dupsize1.bin = "AAAA" (4 bytes)      (size-collision w/ dupsize2, diff content)
//       dupsize2.bin = "BBBB" (4 bytes)
//       tree/x       = "xxxx"
//       tree/y       = "yyyy"
//     B/
//       b.txt        = "hello world"         (content-dup of A/a.txt)
//       tree2/x      = "xxxx"
//       tree2/y      = "yyyy"                (tree/ == tree2/ → tree dup)
func buildFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "A/a.txt"), "hello world")
	writeFile(t, filepath.Join(root, "A/uniq.txt"), "i am unique-sized")
	writeFile(t, filepath.Join(root, "A/dupsize1.bin"), "AAAA")
	writeFile(t, filepath.Join(root, "A/dupsize2.bin"), "BBBB")
	writeFile(t, filepath.Join(root, "A/tree/x"), "xxxx")
	writeFile(t, filepath.Join(root, "A/tree/y"), "yyyy")
	writeFile(t, filepath.Join(root, "B/b.txt"), "hello world")
	writeFile(t, filepath.Join(root, "B/tree2/x"), "xxxx")
	writeFile(t, filepath.Join(root, "B/tree2/y"), "yyyy")
	return root
}

func scanDir(t *testing.T, dir string) []ScannedFile {
	t.Helper()
	c := &Config{Recursive: true}
	files, err := Scan(dir, c, nil, nil, nil)
	if err != nil {
		t.Fatalf("Scan(%s): %v", dir, err)
	}
	return files
}

// groupSig returns a stable signature of a dup group: sorted basenames.
func groupSig(g DupGroup) []string {
	s := make([]string, len(g.Files))
	for i, f := range g.Files {
		s[i] = filepath.Base(f.Path)
	}
	sort.Strings(s)
	return s
}

// ── golden tests (lock current behavior before the SQLite refactor) ───────────

func TestScanCountsRegularFiles(t *testing.T) {
	root := buildFixture(t)
	files := scanDir(t, root)
	if len(files) != 9 {
		t.Fatalf("expected 9 files, got %d", len(files))
	}
}

func TestGroupBySizeKeepsOnlyCollisions(t *testing.T) {
	root := buildFixture(t)
	files := scanDir(t, root)
	bySize := GroupBySize(files)
	// "hello world" (11B) x2, "AAAA"/"BBBB" (4B) x... plus tree files "xxxx"/"yyyy"
	// (4B) x4. So size 4 and size 11 collide; size 17 (uniq.txt) does not.
	for size, g := range bySize {
		if len(g) < 2 {
			t.Fatalf("size %d kept with <2 files", size)
		}
	}
	if _, ok := bySize[17]; ok {
		t.Fatalf("unique-size file (17B) should not be in collision map")
	}
}

func TestChecksumDupsFindsContentGroups(t *testing.T) {
	root := buildFixture(t)
	files := scanDir(t, root)
	var groups []DupGroup
	err := ChecksumDups(files, nil, false, nil, 2, nil, nil,
		func(ng []DupGroup) bool { groups = append(groups, ng...); return true })
	if err != nil {
		t.Fatal(err)
	}
	// Expect: {a.txt,b.txt} (hello world) and {x,x} and {y,y} (4-byte tree files).
	// dupsize1/dupsize2 share size but NOT content → not a group.
	sigs := map[string]bool{}
	for _, g := range groups {
		sigs[joinSig(groupSig(g))] = true
	}
	if !sigs["a.txt|b.txt"] {
		t.Errorf("missing content-dup group a.txt|b.txt; got %v", sigs)
	}
	if sigs["dupsize1.bin|dupsize2.bin"] {
		t.Errorf("dupsize1/dupsize2 collide on size but differ in content — must NOT group")
	}
}

func TestFindTreeDupsByHashFindsIdenticalSubtree(t *testing.T) {
	root := buildFixture(t)
	files := scanDir(t, root)
	SortFilesByPath(files)
	pairs := FindTreeDupsByHash(files, nil)
	found := false
	for _, p := range pairs {
		if filepath.Base(p.DirA) == "tree" && filepath.Base(p.DirB) == "tree2" ||
			filepath.Base(p.DirA) == "tree2" && filepath.Base(p.DirB) == "tree" {
			found = true
			if p.FileCount != 2 {
				t.Errorf("tree pair FileCount = %d, want 2", p.FileCount)
			}
		}
	}
	if !found {
		t.Errorf("expected tree dup pair tree/ <-> tree2/, got %d pairs: %+v", len(pairs), pairs)
	}
}

func joinSig(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += "|"
		}
		out += x
	}
	return out
}
