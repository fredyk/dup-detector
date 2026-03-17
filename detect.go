package main

import (
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"sync"
)

// DupGroup is a set of files with identical content.
type DupGroup struct {
	Size  int64
	Files []ScannedFile
}

// WastedBytes returns the space that could be freed by keeping only one copy.
func (g DupGroup) WastedBytes() int64 {
	return g.Size * int64(len(g.Files)-1)
}

// GroupBySize partitions files by size. Only sizes with ≥2 candidates are kept.
func GroupBySize(files []ScannedFile) map[int64][]ScannedFile {
	raw := make(map[int64][]ScannedFile, len(files)/4)
	for _, f := range files {
		raw[f.Size] = append(raw[f.Size], f)
	}
	out := make(map[int64][]ScannedFile, len(raw)/2)
	for size, group := range raw {
		if len(group) >= 2 {
			out[size] = group
		}
	}
	return out
}

// SizesSortedDesc returns the sizes present in a GroupBySize map, largest first.
func SizesSortedDesc(bySize map[int64][]ScannedFile) []int64 {
	sizes := make([]int64, 0, len(bySize))
	for s := range bySize {
		sizes = append(sizes, s)
	}
	sort.Slice(sizes, func(i, j int) bool { return sizes[i] > sizes[j] })
	return sizes
}

// MtimeDups finds duplicates using size+mtime (no I/O, instant).
// In two-dir mode filesB must be non-nil; cross-dir only.
func MtimeDups(filesA, filesB []ScannedFile) []DupGroup {
	twoDir := len(filesB) > 0
	for i := range filesA {
		filesA[i].Source = 0
	}
	for i := range filesB {
		filesB[i].Source = 1
	}
	all := make([]ScannedFile, 0, len(filesA)+len(filesB))
	all = append(all, filesA...)
	all = append(all, filesB...)

	bySize := GroupBySize(all)
	var groups []DupGroup
	for size, candidates := range bySize {
		if twoDir && !hasBothSides(candidates) {
			continue
		}
		for _, g := range groupByMtime(candidates, size) {
			if len(g.Files) < 2 {
				continue
			}
			if twoDir && !hasBothSides(g.Files) {
				continue
			}
			groups = append(groups, g)
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Size > groups[j].Size })
	return groups
}

// ChecksumDups finds duplicates using MD5, processing size groups largest-first.
//
//   - skip: file paths to skip (already deleted)
//   - workers: parallel MD5 goroutines (0 = runtime.NumCPU)
//   - onProgress: called after each size group with (bytesProcessed, totalBytes)
//   - onBatch: called after each size group with newly found groups; return false to stop early
func ChecksumDups(filesA, filesB []ScannedFile, twoDir bool, skip map[string]bool,
	workers int, onProgress func(done, total int64), onBatch func([]DupGroup) bool) error {

	if workers <= 0 {
		workers = runtime.NumCPU()
	}

	all := make([]ScannedFile, 0, len(filesA)+len(filesB))
	for _, f := range filesA {
		if !skip[f.Path] {
			f.Source = 0
			all = append(all, f)
		}
	}
	for _, f := range filesB {
		if !skip[f.Path] {
			f.Source = 1
			all = append(all, f)
		}
	}

	bySize := GroupBySize(all)
	sizes := SizesSortedDesc(bySize)

	// Total bytes to process (for progress)
	var totalBytes int64
	for size, cands := range bySize {
		totalBytes += size * int64(len(cands))
	}
	var doneBytes int64

	for _, size := range sizes {
		candidates := bySize[size]
		if twoDir && !hasBothSides(candidates) {
			doneBytes += size * int64(len(candidates))
			if onProgress != nil {
				onProgress(doneBytes, totalBytes)
			}
			continue
		}

		newGroups, err := checksumGroup(candidates, size, twoDir, workers)
		if err != nil {
			return err
		}
		doneBytes += size * int64(len(candidates))
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

// checksumGroup computes MD5 for all candidates and returns dup groups.
func checksumGroup(candidates []ScannedFile, size int64, twoDir bool, workers int) ([]DupGroup, error) {
	type hashResult struct {
		idx  int
		hash string
		err  error
	}

	results := make([]hashResult, len(candidates))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i, f := range candidates {
		wg.Add(1)
		i, f := i, f
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			hash, err := md5File(f.Path)
			results[i] = hashResult{i, hash, err}
		}()
	}
	wg.Wait()

	byHash := make(map[string][]ScannedFile)
	for i, r := range results {
		if r.err != nil || r.hash == "" {
			continue
		}
		byHash[r.hash] = append(byHash[r.hash], candidates[i])
	}

	var groups []DupGroup
	for _, fs := range byHash {
		if len(fs) < 2 {
			continue
		}
		if twoDir && !hasBothSides(fs) {
			continue
		}
		groups = append(groups, DupGroup{Size: size, Files: fs})
	}
	return groups, nil
}

// DetectDups is the original all-at-once API (kept for compatibility).
func DetectDups(filesA, filesB []ScannedFile, cfg *Config) ([]DupGroup, error) {
	if !cfg.Checksum {
		groups := MtimeDups(filesA, filesB)
		return groups, nil
	}
	twoDir := len(filesB) > 0
	var all []DupGroup
	err := ChecksumDups(filesA, filesB, twoDir, nil, cfg.Workers,
		func(done, total int64) {
			if cfg.Progress {
				pct := int(100 * done / (total + 1))
				fmt.Fprintf(os.Stderr, "\r  MD5: %d%%  (%s / %s)  ",
					pct, FormatSize(done), FormatSize(total))
			}
		},
		func(newGroups []DupGroup) bool {
			all = append(all, newGroups...)
			return true
		},
	)
	if cfg.Progress {
		fmt.Fprintln(os.Stderr)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Size > all[j].Size })
	return all, err
}

func hasBothSides(files []ScannedFile) bool {
	var hasA, hasB bool
	for _, f := range files {
		if f.Source == 0 {
			hasA = true
		} else {
			hasB = true
		}
		if hasA && hasB {
			return true
		}
	}
	return false
}

func groupByMtime(files []ScannedFile, size int64) []DupGroup {
	byMtime := make(map[int64][]ScannedFile)
	for _, f := range files {
		byMtime[f.ModTime] = append(byMtime[f.ModTime], f)
	}
	groups := make([]DupGroup, 0, len(byMtime))
	for _, fs := range byMtime {
		groups = append(groups, DupGroup{Size: size, Files: fs})
	}
	return groups
}

func md5File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
