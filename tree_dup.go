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
	handledIdx   int             // index into Confirmed up to which trees have been offered
	deletedPaths map[string]bool // files deleted during progressive phase
	Workers      int             // parallel workers for pair verification (0 = NumCPU)
	OnProgress   func(done, total int) // called after each verified pair; may be nil
}

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
		deletedPaths: make(map[string]bool),
	}
}

// DeletedPaths returns a copy of all paths deleted during progressive handling.
func (s *TreeDupState) DeletedPaths() map[string]bool {
	out := make(map[string]bool, len(s.deletedPaths))
	for k, v := range s.deletedPaths {
		out[k] = v
	}
	return out
}

// MarkDeleted records a path as deleted (called by OfferTreeDeletions).
func (s *TreeDupState) MarkDeleted(path string) {
	s.deletedPaths[path] = true
}

// MarkHandled advances the handled index after a batch of trees has been offered.
func (s *TreeDupState) MarkHandled() {
	s.handledIdx = len(s.Confirmed)
}

// UnhandledTrees returns trees in Confirmed that haven't been offered yet.
func (s *TreeDupState) UnhandledTrees() []TreeDupPair {
	return s.Confirmed[s.handledIdx:]
}

// AddGroups ingests new dup groups and returns any newly discovered tree dup pairs.
// allFiles must be sorted by Path (call SortFilesByPath first).
func (s *TreeDupState) AddGroups(newGroups []DupGroup, allFiles []ScannedFile, verified bool) []TreeDupPair {
	if len(newGroups) == 0 {
		return nil
	}

	// Update dup index with new groups
	for _, g := range newGroups {
		paths := make([]string, len(g.Files))
		for i, f := range g.Files {
			paths[i] = f.Path
		}
		for _, f := range g.Files {
			dups := make([]string, 0, len(paths)-1)
			for _, p := range paths {
				if p != f.Path {
					dups = append(dups, p)
				}
			}
			// Merge with existing dups (earlier rounds may have added some)
			s.dupIndex[f.Path] = mergeDedupe(s.dupIndex[f.Path], dups)
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

	verifyPair := func(i int, dk dirKey) {
		filesA := filesUnderDir(dk.a, allFiles)
		filesB := filesUnderDir(dk.b, allFiles)
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

	newPairs = removeSubPairsFrom(newPairs, s.Confirmed)
	// Also remove sub-pairs within the new batch
	newPairs = removeSubPairs(newPairs)

	sort.Slice(newPairs, func(i, j int) bool {
		return newPairs[i].TotalSize > newPairs[j].TotalSize
	})

	s.Confirmed = append(s.Confirmed, newPairs...)
	return newPairs
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

// FindTreeDups finds all tree dup pairs in one shot.
// allFiles must be sorted by Path.
func FindTreeDups(groups []DupGroup, allFiles []ScannedFile) []TreeDupPair {
	state := NewTreeDupState()
	return state.AddGroups(groups, allFiles, false)
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
	const maxDepth = 8

	for i, f := range allFiles {
		if onProgress != nil && i%100000 == 0 {
			onProgress(i, total)
		}

		// Walk up the directory tree using zero-allocation string slicing.
		// current is always a slice of f.Path (or "/" which is a constant).
		p := f.Path
		current := p[:strings.LastIndexByte(p, '/')] // immediate parent, no alloc
		if current == "" {
			current = "/"
		}

		for depth := 0; depth < maxDepth; depth++ {
			dirLen := len(current)
			dd := accum[current]
			if dd == nil {
				dd = &dirAccum{}
				accum[current] = dd
			}
			// relPath = everything after the trailing slash — zero allocation
			dd.hash += hashTreeEntry(p[dirLen+1:], f.Size, f.ModTime)
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

// removeSubPairsFrom drops pairs that are sub-trees of anything in 'reference'.
func removeSubPairsFrom(pairs []TreeDupPair, reference []TreeDupPair) []TreeDupPair {
	if len(reference) == 0 {
		return pairs
	}
	// Use the fast version when reference is large.
	combined := removeSubPairsFast(append(append([]TreeDupPair(nil), reference...), pairs...))
	// Return only pairs that survived and weren't already in reference.
	refSet := make(map[[2]string]bool, len(reference))
	for _, r := range reference {
		a, b := r.DirA, r.DirB
		if b < a {
			a, b = b, a
		}
		refSet[[2]string{a, b}] = true
	}
	result := combined[:0]
	for _, p := range combined {
		a, b := p.DirA, p.DirB
		if b < a {
			a, b = b, a
		}
		if !refSet[[2]string{a, b}] {
			result = append(result, p)
		}
	}
	return result
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

	// partnerOf[dir] = list of confirmed partner dirs.
	// When we confirm pair (A,B), we add A→B and B→A.
	partnerOf := make(map[string][]string, len(pairs)/4)
	addPartner := func(d, partner string) {
		partnerOf[d] = append(partnerOf[d], partner)
	}

	// isDominated: walk up from a (and b for cross-case), look up partnerOf at each
	// ancestor, and check if the partner is an ancestor of b (or a).
	isDominated := func(a, b string) bool {
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
		// Cross-case: walk up from b
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

	result := make([]TreeDupPair, 0, len(pairs)/4)
	for _, p := range pairs {
		if isDominated(p.DirA, p.DirB) {
			continue
		}
		result = append(result, p)
		addPartner(p.DirA, p.DirB)
		addPartner(p.DirB, p.DirA)
	}
	return result
}

func mergeDedupe(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	for _, s := range a {
		seen[s] = true
	}
	result := append([]string(nil), a...)
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}
