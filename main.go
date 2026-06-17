package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// Config holds all runtime options.
type Config struct {
	// rsync-compatible flags
	Checksum      bool
	Recursive     bool
	Verbose       bool
	Quiet         bool
	DryRun        bool
	Progress      bool
	Excludes      []string
	ExcludeFrom   string
	Includes      []string
	IncludeFrom   string
	MinSizeStr    string
	MaxSizeStr    string
	OneFileSystem bool

	// Hash cache (only used in -c mode)
	NoCache   bool
	Rehash    bool
	CachePath string

	// Output
	Format string

	// Performance
	Workers int

	// Parsed values
	MinSize int64
	MaxSize int64
	Rules   []FilterRule
}

var cfg Config

// defaultExcludes are directory/file name patterns always skipped, regardless
// of flags. `.flexiblefs` marks FlexibleFS metadata dirs whose contents are
// internal bookkeeping, never user duplicates. A user can still re-include one
// explicitly with `--include .flexiblefs` (last match wins, rsync semantics).
var defaultExcludes = []string{".flexiblefs"}

var rootCmd = &cobra.Command{
	Use:   "dup-detector [OPTIONS] DIR_A [DIR_B]",
	Short: "Detect duplicate files between or within directories",
	Long: `Detect duplicate files.

If only DIR_A is provided, finds duplicates within that directory.
If DIR_A and DIR_B are provided, finds files whose content exists in both.

Directories are interchangeable - order does not affect the result.

If one directory is a subdirectory of the other, it is automatically excluded
from the parent scan to avoid false positives.

Comparison modes:
  default   size + modification time (fast)
  -c        size + MD5 checksum (slower, collision-proof)

Output formats:  columns (default), json, csv, simple

After detection, you will be prompted to interactively delete duplicates
(largest files first). Use -n/--dry-run to skip the deletion prompt.`,
	Args:          cobra.RangeArgs(1, 2),
	RunE:          run,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	f := rootCmd.Flags()
	f.BoolVarP(&cfg.Checksum, "checksum", "c", false,
		"compare by size+MD5 instead of size+mtime")
	f.BoolVarP(&cfg.Recursive, "recursive", "r", true,
		"recurse into subdirectories (default true; use --recursive=false to disable)")
	f.BoolVarP(&cfg.Verbose, "verbose", "v", false,
		"increase verbosity")
	f.BoolVarP(&cfg.Quiet, "quiet", "q", false,
		"suppress status output (duplicates still printed to stdout)")
	f.BoolVarP(&cfg.DryRun, "dry-run", "n", false,
		"scan and report only; skip deletion prompt")
	f.BoolVar(&cfg.Progress, "progress", false,
		"show progress during scan")
	f.StringArrayVar(&cfg.Excludes, "exclude", nil,
		"exclude files/dirs matching PATTERN (can be repeated)")
	f.StringVar(&cfg.ExcludeFrom, "exclude-from", "",
		"read exclude patterns from FILE")
	f.StringArrayVar(&cfg.Includes, "include", nil,
		"include files matching PATTERN even if excluded (can be repeated)")
	f.StringVar(&cfg.IncludeFrom, "include-from", "",
		"read include patterns from FILE")
	f.StringVar(&cfg.MinSizeStr, "min-size", "",
		"skip files smaller than SIZE (e.g. 1k, 10M, 1G)")
	f.StringVar(&cfg.MaxSizeStr, "max-size", "",
		"skip files larger than SIZE")
	f.StringVar(&cfg.Format, "format", "columns",
		"output format: columns, json, csv, simple")
	f.IntVar(&cfg.Workers, "workers", 0,
		"parallel MD5 workers (default: number of CPUs)")
	f.BoolVar(&cfg.OneFileSystem, "one-file-system", false,
		"don't cross filesystem boundaries (skip nested mounts)")
	f.BoolVar(&cfg.NoCache, "no-cache", false,
		"disable the on-disk MD5 cache (always re-read file contents)")
	f.BoolVar(&cfg.Rehash, "rehash", false,
		"ignore cached MD5s and recompute them (refreshes the cache)")
	f.StringVar(&cfg.CachePath, "cache-path", "",
		"path to the MD5 cache DB (default: ~/.cache/dup-detector/hashes.db)")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(_ *cobra.Command, args []string) error {
	// Parse size thresholds
	var err error
	if cfg.MinSize, err = ParseSize(cfg.MinSizeStr); err != nil {
		return fmt.Errorf("--min-size: %w", err)
	}
	if cfg.MaxSize, err = ParseSize(cfg.MaxSizeStr); err != nil {
		return fmt.Errorf("--max-size: %w", err)
	}

	// Build filter rules.
	// Built-in excludes always come first so they form the base; a later
	// --include can still override them (last match wins, rsync semantics).
	for _, pat := range defaultExcludes {
		cfg.Rules = append(cfg.Rules, FilterRule{Pattern: pat, Exclude: true})
	}
	// Includes are prepended so an explicit --include can override a later --exclude.
	// Within each group, order is preserved (last match wins, rsync semantics).
	for _, pat := range cfg.Includes {
		cfg.Rules = append(cfg.Rules, FilterRule{Pattern: pat, Exclude: false})
	}
	if cfg.IncludeFrom != "" {
		rules, err := LoadRulesFromFile(cfg.IncludeFrom, false)
		if err != nil {
			return fmt.Errorf("--include-from: %w", err)
		}
		cfg.Rules = append(cfg.Rules, rules...)
	}
	for _, pat := range cfg.Excludes {
		cfg.Rules = append(cfg.Rules, FilterRule{Pattern: pat, Exclude: true})
	}
	if cfg.ExcludeFrom != "" {
		rules, err := LoadRulesFromFile(cfg.ExcludeFrom, true)
		if err != nil {
			return fmt.Errorf("--exclude-from: %w", err)
		}
		cfg.Rules = append(cfg.Rules, rules...)
	}

	// Resolve directories
	dirA, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolving %s: %w", args[0], err)
	}

	twoDir := len(args) == 2
	var dirB string
	if twoDir {
		dirB, err = filepath.Abs(args[1])
		if err != nil {
			return fmt.Errorf("resolving %s: %w", args[1], err)
		}
		if dirA == dirB {
			twoDir = false
			if !cfg.Quiet {
				fmt.Fprintln(os.Stderr, "note: DIR_A and DIR_B are the same; running in single-dir mode")
			}
		}
	}

	// Detect subdir relationships and set up per-scan exclusions
	var excludeFromA, excludeFromB []string
	if twoDir {
		switch {
		case IsSubdir(dirA, dirB):
			// dirB is inside dirA → exclude dirB from A scan
			excludeFromA = append(excludeFromA, dirB)
			if !cfg.Quiet {
				fmt.Fprintf(os.Stderr, "note: %s is inside %s; excluding it from A scan\n",
					dirB, dirA)
			}
		case IsSubdir(dirB, dirA):
			// dirA is inside dirB → exclude dirA from B scan
			excludeFromB = append(excludeFromB, dirA)
			if !cfg.Quiet {
				fmt.Fprintf(os.Stderr, "note: %s is inside %s; excluding it from B scan\n",
					dirA, dirB)
			}
		}
	}

	// Scan
	status := func(format string, a ...any) {
		if !cfg.Quiet {
			fmt.Fprintf(os.Stderr, format, a...)
		}
	}

	// Shared inode map: also catches hardlinks pointing to the same inode
	// across DIR_A and DIR_B when they live on the same filesystem.
	seenInodes := make(map[[2]uint64]struct{})

	// In -c mode, open the MD5 cache up front so it can serve the progressive
	// hasher during the walk. A nil cache (disabled / failed to open) just
	// means uncached reads.
	var cache *HashCache
	var cachePath string
	if cfg.Checksum && !cfg.NoCache {
		cachePath = cfg.CachePath
		if cachePath == "" {
			cachePath = DefaultCachePath()
		}
		if cachePath == "" {
			status("warning: cannot locate cache dir; running without MD5 cache\n")
		} else if c, cerr := OpenHashCache(cachePath, cfg.Rehash); cerr != nil {
			status("warning: MD5 cache disabled (%v)\n", cerr)
		} else {
			cache = c
			defer func() {
				if !cfg.Quiet {
					hits, misses := cache.Stats()
					fmt.Fprintf(os.Stderr, "  MD5 cache: %d reused, %d computed (%s)\n",
						hits, misses, cachePath)
				}
				if cerr := cache.Close(); cerr != nil {
					fmt.Fprintf(os.Stderr, "warning: closing MD5 cache: %v\n", cerr)
				}
			}()
		}
	}

	// Stream the walk into an on-disk SQLite store instead of holding the full
	// file list in RAM. Peak memory becomes O(working set) — one size group /
	// the per-directory accumulator — not O(total files). The store lives next
	// to the MD5 cache (a real disk), never /tmp (often tmpfs = RAM).
	storeDir := filepath.Dir(DefaultCachePath())
	if storeDir == "" || storeDir == "." {
		storeDir = os.TempDir()
	}
	storePath := filepath.Join(storeDir, fmt.Sprintf("dup-detector-scan-%d.db", os.Getpid()))
	CleanStaleStores(storeDir) // sweep DBs orphaned by earlier killed runs
	store, err := NewFileStore(storePath)
	if err != nil {
		return fmt.Errorf("creating scan store: %w", err)
	}
	defer store.Close()

	var nA, nB int
	status("Scanning %s ...\n", dirA)
	if err := ScanToStore(store, dirA, &cfg, excludeFromA, seenInodes, 0,
		func(ScannedFile) { nA++ }); err != nil {
		return fmt.Errorf("scanning %s: %w", dirA, err)
	}
	if twoDir {
		status("Scanning %s ...\n", dirB)
		if err := ScanToStore(store, dirB, &cfg, excludeFromB, seenInodes, 1,
			func(ScannedFile) { nB++ }); err != nil {
			return fmt.Errorf("scanning %s: %w", dirB, err)
		}
		status("Found %d files in A, %d files in B\n", nA, nB)
	} else {
		status("Found %d files\n", nA)
	}

	if err := store.Finalize(); err != nil {
		return fmt.Errorf("indexing scan store: %w", err)
	}

	// Files-under-dir resolver backed by the store (indexed prefix range).
	lookup := func(dir string) []ScannedFile {
		files, _ := store.FilesUnderDir(dir)
		return files
	}

	// ── Phase 1: fast tree detection via directory hashing ───────────────────
	status("Detecting duplicates (fast pass)...\n")

	treeState := NewTreeDupState()
	treeState.Workers = cfg.Workers
	treeState.CountUnder = func(d string) int { n, _ := store.CountUnderDir(d); return n }
	var allGroups []DupGroup

	var hashProgressFn func(done, total int)
	if cfg.Progress {
		hashProgressFn = func(done, total int) {
			fmt.Fprintf(os.Stderr, "\r  hashing dirs: %d / %d files  ", done, total)
		}
	}
	earlyTrees, err := FindTreeDupsByHashStore(store, &cfg, hashProgressFn)
	if err != nil {
		return fmt.Errorf("tree detection: %w", err)
	}
	if cfg.Progress {
		fmt.Fprintln(os.Stderr)
	}
	if cfg.Checksum {
		// Upgrade the fast mtime-based tree pairs to content-verified before they
		// can ever be offered for deletion (size+mtime collide in backups).
		earlyTrees = VerifyTreePairsByContent(earlyTrees, lookup, cache)
	}
	treeState.Confirmed = append(treeState.Confirmed, earlyTrees...)

	// ── Phase 2: MD5 (only if -c flag set) ───────────────────────────────────
	if cfg.Checksum {
		status("Detecting duplicates (MD5 pass, largest first)...\n")
		err = ChecksumDupsStore(store, twoDir, nil, cfg.Workers, cache,
			func(done, total int64) {
				if cfg.Progress {
					pct := int(100 * done / (total + 1))
					fmt.Fprintf(os.Stderr, "\r  MD5: %d%%  (%s / %s)  ",
						pct, FormatSize(done), FormatSize(total))
				}
			},
			func(newGroups []DupGroup) bool {
				allGroups = append(allGroups, newGroups...)
				// Accumulate newly-confirmed tree dups silently; offering
				// happens once at the end.
				treeState.AddGroups(newGroups, lookup, true)
				return true
			},
		)
		if cfg.Progress {
			fmt.Fprintln(os.Stderr)
		}
		if err != nil {
			return fmt.Errorf("detecting duplicates: %w", err)
		}
	} else {
		groups, gerr := MtimeDupsStore(store, twoDir)
		if gerr != nil {
			return fmt.Errorf("detecting duplicates: %w", gerr)
		}
		allGroups = groups
	}

	// ── Summary and output ────────────────────────────────────────────────────
	finalTrees := treeState.Confirmed

	if len(allGroups) == 0 && len(finalTrees) == 0 {
		status("No duplicates found.\n")
		return nil
	}

	var totalWasted int64
	for _, g := range allGroups {
		totalWasted += g.WastedBytes()
	}
	status("Found %d tree duplicate(s), %d file-level group(s), %s reclaimable\n\n",
		len(finalTrees), len(allGroups), FormatSize(totalWasted))

	// Print results to stdout
	if len(finalTrees) > 0 {
		if err := PrintTreeDups(finalTrees, cfg.Format, os.Stdout); err != nil {
			return err
		}
	}
	if len(allGroups) > 0 {
		if err := PrintGroups(allGroups, cfg.Format, os.Stdout); err != nil {
			return err
		}
	}

	if !cfg.DryRun {
		if err := InteractiveDelete(treeState.Confirmed, allGroups, lookup, &cfg); err != nil {
			return err
		}
	}

	return nil
}
