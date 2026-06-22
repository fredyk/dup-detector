package main

import "sort"

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

// BuildOverlapBlocks groups cross-root dup groups into 2-column blocks and
// returns the blocks plus the groups NOT absorbed into any block (which remain
// individual file-level actions). roots maps Source index → root path.
//
// Only groups spanning EXACTLY two roots are eligible (the clean 2-column case);
// a group spanning 3+ roots is left as a normal file group (N-way dedup is not a
// 2-column view). A root pair needs >= minOverlap shared files to form blocks;
// fewer → those groups stay as file groups too. Within a pair, items are sorted
// by size DESC and chunked into blocks of blockSize, so block 1 holds the
// blockSize LARGEST shared files.
func BuildOverlapBlocks(groups []DupGroup, roots []string, blockSize, minOverlap int) (blocks []dirOverlapBlock, remaining []DupGroup) {
	if blockSize <= 0 {
		blockSize = defaultOverlapBlockSize
	}
	if minOverlap < 2 {
		minOverlap = 2
	}

	type pairKey struct{ a, b int }
	type cand struct {
		gi   int
		item overlapItem
	}
	pairItems := map[pairKey][]cand{}

	for gi, g := range groups {
		bySrc := map[int][]ScannedFile{}
		for _, f := range g.Files {
			bySrc[f.Source] = append(bySrc[f.Source], f)
		}
		if len(bySrc) != 2 { // only clean 2-root groups become blocks
			continue
		}
		srcs := make([]int, 0, 2)
		for s := range bySrc {
			srcs = append(srcs, s)
		}
		sort.Ints(srcs)
		pk := pairKey{srcs[0], srcs[1]}
		pairItems[pk] = append(pairItems[pk], cand{gi, overlapItem{
			size: g.Size, aFiles: bySrc[srcs[0]], bFiles: bySrc[srcs[1]],
		}})
	}

	absorbed := map[int]bool{}
	// Deterministic order over pairs (map iteration is random).
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
				rootA: roots[pk.a], rootB: roots[pk.b],
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
