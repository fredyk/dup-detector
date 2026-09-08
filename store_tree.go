package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// countFilesOnDisk counts the regular files under dir by walking the real tree.
func countFilesOnDisk(dir string) (int, error) {
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Type().IsRegular() {
			n++
		}
		return nil
	})
	return n, err
}

// dirStoreChecker answers, per directory, the two questions the tree-pair
// verification asks: how many files the store holds under it, and whether the
// real tree on disk holds more (i.e. some were dropped by a filter or by the
// hardlink skip, which makes a tree-dup claim unsound).
//
// Both answers are memoized because both are fixed within a run — the store is
// read-only after Finalize — while the same directory shows up in EVERY pair of
// its bucket: k identical dirs produce k(k-1)/2 pairs, so the uncached cost was
// O(pairs) full disk walks where O(dirs) suffices. And the guard is armed on
// every run, filters or not: defaultExcludes always leaves cfg.Rules non-empty.
// Measured on a real /tank run: 16 h at 5 % of one core, 66 GB read from the
// storage layer, with 9 of every 10 pprof samples inside this walk.
type dirStoreChecker struct {
	fs        *FileStore
	walkCount func(dir string) (int, error)

	mu         sync.Mutex
	stored     map[string]int
	incomplete map[string]bool
}

func newDirStoreChecker(fs *FileStore) *dirStoreChecker {
	return &dirStoreChecker{
		fs:         fs,
		walkCount:  countFilesOnDisk,
		stored:     make(map[string]int),
		incomplete: make(map[string]bool),
	}
}

// countStored is the memoized fs.CountUnderDir.
func (c *dirStoreChecker) countStored(dir string) (int, error) {
	c.mu.Lock()
	n, ok := c.stored[dir]
	c.mu.Unlock()
	if ok {
		return n, nil
	}
	n, err := c.fs.CountUnderDir(dir)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.stored[dir] = n
	c.mu.Unlock()
	return n, nil
}

// incompleteDir reports whether the real directory on disk holds more regular
// files than the store knows about. Fail safe: any read error returns true.
func (c *dirStoreChecker) incompleteDir(dir string) bool {
	c.mu.Lock()
	v, ok := c.incomplete[dir]
	c.mu.Unlock()
	if ok {
		return v
	}
	res := true
	if stored, err := c.countStored(dir); err == nil {
		if real, werr := c.walkCount(dir); werr == nil {
			res = real != stored
		}
	}
	c.mu.Lock()
	c.incomplete[dir] = res
	c.mu.Unlock()
	return res
}

// FindTreeDupsByHashStore is the store-backed FindTreeDupsByHash. It streams the
// files ordered by path to accumulate a per-directory hash (the accum map is
// O(#dirs), not O(#files)), then verifies candidate pairs with prefix-range
// queries instead of an in-RAM sorted slice.
func FindTreeDupsByHashStore(fs *FileStore, cfg *Config, onProgress func(done, total int)) ([]TreeDupPair, error) {
	type dirAccum struct {
		hash      uint64
		fileCount int
		totalSize int64
	}
	accum := make(map[string]*dirAccum)

	total, err := fs.Count()
	if err != nil {
		return nil, err
	}
	i := 0
	err = fs.IterAllByPath(func(f ScannedFile) error {
		if onProgress != nil && i%100000 == 0 {
			onProgress(i, total)
		}
		i++

		p := f.Path
		sep := strings.LastIndexByte(p, '/')
		if sep < 0 {
			return nil
		}
		current := p[:sep]
		if current == "" {
			current = "/"
		}
		for {
			dd := accum[current]
			if dd == nil {
				dd = &dirAccum{}
				accum[current] = dd
			}
			var relPath string
			if current == "/" {
				relPath = p[1:]
			} else {
				relPath = p[len(current)+1:]
			}
			dd.hash += hashTreeEntry(relPath, f.Size, f.ModTime)
			dd.fileCount++
			dd.totalSize += f.Size

			sep := strings.LastIndexByte(current, '/')
			if sep <= 0 {
				break
			}
			current = current[:sep]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(total, total)
	}

	type key struct {
		hash      uint64
		fileCount int
	}
	byKey := make(map[key][]string)
	for dir, dd := range accum {
		if dd.fileCount == 0 {
			continue
		}
		byKey[key{dd.hash, dd.fileCount}] = append(byKey[key{dd.hash, dd.fileCount}], dir)
	}

	const maxDirsPerBucket = 2000
	const minFileCount = 2

	var pairs []TreeDupPair
	skippedBuckets := 0
	for k, dirs := range byKey {
		if k.fileCount < minFileCount || len(dirs) < 2 {
			continue
		}
		if len(dirs) > maxDirsPerBucket {
			// Don't silently drop: too many identical dirs to pair (O(n²) guard).
			skippedBuckets++
			continue
		}
		sort.Strings(dirs)
		for a := 0; a < len(dirs); a++ {
			for b := a + 1; b < len(dirs); b++ {
				di, dj := dirs[a], dirs[b]
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

	if skippedBuckets > 0 && cfg != nil && !cfg.Quiet {
		fmt.Fprintf(os.Stderr,
			"  note: %d directory group(s) had >%d identical dirs and were skipped (pairing capped to avoid O(n²)); some tree dups may be unreported\n",
			skippedBuckets, maxDirsPerBucket)
	}

	pairs = removeSubPairsFast(pairs)

	// Verify every surviving pair (the accum hash only walks bounded depth).
	pairs, err = verifyPairsMtimeStore(pairs, fs, cfg, newDirStoreChecker(fs))
	if err != nil {
		return nil, err
	}

	sort.Slice(pairs, func(a, b int) bool { return pairs[a].TotalSize > pairs[b].TotalSize })
	return pairs, nil
}

// VerifyTreePairsByContent re-checks each tree pair by MD5. size+mtime can
// collide — especially across backup rotations where rsync -a preserves mtime —
// so the fast mtime-based tree pass is unsafe to act on. In -c mode this upgrades
// those pairs to content-verified before they can be offered for deletion; pairs
// whose files don't all hash-match are dropped. The cache memoizes the hashing
// (and is shared with the file-level MD5 pass, so files aren't read twice).
func VerifyTreePairsByContent(pairs []TreeDupPair, lookup DirLookup, cache *HashCache) []TreeDupPair {
	out := pairs[:0]
	for _, p := range pairs {
		if treePairContentEqual(p, lookup, cache) {
			p.Verified = true
			out = append(out, p)
		}
	}
	return out
}

func treePairContentEqual(p TreeDupPair, lookup DirLookup, cache *HashCache) bool {
	filesA := lookup(p.DirA)
	filesB := lookup(p.DirB)
	if len(filesA) == 0 || len(filesA) != len(filesB) {
		return false
	}
	prefixA := p.DirA + string(filepath.Separator)
	prefixB := p.DirB + string(filepath.Separator)
	bMap := make(map[string]ScannedFile, len(filesB))
	for _, f := range filesB {
		bMap[f.Path[len(prefixB):]] = f
	}
	for _, fa := range filesA {
		fb, ok := bMap[fa.Path[len(prefixA):]]
		if !ok || fa.Size != fb.Size {
			return false
		}
		ha, err := cache.Hash(fa)
		if err != nil {
			return false
		}
		hb, err := cache.Hash(fb)
		if err != nil || ha != hb {
			return false
		}
	}
	return true
}

// verifyTreePairMtimeStore is the store-backed verifyTreePairMtime: it pulls each
// dir's files with a prefix-range query instead of binary-searching a slice.
// verifyPairsMtimeStore keeps the pairs whose two dirs really match, sharing one
// dirStoreChecker across the whole batch so each directory is read from disk and
// counted in the store once, not once per pair it takes part in.
func verifyPairsMtimeStore(pairs []TreeDupPair, fs *FileStore, cfg *Config, chk *dirStoreChecker) ([]TreeDupPair, error) {
	verified := pairs[:0]
	for _, p := range pairs {
		ok, err := verifyTreePairMtimeStore(p.DirA, p.DirB, fs, cfg, chk)
		if err != nil {
			return nil, err
		}
		if ok {
			verified = append(verified, p)
		}
	}
	return verified, nil
}

func verifyTreePairMtimeStore(dirA, dirB string, fs *FileStore, cfg *Config, chk *dirStoreChecker) (bool, error) {
	if chk == nil {
		chk = newDirStoreChecker(fs)
	}
	// Soundness guard for filtered scans: when --min/max-size or --exclude/--include
	// is active, files removed by the filter were never stored, so the store's view
	// of a dir is incomplete and a "tree dup" claim would be unsound (two dirs can
	// match on the visible files yet differ in the filtered ones). Confirm only if
	// BOTH dirs are fully represented in the store. Fail safe (reject) on doubt.
	if cfg != nil && (cfg.MinSize > 0 || cfg.MaxSize > 0 || len(cfg.Rules) > 0) {
		if chk.incompleteDir(dirA) || chk.incompleteDir(dirB) {
			return false, nil
		}
	}

	// Count-first: reject mismatched pairs without materializing huge dir lists.
	na, err := chk.countStored(dirA)
	if err != nil {
		return false, err
	}
	if na == 0 {
		return false, nil
	}
	if nb, err := chk.countStored(dirB); err != nil {
		return false, err
	} else if na != nb {
		return false, nil
	}

	filesA, err := fs.FilesUnderDir(dirA)
	if err != nil {
		return false, err
	}
	filesB, err := fs.FilesUnderDir(dirB)
	if err != nil {
		return false, err
	}
	if len(filesA) == 0 || len(filesA) != len(filesB) {
		return false, nil
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
			return false, nil
		}
	}
	return true, nil
}
