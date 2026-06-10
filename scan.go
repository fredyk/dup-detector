package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// ScannedFile holds metadata for a scanned file.
type ScannedFile struct {
	Path    string
	RelPath string // relative to scan root
	Size    int64
	ModTime int64 // Unix timestamp
	Inode   int64 // inode number (0 if unavailable); used to validate the hash cache
	Source  int   // 0 = dir A, 1 = dir B
}

// Scan walks root and returns all files that pass the configured filters.
// absExcludes is a list of absolute directory paths to skip entirely.
// seenInodes (optional, pass nil to start fresh) is a shared (Dev,Ino) map
// used to skip hardlinks pointing at an already-seen inode — deleting those
// wouldn't reclaim space. Pass the same map across multiple Scan calls to
// also catch cross-directory hardlinks.
// onFile, if non-nil, is invoked for every accepted regular file as it is
// discovered during the walk. It enables progressive work (e.g. streaming MD5
// hashing) to overlap with the directory traversal.
func Scan(root string, cfg *Config, absExcludes []string, seenInodes map[[2]uint64]string, onFile func(ScannedFile)) ([]ScannedFile, error) {
	var files []ScannedFile
	var count int
	var hardlinkCount int

	if seenInodes == nil {
		seenInodes = make(map[[2]uint64]string)
	}

	normExcludes := make([]string, len(absExcludes))
	for i, d := range absExcludes {
		normExcludes[i] = filepath.Clean(d)
	}

	// Capture root's filesystem device for --one-file-system checks.
	var rootDev uint64
	var rootDevOk bool
	if cfg.OneFileSystem {
		if rootInfo, err := os.Stat(root); err == nil {
			if key, ok := inodeKey(rootInfo); ok {
				rootDev = key[0]
				rootDevOk = true
			}
		}
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
			if !cfg.Recursive && path != root {
				return filepath.SkipDir
			}
			// --one-file-system: skip subdirs on a different filesystem (nested mounts).
			if rootDevOk && path != root {
				if info, err := d.Info(); err == nil {
					if key, ok := inodeKey(info); ok && key[0] != rootDev {
						if cfg.Verbose {
							fmt.Fprintf(os.Stderr, "  skipping nested mount: %s\n", path)
						}
						return filepath.SkipDir
					}
				}
			}
			return nil
		}

		// Only regular files. Rules out symlinks, sockets, FIFOs, device
		// nodes and named pipes — opening a FIFO or socket with md5File
		// would block the scan indefinitely.
		if !d.Type().IsRegular() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		// Skip hardlinks to inodes already seen — deleting these reclaims no bytes.
		var inode int64
		if key, ok := inodeKey(info); ok {
			inode = int64(key[1])
			if prev, exists := seenInodes[key]; exists {
				hardlinkCount++
				if cfg.Verbose {
					fmt.Fprintf(os.Stderr, "  hardlink: %s → %s (skipped)\n", path, prev)
				}
				return nil
			}
			seenInodes[key] = path
		}

		size := info.Size()

		if cfg.MinSize > 0 && size < cfg.MinSize {
			return nil
		}
		if cfg.MaxSize > 0 && size > cfg.MaxSize {
			return nil
		}

		sf := ScannedFile{
			Path:    path,
			RelPath: relPath,
			Size:    size,
			ModTime: info.ModTime().Unix(),
			Inode:   inode,
		}
		files = append(files, sf)
		if onFile != nil {
			onFile(sf)
		}

		count++
		if cfg.Progress && count%500 == 0 {
			fmt.Fprintf(os.Stderr, "\r  %d files scanned...", count)
		}

		return nil
	})

	if cfg.Progress && count > 0 {
		fmt.Fprintf(os.Stderr, "\r  %d files scanned    \n", count)
	}
	if (cfg.Progress || cfg.Verbose) && hardlinkCount > 0 {
		fmt.Fprintf(os.Stderr, "  skipped %d hardlink(s) to previously-seen inodes\n", hardlinkCount)
	}

	return files, err
}
