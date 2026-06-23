package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestDeleteOverlapSideKeepsLastCopy is the data-loss guard: deleting a column
// must skip any item whose OTHER side is already gone, so a shared file's last
// copy is never removed.
func TestDeleteOverlapSideKeepsLastCopy(t *testing.T) {
	root := t.TempDir()
	ap := filepath.Join(root, "A", "x")
	bp := filepath.Join(root, "B", "x")
	writeFile(t, ap, "data")
	writeFile(t, bp, "data")
	block := dirOverlapBlock{
		rootA: filepath.Join(root, "A"), rootB: filepath.Join(root, "B"),
		items: []overlapItem{{size: 4, aFiles: []ScannedFile{{Path: ap}}, bFiles: []ScannedFile{{Path: bp}}}},
	}
	deleted := map[string]bool{bp: true} // B-side already removed by a prior action
	deleteOverlapSide(&block, true, deleted, &Config{})
	if deleted[ap] {
		t.Fatal("A-side deleted though B-side was already gone — last copy lost")
	}
	if _, err := os.Stat(ap); err != nil {
		t.Fatalf("A file must still exist: %v", err)
	}
}

// TestDeleteOverlapSideKeepsDirectories enforces the JFMV rule: an overlap
// (2-column) deletion removes ONLY files, never directories — not even ones it
// leaves empty.
func TestDeleteOverlapSideKeepsDirectories(t *testing.T) {
	root := t.TempDir()
	ap := filepath.Join(root, "A", "sub", "x")
	bp := filepath.Join(root, "B", "sub", "x")
	writeFile(t, ap, "data")
	writeFile(t, bp, "data")
	block := dirOverlapBlock{
		rootA: filepath.Join(root, "A"), rootB: filepath.Join(root, "B"),
		items: []overlapItem{{size: 4, aFiles: []ScannedFile{{Path: ap}}, bFiles: []ScannedFile{{Path: bp}}}},
	}
	deleteOverlapSide(&block, false, map[string]bool{}, &Config{}) // delete B side
	if _, err := os.Stat(bp); err == nil {
		t.Fatal("B file should be gone")
	}
	// The now-empty B/sub directory must still exist.
	if _, err := os.Stat(filepath.Join(root, "B", "sub")); err != nil {
		t.Fatalf("B/sub directory must remain (overlap deletes only files): %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "B")); err != nil {
		t.Fatalf("B directory must remain: %v", err)
	}
}

// TestDeleteOverlapSideDeletesChosenColumn: normal case deletes the chosen
// column and keeps the other.
func TestDeleteOverlapSideDeletesChosenColumn(t *testing.T) {
	root := t.TempDir()
	ap := filepath.Join(root, "A", "x")
	bp := filepath.Join(root, "B", "x")
	writeFile(t, ap, "data")
	writeFile(t, bp, "data")
	block := dirOverlapBlock{
		rootA: filepath.Join(root, "A"), rootB: filepath.Join(root, "B"),
		items: []overlapItem{{size: 4, aFiles: []ScannedFile{{Path: ap}}, bFiles: []ScannedFile{{Path: bp}}}},
	}
	deleted := map[string]bool{}
	deleteOverlapSide(&block, false, deleted, &Config{}) // delete column B
	if !deleted[bp] || deleted[ap] {
		t.Fatalf("expected B deleted and A kept; deleted=%v", deleted)
	}
	if _, err := os.Stat(bp); err == nil {
		t.Fatal("B file should be gone")
	}
	if _, err := os.Stat(ap); err != nil {
		t.Fatal("A file should still exist")
	}
}

// bySourceColumn is the multi-root column mapping (file's root path by Source).
func bySourceColumn(roots []string) columnOf {
	return func(f ScannedFile) string {
		if f.Source >= 0 && f.Source < len(roots) {
			return roots[f.Source]
		}
		return ""
	}
}

func TestVirtualRootOf(t *testing.T) {
	cases := []struct {
		path, root string
		depth      int
		want       string
	}{
		{"/bk/2024-01/a/x", "/bk", 1, "/bk/2024-01"},
		{"/bk/2024-01/a/x", "/bk", 2, "/bk/2024-01/a"},
		{"/bk/2024-01/x", "/bk", 1, "/bk/2024-01"},
		{"/bk/2024-01/x", "/bk", 2, ""}, // file sits exactly at depth 2 → no depth-2 subdir
		{"/bk/top.txt", "/bk", 1, ""},   // directly under root → no subdir column
		{"/other/x", "/bk", 1, ""},      // not under root
	}
	for _, c := range cases {
		if got := virtualRootOf(c.path, c.root, c.depth); got != c.want {
			t.Errorf("virtualRootOf(%q,%q,%d) = %q, want %q", c.path, c.root, c.depth, got, c.want)
		}
	}
}

// TestBuildOverlapBlocksSingleRoot: Fase 2 — within ONE root, subdirs at depth 1
// are compared pairwise as columns; a group internal to one subdir is not a block.
func TestBuildOverlapBlocksSingleRoot(t *testing.T) {
	root := "/bk"
	colOf := func(f ScannedFile) string { return virtualRootOf(f.Path, root, 1) }
	mk := func(size int64, pa, pb string) DupGroup {
		return DupGroup{Size: size, Files: []ScannedFile{{Path: pa, Size: size}, {Path: pb, Size: size}}}
	}
	groups := []DupGroup{
		mk(300, "/bk/A/photos/x", "/bk/B/photos/x"),
		mk(200, "/bk/A/y", "/bk/B/y"),
		mk(100, "/bk/A/z", "/bk/B/z"),
		mk(50, "/bk/A/dup1", "/bk/A/dup2"), // internal to A → 1 column → not a block
	}
	blocks, remaining := BuildOverlapBlocks(groups, colOf, 30, 2)
	if len(blocks) != 1 {
		t.Fatalf("want 1 block (A vs B), got %d", len(blocks))
	}
	b := blocks[0]
	if b.rootA != "/bk/A" || b.rootB != "/bk/B" {
		t.Errorf("columns = %q, %q (want /bk/A, /bk/B)", b.rootA, b.rootB)
	}
	if len(b.items) != 3 || b.items[0].size != 300 {
		t.Errorf("items=%d first=%d (want 3, largest 300)", len(b.items), b.items[0].size)
	}
	if len(remaining) != 1 {
		t.Fatalf("internal-to-A group should remain a file group; remaining=%d", len(remaining))
	}
}

func mkGroup(size int64, srcs ...int) DupGroup {
	g := DupGroup{Size: size}
	for i, s := range srcs {
		g.Files = append(g.Files, ScannedFile{
			Path:   fmt.Sprintf("/r%d/f_%d_%d", s, size, i),
			Size:   size,
			Source: s,
		})
	}
	return g
}

func TestBuildOverlapBlocks(t *testing.T) {
	roots := []string{"/r0", "/r1", "/r2"}
	var groups []DupGroup
	// 35 shared files between root 0 and root 1, distinct sizes 1000..966.
	for i := 0; i < 35; i++ {
		groups = append(groups, mkGroup(int64(1000-i), 0, 1))
	}
	// Only 1 shared file between root 0 and root 2 → below minOverlap → no block.
	groups = append(groups, mkGroup(500, 0, 2))
	// A 3-root group → not eligible for a 2-column block.
	groups = append(groups, mkGroup(777, 0, 1, 2))

	blocks, remaining := BuildOverlapBlocks(groups, bySourceColumn(roots), 30, 2)

	// Pair (0,1) with 35 items → blocks of 30 + 5.
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(blocks))
	}
	if len(blocks[0].items) != 30 || len(blocks[1].items) != 5 {
		t.Fatalf("block item counts = %d, %d (want 30, 5)", len(blocks[0].items), len(blocks[1].items))
	}
	// Block 1 must hold the 30 LARGEST (sizes 1000..971), block 2 the next 5.
	if blocks[0].items[0].size != 1000 {
		t.Errorf("block0 first size = %d, want 1000", blocks[0].items[0].size)
	}
	if blocks[0].items[29].size != 971 {
		t.Errorf("block0 last size = %d, want 971", blocks[0].items[29].size)
	}
	if blocks[1].items[0].size != 970 {
		t.Errorf("block1 first size = %d, want 970", blocks[1].items[0].size)
	}
	// sharedBytes = Σ of the 30 largest sizes.
	var want0 int64
	for s := int64(971); s <= 1000; s++ {
		want0 += s
	}
	if blocks[0].sharedBytes != want0 {
		t.Errorf("block0 sharedBytes = %d, want %d", blocks[0].sharedBytes, want0)
	}
	// Roots labeled correctly (lower index = A).
	if blocks[0].rootA != "/r0" || blocks[0].rootB != "/r1" {
		t.Errorf("block roots = %q, %q", blocks[0].rootA, blocks[0].rootB)
	}
	// Remaining = the single-file (0,2) group + the 3-root group.
	if len(remaining) != 2 {
		t.Fatalf("want 2 remaining groups, got %d", len(remaining))
	}
	// aSide/bSide flatten correctly.
	if got := len(blocks[0].aSide()); got != 30 {
		t.Errorf("aSide len = %d, want 30", got)
	}
}

// TestBuildOverlapBlocksBelowThreshold: a pair sharing exactly 1 file forms no
// block and the group stays as a file group.
func TestBuildOverlapBlocksBelowThreshold(t *testing.T) {
	roots := []string{"/r0", "/r1"}
	groups := []DupGroup{mkGroup(100, 0, 1)}
	blocks, remaining := BuildOverlapBlocks(groups, bySourceColumn(roots), 30, 2)
	if len(blocks) != 0 {
		t.Fatalf("want 0 blocks (below minOverlap), got %d", len(blocks))
	}
	if len(remaining) != 1 {
		t.Fatalf("want the group to remain, got %d", len(remaining))
	}
}
