package main

import (
	"path/filepath"
	"sort"
	"strings"
)

// Partial directory-overlap detection.
//
// When two roots are not identical but share many files, those shared files
// otherwise show up as individual file-level dup groups → resolved one by one
// in the interactive queue. This groups the cross-root shared files of a pair
// of roots into "blocks" of up to N files (the N largest first), presented in
// two columns so a whole column can be deduped at once.
//
// A shared item is matched by DUPLICATE IDENTITY (the DupGroup: size+mtime, or
// MD5 with -c) — NOT by relative path. RelPath is display-only.

const (
	defaultOverlapBlockSize = 30 // files shown per 2-column block ("up to 30")
	defaultMinOverlap       = 2  // min shared files for a root pair to form blocks
)

// overlapItem is one shared content-group between two roots: the members of a
// single DupGroup that fall under rootA and under rootB. Normally 1 file each
// (a backup file present in both); >1 per side when a root has internal copies.
type overlapItem struct {
	size   int64
	aFiles []ScannedFile // group members under rootA (>=1)
	bFiles []ScannedFile // group members under rootB (>=1)
}

// dirOverlapBlock is up to blockSize shared items between two roots, presented
// as one 2-column interactive action.
type dirOverlapBlock struct {
	rootA, rootB string
	items        []overlapItem
	sharedBytes  int64 // Σ item.size (one copy each) — drives queue ordering
}

// aSide / bSide flatten the files of one column across all items in the block.
func (b *dirOverlapBlock) aSide() []ScannedFile { return b.side(true) }
func (b *dirOverlapBlock) bSide() []ScannedFile { return b.side(false) }
func (b *dirOverlapBlock) side(a bool) []ScannedFile {
	var out []ScannedFile
	for _, it := range b.items {
		if a {
			out = append(out, it.aFiles...)
		} else {
			out = append(out, it.bFiles...)
		}
	}
	return out
}

// columnOf maps a file to the "column" (a directory path) it belongs to in the
// 2-column overlap view, or "" to exclude it. Two modes:
//   - multi-root: the file's root path (roots[f.Source]).
//   - single-root: the depth-N ancestor subdir under the root (virtualRootOf).
type columnOf func(ScannedFile) string

// virtualRootOf returns the depth-N ancestor directory of path under root — the
// overlap "column" in single-root mode — or "" if path is not deep enough (a
// file at/above the column level can't be attributed to a comparable subdir).
// Example: root=/bk, depth=1, /bk/2024-01/a/x → /bk/2024-01.
func virtualRootOf(path, root string, depth int) string {
	if depth < 1 {
		depth = 1
	}
	sep := string(filepath.Separator)
	rel := strings.TrimPrefix(path, root+sep)
	if rel == path {
		return "" // not under root (shouldn't happen)
	}
	parts := strings.Split(rel, sep)
	if len(parts) <= depth {
		return "" // file sits at/above the column level → no subdir to compare
	}
	return root + sep + filepath.Join(parts[:depth]...)
}

// BuildOverlapBlocks groups dup groups into 2-column overlap blocks keyed by
// colOf (root path for multi-root, or depth-N subdir for single-root), and
// returns the blocks plus the groups NOT absorbed (which stay as individual
// file-level actions).
//
// Only groups spanning EXACTLY two columns are eligible (the clean 2-column
// case); a group spanning 3+ columns is left as a normal file group. A column
// pair needs >= minOverlap shared files to form blocks. Within a pair, items
// are sorted by size DESC and chunked into blocks of blockSize, so block 1
// holds the blockSize LARGEST shared files.
func BuildOverlapBlocks(groups []DupGroup, colOf columnOf, blockSize, minOverlap int) (blocks []dirOverlapBlock, remaining []DupGroup) {
	if blockSize <= 0 {
		blockSize = defaultOverlapBlockSize
	}
	if minOverlap < 2 {
		minOverlap = 2
	}

	type pairKey struct{ a, b string }
	type cand struct {
		gi   int
		item overlapItem
	}
	pairItems := map[pairKey][]cand{}

	for gi, g := range groups {
		byCol := map[string][]ScannedFile{}
		for _, f := range g.Files {
			col := colOf(f)
			if col == "" {
				continue
			}
			byCol[col] = append(byCol[col], f)
		}
		if len(byCol) != 2 { // only clean 2-column groups become blocks
			continue
		}
		cols := make([]string, 0, 2)
		for c := range byCol {
			cols = append(cols, c)
		}
		sort.Strings(cols)
		pk := pairKey{cols[0], cols[1]}
		pairItems[pk] = append(pairItems[pk], cand{gi, overlapItem{
			size: g.Size, aFiles: byCol[cols[0]], bFiles: byCol[cols[1]],
		}})
	}

	absorbed := map[int]bool{}
	pks := make([]pairKey, 0, len(pairItems))
	for pk := range pairItems {
		pks = append(pks, pk)
	}
	sort.Slice(pks, func(i, j int) bool {
		if pks[i].a != pks[j].a {
			return pks[i].a < pks[j].a
		}
		return pks[i].b < pks[j].b
	})

	for _, pk := range pks {
		cs := pairItems[pk]
		if len(cs) < minOverlap {
			continue // too few shared files → leave as individual file groups
		}
		// Largest shared files first → block 1 = the blockSize biggest.
		sort.SliceStable(cs, func(i, j int) bool { return cs[i].item.size > cs[j].item.size })
		for start := 0; start < len(cs); start += blockSize {
			end := start + blockSize
			if end > len(cs) {
				end = len(cs)
			}
			chunk := cs[start:end]
			items := make([]overlapItem, 0, len(chunk))
			var sb int64
			for _, c := range chunk {
				items = append(items, c.item)
				sb += c.item.size
				absorbed[c.gi] = true
			}
			blocks = append(blocks, dirOverlapBlock{
				rootA: pk.a, rootB: pk.b,
				items: items, sharedBytes: sb,
			})
		}
	}

	for gi, g := range groups {
		if !absorbed[gi] {
			remaining = append(remaining, g)
		}
	}
	return blocks, remaining
}
