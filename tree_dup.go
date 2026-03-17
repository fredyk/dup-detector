package main

import (
	"path/filepath"
	"sort"
	"strings"
)

// TreeDupPair represents two directory trees with byte-identical content.
// Every file in DirA has an exact duplicate somewhere in DirB, and vice versa.
// Neither tree has any file without a counterpart in the other.
type TreeDupPair struct {
	DirA      string
	DirB      string
	FileCount int   // number of files in each tree
	TotalSize int64 // total bytes (one copy)
}

// FindTreeDups identifies directory trees that are completely duplicated.
// allFiles must be sorted by Path (call SortFilesByPath first).
func FindTreeDups(groups []DupGroup, allFiles []ScannedFile) []TreeDupPair {
	if len(groups) == 0 || len(allFiles) == 0 {
		return nil
	}

	fileToDups := buildDupIndex(groups)

	// Generate candidate directory pairs by walking up the ancestor chain
	// of each file pair in every dup group.
	type dirPair struct{ a, b string }
	seen := make(map[dirPair]bool)

	for _, g := range groups {
		for i := 0; i < len(g.Files); i++ {
			for j := i + 1; j < len(g.Files); j++ {
				da := filepath.Dir(g.Files[i].Path)
				db := filepath.Dir(g.Files[j].Path)
				for depth := 0; depth < 12; depth++ {
					if da == "." || da == "/" || db == "." || db == "/" || da == db {
						break
					}
					a, b := da, db
					if a > b {
						a, b = b, a
					}
					seen[dirPair{a, b}] = true
					da = filepath.Dir(da)
					db = filepath.Dir(db)
				}
			}
		}
	}

	var pairs []TreeDupPair
	for pair := range seen {
		filesA := filesUnderDir(pair.a, allFiles)
		filesB := filesUnderDir(pair.b, allFiles)

		// Same number of files, every file covered on both sides
		if len(filesA) == 0 || len(filesB) == 0 || len(filesA) != len(filesB) {
			continue
		}
		if !allCoveredBy(filesA, pair.b, fileToDups) {
			continue
		}
		if !allCoveredBy(filesB, pair.a, fileToDups) {
			continue
		}

		var totalSize int64
		for _, f := range filesA {
			totalSize += f.Size
		}
		pairs = append(pairs, TreeDupPair{
			DirA:      pair.a,
			DirB:      pair.b,
			FileCount: len(filesA),
			TotalSize: totalSize,
		})
	}

	// Drop sub-pairs: if (/a, /x) is already a tree dup, don't also report (/a/b, /x/b)
	pairs = removeSubPairs(pairs)

	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].TotalSize > pairs[j].TotalSize
	})
	return pairs
}

// SortFilesByPath sorts a slice of ScannedFile by path for binary-search lookups.
func SortFilesByPath(files []ScannedFile) {
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
}

// buildDupIndex returns a map: filePath → []duplicatePaths (all other files in same group).
func buildDupIndex(groups []DupGroup) map[string][]string {
	index := make(map[string][]string)
	for _, g := range groups {
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
			index[f.Path] = dups
		}
	}
	return index
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

// allCoveredBy checks that every file in files has at least one duplicate under dir.
func allCoveredBy(files []ScannedFile, dir string, fileToDups map[string][]string) bool {
	prefix := filepath.Clean(dir) + string(filepath.Separator)
	for _, f := range files {
		found := false
		for _, dup := range fileToDups[f.Path] {
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

// removeSubPairs drops pairs whose directories are sub-trees of another pair's directories.
func removeSubPairs(pairs []TreeDupPair) []TreeDupPair {
	result := make([]TreeDupPair, 0, len(pairs))
	for i, p := range pairs {
		dominated := false
		for j, o := range pairs {
			if i == j {
				continue
			}
			// p is dominated by o if both of p's dirs sit inside o's respective dirs
			case1 := IsSubdir(o.DirA, p.DirA) && IsSubdir(o.DirB, p.DirB)
			case2 := IsSubdir(o.DirB, p.DirA) && IsSubdir(o.DirA, p.DirB)
			if case1 || case2 {
				dominated = true
				break
			}
		}
		if !dominated {
			result = append(result, p)
		}
	}
	return result
}
