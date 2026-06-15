package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// dirHasOutOfWindowFile reports whether dir contains (recursively) any regular
// file whose size falls outside [minSize, maxSize] (maxSize<=0 = no upper bound).
// Such a file is invisible to a size-filtered scan, so a tree-dup claim over dir
// would be unsound. Returns true on the first offender OR on any read error
// (fail safe: if identity can't be guaranteed, don't confirm the tree).
func dirHasOutOfWindowFile(dir string, minSize, maxSize int64) bool {
	found := false
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			found = true
			return filepath.SkipAll
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			found = true
			return filepath.SkipAll
		}
		sz := info.Size()
		if (minSize > 0 && sz < minSize) || (maxSize > 0 && sz > maxSize) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
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
	for k, dirs := range byKey {
		if k.fileCount < minFileCount || len(dirs) < 2 || len(dirs) > maxDirsPerBucket {
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

	pairs = removeSubPairsFast(pairs)

	// Verify every surviving pair (the accum hash only walks bounded depth).
	verified := pairs[:0]
	for _, p := range pairs {
		ok, err := verifyTreePairMtimeStore(p.DirA, p.DirB, fs, cfg)
		if err != nil {
			return nil, err
		}
		if ok {
			verified = append(verified, p)
		}
	}
	pairs = verified

	sort.Slice(pairs, func(a, b int) bool { return pairs[a].TotalSize > pairs[b].TotalSize })
	return pairs, nil
}

// verifyTreePairMtimeStore is the store-backed verifyTreePairMtime: it pulls each
// dir's files with a prefix-range query instead of binary-searching a slice.
func verifyTreePairMtimeStore(dirA, dirB string, fs *FileStore, cfg *Config) (bool, error) {
	// Soundness guard for size-filtered scans: if --min-size/--max-size is active,
	// files outside the window were never scanned, so the store's view of a dir is
	// incomplete and a "tree dup" claim would be unsound (two dirs can match on
	// their big files yet differ in small ones). Only confirm the pair if BOTH
	// dirs contain exclusively in-window files on disk — i.e. the store sees the
	// whole tree. Fail safe (reject) on any read error.
	if cfg != nil && (cfg.MinSize > 0 || cfg.MaxSize > 0) {
		if dirHasOutOfWindowFile(dirA, cfg.MinSize, cfg.MaxSize) ||
			dirHasOutOfWindowFile(dirB, cfg.MinSize, cfg.MaxSize) {
			return false, nil
		}
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
