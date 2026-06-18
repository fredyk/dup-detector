package main

import (
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// TreeDupPair represents two directory trees with byte-identical content.
type TreeDupPair struct {
	DirA      string
	DirB      string
	FileCount int   // number of files in each tree
	TotalSize int64 // total bytes (one copy)
	Verified  bool  // true = MD5 confirmed; false = size+mtime only
}

// ── Incremental state ────────────────────────────────────────────────────────

// TreeDupState allows incremental tree dup detection across multiple rounds
// of new dup groups, avoiding re-checking already-verified dir pairs.
type TreeDupState struct {
	dupIndex     map[string][]string // filePath → []duplicatePaths
	checkedPairs map[dirKey]bool     // already verified (confirmed or rejected)
	Confirmed    []TreeDupPair
	// partnerOf is the persistent dominator index over every pair in Confirmed:
	// for a confirmed pair (A,B) it holds A→B and B→A. AddGroups checks new pairs
	// against it incrementally (O(depth) per pair) instead of rebuilding and
	// re-sorting the whole Confirmed set every batch (which was O(batches·|Confirmed|),
	// the real CPU hog found via pprof). INVARIANT: partnerOf carries exactly the
	// partners of the pairs in Confirmed — every write to Confirmed must go through
	// addConfirmed/AddConfirmed so the two stay in lockstep.
	partnerOf    map[string][]string
	Workers      int                   // parallel workers for pair verification (0 = NumCPU)
	OnProgress   func(done, total int) // called after each verified pair; may be nil
	// CountUnder, if set, returns the file count under a dir WITHOUT materializing
	// the list (store-backed SQL COUNT). Used to reject mismatched candidate pairs
	// cheaply before loading their (possibly millions of) files. nil → len(lookup).
	CountUnder DirCounter
	// CoverageCheck, if set, streams the verification that every file under dirA
	// has a dupIndex member under dirB (and vice versa), accumulating total size.
	// Avoids materializing millions of paths into []ScannedFile. When nil, falls
	// back to the materializing lookup + allCoveredByIndex path.
	CoverageCheck func(dirA, dirB string, index map[string][]string) (bool, int64, error)
}

// DirCounter returns the number of files under dir/ cheaply (no materialization).
type DirCounter func(dir string) int

type dirKey struct{ a, b string } // a < b always (canonical form)

func newDirKey(x, y string) dirKey {
	if x < y {
		return dirKey{x, y}
	}
	return dirKey{y, x}
}

// NewTreeDupState returns an empty incremental state.
func NewTreeDupState() *TreeDupState {
	return &TreeDupState{
		dupIndex:     make(map[string][]string),
		checkedPairs: make(map[dirKey]bool),
		partnerOf:    make(map[string][]string),
	}
}

// addConfirmed appends a pair to Confirmed and registers it in partnerOf so it
// dominates deeper pairs discovered later. The sole writer of Confirmed.
func (s *TreeDupState) addConfirmed(p TreeDupPair) {
	s.Confirmed = append(s.Confirmed, p)
	s.partnerOf[p.DirA] = append(s.partnerOf[p.DirA], p.DirB)
	s.partnerOf[p.DirB] = append(s.partnerOf[p.DirB], p.DirA)
}

// AddConfirmed seeds the state with externally-discovered confirmed pairs (e.g.
// the early mtime/content tree pass) so subsequent AddGroups calls treat them
// as dominators. Pairs are assumed already de-duplicated among themselves.
func (s *TreeDupState) AddConfirmed(pairs []TreeDupPair) {
	for _, p := range pairs {
		s.addConfirmed(p)
	}
}

// DirLookup returns all files under dir/ (backed by either a sorted slice or a
// FileStore prefix query). Replaces threading the full []ScannedFile around.
type DirLookup func(dir string) []ScannedFile

// AddGroups ingests new dup groups and returns any newly discovered tree dup pairs.
// lookup resolves files-under-dir (slice- or store-backed).
func (s *TreeDupState) AddGroups(newGroups []DupGroup, lookup DirLookup, verified bool) []TreeDupPair {
	if len(newGroups) == 0 {
		return nil
	}

	// Update dup index with new groups.
	// Use one shared members slice per group (O(k) instead of O(k²) per group):
	// every file in the group gets a reference to the same k-length slice. The
	// slice includes f itself; allCoveredByIndex skips the self entry so coverage
	// still requires a *different* group member under the target dir (preserving
	// the pre-optimization semantics for nested candidate dir pairs).
	for _, g := range newGroups {
		members := make([]string, len(g.Files))
		for i, f := range g.Files {
			members[i] = f.Path
		}
		for _, f := range g.Files {
			s.dupIndex[f.Path] = members // shared slice, O(1) per file
		}
	}

	// Generate candidate pairs from NEW groups only
	var progressFn func(done, total int)
	if s.OnProgress != nil {
		progressFn = func(done, total int) {
			s.OnProgress(-(done + 1), total) // negative = candidate-gen phase
		}
	}
	candidates := s.newCandidatePairs(newGroups, progressFn)

	// Filter to unchecked pairs and mark them checked now (single-threaded)
	unchecked := make([]dirKey, 0, len(candidates))
	for dk := range candidates {
		if !s.checkedPairs[dk] {
			s.checkedPairs[dk] = true
			unchecked = append(unchecked, dk)
		}
	}

	// Verify pairs in parallel
	workers := s.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers > len(unchecked) {
		workers = len(unchecked)
	}

	type result struct {
		pair TreeDupPair
		ok   bool
	}
	results := make([]result, len(unchecked))

	countUnder := func(dir string) int {
		if s.CountUnder != nil {
			return s.CountUnder(dir)
		}
		return len(lookup(dir))
	}

	verifyPair := func(i int, dk dirKey) {
		// Cheap count check first: a tree-dup pair needs equal file counts. Reject
		// mismatches here (indexed COUNT) without materializing the file lists —
		// top-level candidate dirs can hold millions of files (pprof: this was the
		// MD5-phase RAM spike via FilesUnderDir).
		na := countUnder(dk.a)
		if na == 0 || na != countUnder(dk.b) {
			return
		}
		// Streaming coverage check: when available (store-backed), iterate each
		// file under the dir one row at a time instead of loading all paths into
		// a []ScannedFile slice. Avoids the 2.4 GB RSS / 10.7 GB virtual spike
		// from FilesUnderDir on large dirs (pprof: issue #16).
		if s.CoverageCheck != nil {
			ok, totalSize, err := s.CoverageCheck(dk.a, dk.b, s.dupIndex)
			if err != nil || !ok {
				return
			}
			ok, _, err = s.CoverageCheck(dk.b, dk.a, s.dupIndex)
			if err != nil || !ok {
				return
			}
			results[i] = result{TreeDupPair{
				DirA: dk.a, DirB: dk.b,
				FileCount: na, TotalSize: totalSize, Verified: verified,
			}, true}
			return
		}
		filesA := lookup(dk.a)
		filesB := lookup(dk.b)
		if len(filesA) == 0 || len(filesB) == 0 || len(filesA) != len(filesB) {
			return
		}
		if !allCoveredByIndex(filesA, dk.b, s.dupIndex) {
			return
		}
		if !allCoveredByIndex(filesB, dk.a, s.dupIndex) {
			return
		}
		var totalSize int64
		for _, f := range filesA {
			totalSize += f.Size
		}
		results[i] = result{TreeDupPair{
			DirA: dk.a, DirB: dk.b,
			FileCount: len(filesA), TotalSize: totalSize, Verified: verified,
		}, true}
	}

	total := len(unchecked)
	if workers <= 1 {
		for i, dk := range unchecked {
			verifyPair(i, dk)
			if s.OnProgress != nil {
				s.OnProgress(i+1, total)
			}
		}
	} else {
		var wg sync.WaitGroup
		var done sync.WaitGroup
		ch := make(chan int, len(unchecked))
		for k := range unchecked {
			ch <- k
		}
		close(ch)

		// Progress counter goroutine
		var progressCh chan struct{}
		if s.OnProgress != nil {
			progressCh = make(chan struct{}, len(unchecked))
			done.Add(1)
			go func() {
				defer done.Done()
				n := 0
				for range progressCh {
					n++
					s.OnProgress(n, total)
				}
			}()
		}

		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range ch {
					verifyPair(i, unchecked[i])
					if progressCh != nil {
						progressCh <- struct{}{}
					}
				}
			}()
		}
		wg.Wait()
		if progressCh != nil {
			close(progressCh)
			done.Wait()
		}
	}

	var newPairs []TreeDupPair
	for _, r := range results {
		if r.ok {
			newPairs = append(newPairs, r.pair)
		}
	}

	// Drop pairs dominated by an already-confirmed pair (the persistent partnerOf)
	// or by a higher pair within this same batch — equivalent to the old
	// removeSubPairsFrom(newPairs, Confirmed) + removeSubPairs(newPairs), but
	// without rebuilding/re-sorting all of Confirmed each batch. Process
	// shortest-path-first so a higher pair is registered before a deeper one is
	// tested against it (a dominator's paths are prefixes ⇒ never longer).
	sort.Slice(newPairs, func(i, j int) bool {
		return len(newPairs[i].DirA)+len(newPairs[i].DirB) < len(newPairs[j].DirA)+len(newPairs[j].DirB)
	})
	kept := newPairs[:0]
	for _, p := range newPairs {
		if treeDominated(s.partnerOf, p.DirA, p.DirB) {
			continue
		}
		// Register immediately so later (longer) pairs in this batch see it.
		s.addConfirmed(p)
		kept = append(kept, p)
	}

	// Return largest-first (Confirmed order is irrelevant; callers sort).
	sort.Slice(kept, func(i, j int) bool {
		return kept[i].TotalSize > kept[j].TotalSize
	})
	return kept
}

// maxDirsPerGroup caps the number of unique parent directories from a single
// dup group used to generate candidate pairs. Files that appear in >maxDirsPerGroup
// different directories are ubiquitous (thumbnails, empty files…) and can't
// realistically be tree-dup markers.
const maxDirsPerGroup = 50

func (s *TreeDupState) newCandidatePairs(newGroups []DupGroup, onProgress func(done, total int)) map[dirKey]bool {
	candidates := make(map[dirKey]bool)
	total := len(newGroups)
	for gi, g := range newGroups {
		if onProgress != nil && gi%500 == 0 {
			onProgress(gi, total)
		}

		// Collect unique immediate parent directories for this group
		seen := make(map[string]bool, len(g.Files))
		var dirs []string
		for _, f := range g.Files {
			d := filepath.Dir(f.Path)
			if !seen[d] {
				seen[d] = true
				dirs = append(dirs, d)
				if len(dirs) == maxDirsPerGroup {
					break
				}
			}
		}

		// Generate candidate pairs from unique dirs (O(d²) where d ≤ maxDirsPerGroup)
		for i := 0; i < len(dirs); i++ {
			for j := i + 1; j < len(dirs); j++ {
				da, db := dirs[i], dirs[j]
				for depth := 0; depth < 12; depth++ {
					if da == "." || da == "/" || db == "." || db == "/" || da == db {
						break
					}
					candidates[newDirKey(da, db)] = true
					da = filepath.Dir(da)
					db = filepath.Dir(db)
				}
			}
		}
	}
	if onProgress != nil {
		onProgress(total, total)
	}
	return candidates
}

// ── Standalone FindTreeDups (used in non-progressive mode) ───────────────────

// SliceLookup adapts a sorted []ScannedFile into a DirLookup (binary search).
// allFiles must be sorted by Path.
func SliceLookup(allFiles []ScannedFile) DirLookup {
	return func(dir string) []ScannedFile { return filesUnderDir(dir, allFiles) }
}

// FindTreeDups finds all tree dup pairs in one shot.
// allFiles must be sorted by Path.
func FindTreeDups(groups []DupGroup, allFiles []ScannedFile) []TreeDupPair {
	state := NewTreeDupState()
	return state.AddGroups(groups, SliceLookup(allFiles), false)
}

// FindTreeDupsByHash detects tree duplicates in O(N·depth) time using a
// commutative hash accumulated per directory — no dup-groups needed.
// allFiles must be sorted by Path.
// onProgress(done, total) is called periodically if non-nil.
func FindTreeDupsByHash(allFiles []ScannedFile, onProgress func(done, total int)) []TreeDupPair {
	type dirAccum struct {
		hash      uint64
		fileCount int
		totalSize int64
	}

	accum := make(map[string]*dirAccum, len(allFiles)/8)
	total := len(allFiles)

	for i, f := range allFiles {
		if onProgress != nil && i%100000 == 0 {
			onProgress(i, total)
		}

		// Walk up the entire directory tree using zero-allocation string slicing.
		// Each ancestor accumulates the file's contribution with its own relative path.
		p := f.Path
		sep := strings.LastIndexByte(p, '/')
		if sep < 0 {
			continue // relative path with no separator — nothing to accumulate under
		}
		current := p[:sep] // immediate parent, no alloc
		if current == "" {
			current = "/"
		}

		for {
			dd := accum[current]
			if dd == nil {
				dd = &dirAccum{}
				accum[current] = dd
			}
			// relPath = everything past the separator between current and the
			// remainder. For current == "/" that's p[1:]; otherwise p[len(current)+1:].
			var relPath string
			if current == "/" {
				relPath = p[1:]
			} else {
				relPath = p[len(current)+1:]
			}
			dd.hash += hashTreeEntry(relPath, f.Size, f.ModTime)
			dd.fileCount++
			dd.totalSize += f.Size

			// Walk up: slice to last '/'
			sep := strings.LastIndexByte(current, '/')
			if sep <= 0 {
				break
			}
			current = current[:sep]
		}
	}

	if onProgress != nil {
		onProgress(total, total)
	}

	// Group directories by (hash, fileCount) — same key = identical tree content
	type key struct {
		hash      uint64
		fileCount int
	}
	byKey := make(map[key][]string)
	for dir, dd := range accum {
		if dd.fileCount == 0 {
			continue
		}
		k := key{dd.hash, dd.fileCount}
		byKey[k] = append(byKey[k], dir)
	}

	// maxDirsPerBucket: cap to avoid O(n²) pair explosion from ubiquitous dirs.
	// Single-file dirs (LICENSE, README…) can appear in thousands of places;
	// we skip those via minFileCount regardless of bucket size.
	const maxDirsPerBucket = 2000
	const minFileCount = 2 // ignore single-file dirs — not meaningful tree dups

	var pairs []TreeDupPair
	for k, dirs := range byKey {
		if k.fileCount < minFileCount || len(dirs) < 2 || len(dirs) > maxDirsPerBucket {
			continue
		}
		sort.Strings(dirs)
		for i := 0; i < len(dirs); i++ {
			for j := i + 1; j < len(dirs); j++ {
				di, dj := dirs[i], dirs[j]
				if IsSubdir(di, dj) || IsSubdir(dj, di) {
					continue
				}
				pairs = append(pairs, TreeDupPair{
					DirA:      di,
					DirB:      dj,
					FileCount: accum[di].fileCount,
					TotalSize: accum[di].totalSize,
					Verified:  false,
				})
			}
		}
	}

	pairs = removeSubPairsFast(pairs)

	// Verify every surviving pair: the hash accumulation only walks up maxDepth
	// ancestor levels, so files deeper than that are invisible to the hash.
	// Cross-check all files in each pair to eliminate false positives.
	verified := pairs[:0]
	for _, p := range pairs {
		if verifyTreePairMtime(p.DirA, p.DirB, allFiles) {
			verified = append(verified, p)
		}
	}
	pairs = verified

	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].TotalSize > pairs[j].TotalSize
	})
	return pairs
}

// verifyTreePairMtime returns true iff dirA and dirB contain exactly the same
// files (same relative paths, sizes, and mtimes). allFiles must be sorted by path.
func verifyTreePairMtime(dirA, dirB string, allFiles []ScannedFile) bool {
	filesA := filesUnderDir(dirA, allFiles)
	filesB := filesUnderDir(dirB, allFiles)
	if len(filesA) == 0 || len(filesA) != len(filesB) {
		return false
	}
	prefixA := dirA + string(filepath.Separator)
	prefixB := dirB + string(filepath.Separator)

	type fileKey struct{ size, mtime int64 }
	bMap := make(map[string]fileKey, len(filesB))
	for _, f := range filesB {
		bMap[f.Path[len(prefixB):]] = fileKey{f.Size, f.ModTime}
	}
	for _, f := range filesA {
		rel := f.Path[len(prefixA):]
		bk, ok := bMap[rel]
		if !ok || bk.size != f.Size || bk.mtime != f.ModTime {
			return false
		}
	}
	return true
}

// hashTreeEntry computes a 64-bit FNV-1a hash of (relPath, size, mtime)
// with zero allocations.
func hashTreeEntry(relPath string, size, mtime int64) uint64 {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	for i := 0; i < len(relPath); i++ {
		h ^= uint64(relPath[i])
		h *= prime64
	}
	for i := 0; i < 8; i++ {
		h ^= uint64(size>>(i*8)) & 0xff
		h *= prime64
	}
	for i := 0; i < 8; i++ {
		h ^= uint64(mtime>>(i*8)) & 0xff
		h *= prime64
	}
	return h
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// SortFilesByPath sorts a slice of ScannedFile by path for binary-search lookups.
func SortFilesByPath(files []ScannedFile) {
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
}

// filesUnderDir returns all files whose path starts with dir/.
// allFiles must be sorted by Path.
func filesUnderDir(dir string, allFiles []ScannedFile) []ScannedFile {
	prefix := filepath.Clean(dir) + string(filepath.Separator)
	start := sort.Search(len(allFiles), func(i int) bool {
		return allFiles[i].Path >= prefix
	})
	var result []ScannedFile
	for i := start; i < len(allFiles); i++ {
		if !strings.HasPrefix(allFiles[i].Path, prefix) {
			break
		}
		result = append(result, allFiles[i])
	}
	return result
}

func allCoveredByIndex(files []ScannedFile, dir string, index map[string][]string) bool {
	prefix := filepath.Clean(dir) + string(filepath.Separator)
	for _, f := range files {
		found := false
		for _, dup := range index[f.Path] {
			// dupIndex now stores the file's whole group (shared slice), so it
			// includes f itself; skip it. Coverage requires a *different* file
			// under dir — without this, a nested candidate pair (dir an ancestor
			// of f's dir) would match f against itself and falsely "cover" it.
			if dup == f.Path {
				continue
			}
			if strings.HasPrefix(dup, prefix) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// removeSubPairs drops pairs that are sub-trees of other pairs in the same slice.
// O(n·k) where k = number of surviving (top-level) pairs.
func removeSubPairs(pairs []TreeDupPair) []TreeDupPair {
	return removeSubPairsFast(pairs)
}

// treeDominated reports whether pair (a,b) is a sub-tree of some pair already
// recorded in partnerOf: walk up from a (and, for the cross case, from b),
// looking up each ancestor's partners, and check whether a partner is an
// ancestor of b (resp. a). partnerOf[dir] lists the confirmed partner dirs of
// dir — a confirmed pair (A,B) contributes A→B and B→A.
func treeDominated(partnerOf map[string][]string, a, b string) bool {
	wa := a
	for wa != "" {
		for _, pb := range partnerOf[wa] {
			if pb == b || IsSubdir(pb, b) {
				return true
			}
		}
		sep := strings.LastIndexByte(wa, '/')
		if sep <= 0 {
			break
		}
		wa = wa[:sep]
	}
	wb := b
	for wb != "" {
		for _, pa := range partnerOf[wb] {
			if pa == a || IsSubdir(pa, a) {
				return true
			}
		}
		sep := strings.LastIndexByte(wb, '/')
		if sep <= 0 {
			break
		}
		wb = wb[:sep]
	}
	return false
}

// removeSubPairsFast removes pairs dominated by higher-level pairs.
// Uses ancestor-walk + hash map: O(n·depth) instead of O(n·k).
func removeSubPairsFast(pairs []TreeDupPair) []TreeDupPair {
	if len(pairs) <= 1 {
		return pairs
	}
	// Sort: shorter total path length first (higher in tree = dominates deeper ones).
	sort.Slice(pairs, func(i, j int) bool {
		return len(pairs[i].DirA)+len(pairs[i].DirB) < len(pairs[j].DirA)+len(pairs[j].DirB)
	})

	partnerOf := make(map[string][]string, len(pairs)/4)
	result := make([]TreeDupPair, 0, len(pairs)/4)
	for _, p := range pairs {
		if treeDominated(partnerOf, p.DirA, p.DirB) {
			continue
		}
		result = append(result, p)
		partnerOf[p.DirA] = append(partnerOf[p.DirA], p.DirB)
		partnerOf[p.DirB] = append(partnerOf[p.DirB], p.DirA)
	}
	return result
}


