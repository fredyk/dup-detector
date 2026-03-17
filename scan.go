package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

// ScannedFile holds metadata for a scanned file.
type ScannedFile struct {
	Path    string
	RelPath string // relative to scan root
	Size    int64
	ModTime int64 // Unix timestamp
	Source  int   // 0 = dir A, 1 = dir B
}

// Scan walks root and returns all files that pass the configured filters.
// absExcludes is a list of absolute directory paths to skip entirely.
func Scan(root string, cfg *Config, absExcludes []string) ([]ScannedFile, error) {
	var files []ScannedFile
	var count int64

	normExcludes := make([]string, len(absExcludes))
	for i, d := range absExcludes {
		normExcludes[i] = filepath.Clean(d)
	}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if cfg.Verbose {
				fmt.Fprintf(os.Stderr, "warning: %s: %v\n", path, walkErr)
			}
			return nil
		}

		// Skip the root itself
		if path == root {
			return nil
		}

		cleanPath := filepath.Clean(path)

		// Skip absolute excluded dirs (e.g. the other dir when one is a subdir of the other)
		for _, excl := range normExcludes {
			if cleanPath == excl || IsSubdir(excl, cleanPath) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		// Compute relative path for filter matching
		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		relPath = filepath.ToSlash(relPath)

		// Apply user-defined filter rules (rsync --exclude / --include)
		if ShouldExclude(relPath, cfg.Rules) {
			if d.IsDir() {
				if cfg.Verbose {
					fmt.Fprintf(os.Stderr, "  excluded dir: %s\n", relPath)
				}
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			if !cfg.Recursive {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip symlinks
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		size := info.Size()

		if cfg.MinSize > 0 && size < cfg.MinSize {
			return nil
		}
		if cfg.MaxSize > 0 && size > cfg.MaxSize {
			return nil
		}

		files = append(files, ScannedFile{
			Path:    path,
			RelPath: relPath,
			Size:    size,
			ModTime: info.ModTime().Unix(),
		})

		n := atomic.AddInt64(&count, 1)
		if cfg.Progress && n%500 == 0 {
			fmt.Fprintf(os.Stderr, "\r  %d files scanned...", n)
		}

		return nil
	})

	if cfg.Progress && count > 0 {
		fmt.Fprintf(os.Stderr, "\r  %d files scanned    \n", count)
	}

	return files, err
}
