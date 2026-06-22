package main

import (
	"runtime"
	"sort"
)

// MtimeDupsStore is the store-backed MtimeDups: size+mtime grouping, no I/O.
// Candidates are pulled per size-collision group instead of from an in-RAM slice.
func MtimeDupsStore(fs *FileStore, twoDir bool) ([]DupGroup, error) {
	sizes, err := fs.CollisionSizes()
	if err != nil {
		return nil, err
	}
	var groups []DupGroup
	for _, size := range sizes {
		candidates, err := fs.FilesWithSize(size)
		if err != nil {
			return nil, err
		}
		if twoDir && !spansMultipleSources(candidates) {
			continue
		}
		for _, g := range groupByMtime(candidates, size) {
			if len(g.Files) < 2 {
				continue
			}
			if twoDir && !spansMultipleSources(g.Files) {
				continue
			}
			groups = append(groups, g)
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Size > groups[j].Size })
	return groups, nil
}

// ChecksumDupsStore is the store-backed ChecksumDups: MD5 per size-collision
// group, largest-first, reusing checksumGroup and the HashCache. Peak RAM is one
// size group at a time, not the whole file list.
func ChecksumDupsStore(fs *FileStore, twoDir bool, skip map[string]bool, workers int,
	cache *HashCache, onProgress func(done, total int64), onBatch func([]DupGroup) bool) error {

	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	scs, err := fs.CollisionSizeCounts() // largest-first
	if err != nil {
		return err
	}
	var totalBytes int64
	for _, sc := range scs {
		totalBytes += sc.Size * int64(sc.Count)
	}
	var doneBytes int64

	for _, sc := range scs {
		size := sc.Size
		candidates, err := fs.FilesWithSize(size)
		if err != nil {
			return err
		}
		if skip != nil {
			filtered := candidates[:0]
			for _, f := range candidates {
				if !skip[f.Path] {
					filtered = append(filtered, f)
				}
			}
			candidates = filtered
		}

		doneBytes += size * int64(sc.Count)
		if twoDir && !spansMultipleSources(candidates) {
			if onProgress != nil {
				onProgress(doneBytes, totalBytes)
			}
			continue
		}

		newGroups, err := checksumGroup(candidates, size, twoDir, workers, cache)
		if err != nil {
			return err
		}
		if onProgress != nil {
			onProgress(doneBytes, totalBytes)
		}
		if len(newGroups) > 0 && onBatch != nil {
			if !onBatch(newGroups) {
				return nil
			}
		}
	}
	return nil
}
