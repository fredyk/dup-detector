package main

import (
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"sort"
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

// DetectDups finds duplicate files.
//
//   - Single-dir mode: filesB is nil → finds dups within filesA.
//   - Two-dir mode: finds files whose content exists in both A and B.
//     A group must have at least one file from each dir.
func DetectDups(filesA, filesB []ScannedFile, cfg *Config) ([]DupGroup, error) {
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

	// Pre-filter: group by size (cheap)
	bySize := make(map[int64][]ScannedFile, len(all))
	for _, f := range all {
		bySize[f.Size] = append(bySize[f.Size], f)
	}

	var groups []DupGroup

	for size, candidates := range bySize {
		if len(candidates) < 2 {
			continue
		}
		// In two-dir mode, candidates must have files from both sides
		if twoDir && !hasBothSides(candidates) {
			continue
		}

		var subGroups []DupGroup
		var err error

		if cfg.Checksum {
			subGroups, err = groupByMD5(candidates, size, cfg)
			if err != nil {
				return nil, err
			}
		} else {
			subGroups = groupByMtime(candidates, size)
		}

		for _, g := range subGroups {
			if len(g.Files) < 2 {
				continue
			}
			if twoDir && !hasBothSides(g.Files) {
				continue
			}
			groups = append(groups, g)
		}
	}

	// Sort largest first
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Size > groups[j].Size
	})

	return groups, nil
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

func groupByMD5(files []ScannedFile, size int64, cfg *Config) ([]DupGroup, error) {
	byHash := make(map[string][]ScannedFile)
	total := len(files)

	for i, f := range files {
		if cfg.Progress {
			fmt.Fprintf(os.Stderr, "\r  computing MD5: %d/%d", i+1, total)
		}
		hash, err := md5File(f.Path)
		if err != nil {
			if cfg.Verbose {
				fmt.Fprintf(os.Stderr, "warning: MD5 failed for %s: %v\n", f.Path, err)
			}
			continue
		}
		byHash[hash] = append(byHash[hash], f)
	}

	if cfg.Progress && total > 0 {
		fmt.Fprintf(os.Stderr, "\r                                    \r")
	}

	groups := make([]DupGroup, 0, len(byHash))
	for _, fs := range byHash {
		groups = append(groups, DupGroup{Size: size, Files: fs})
	}
	return groups, nil
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
