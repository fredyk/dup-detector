package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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

	// Roots beyond the 1-2 positional args. Repeatable; avoids error-prone extra
	// positional arguments. The full root set = positional args + these.
	AdditionalRoots []string
	// WithinRoot: with >=2 roots, also report duplicates internal to a single
	// root. Default (false) reports only duplicates that bridge >=2 roots.
	WithinRoot bool

	// Hash cache (only used in -c mode)
	NoCache      bool
	Rehash       bool
	CachePath    string
	CacheMaxAge  string

	// Output
	Format string

	// Performance
	Workers int

	// Parsed values
	MinSize        int64
	MaxSize        int64
	Rules          []FilterRule
	CacheMaxAgeDur time.Duration
}

var cfg Config

// defaultExcludes are directory/file name patterns always skipped, regardless
// of flags. `.flexiblefs` marks FlexibleFS metadata dirs whose contents are
// internal bookkeeping, never user duplicates. A user can still re-include one
// explicitly with `--include .flexiblefs` (last match wins, rsync semantics).
var defaultExcludes = []string{".flexiblefs"}

var rootCmd = &cobra.Command{
	Use:   "dup-detector [OPTIONS] DIR_A [DIR_B] [--additional-root DIR]...",
	Short: "Detect duplicate files between or within directories",
	Long: `Detect duplicate files.

If only DIR_A is provided, finds duplicates within that directory.
If DIR_A and DIR_B are provided, finds files whose content exists in both.

To analyze MORE than two roots, add --additional-root DIR (repeatable) instead
of extra positional args. With >=2 roots only cross-root duplicates are reported
by default (use --within-root to also include duplicates internal to one root).
The list of roots being analyzed is printed at startup.

Directories are interchangeable - order does not affect the result.

If one root is a subdirectory of another, it is automatically excluded
from the outer scan to avoid double-counting.

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
	f.StringVar(&cfg.CacheMaxAge, "cache-max-age", "",
		"re-hash files cached longer than DURATION (e.g. 72h, 14d); 0 or empty = trust cache forever (default)")
	f.StringArrayVar(&cfg.AdditionalRoots, "additional-root", nil,
		"additional root directory to analyze (repeatable; use instead of extra positional args)")
	f.BoolVar(&cfg.WithinRoot, "within-root", false,
		"with >=2 roots, also report duplicates within a single root (default: only cross-root)")
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

	// Parse --cache-max-age duration
	if cfg.CacheMaxAge != "" && cfg.CacheMaxAge != "0" {
		dur, derr := time.ParseDuration(cfg.CacheMaxAge)
		if derr != nil {
			// Try suffixes: "d" for days
			if strings.HasSuffix(cfg.CacheMaxAge, "d") {
				n, nerr := strconv.Atoi(strings.TrimSuffix(cfg.CacheMaxAge, "d"))
				if nerr == nil {
					dur = time.Duration(n) * 24 * time.Hour
					derr = nil
				}
			}
			if derr != nil {
				return fmt.Errorf("--cache-max-age: invalid duration %q: %w", cfg.CacheMaxAge, derr)
			}
		}
		cfg.CacheMaxAgeDur = dur
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

	// Resolve the set of roots: positional args (1-2) + --additional-root
	// (repeatable, to avoid error-prone extra positional args). Normalize to
	// absolute and dedupe.
	rawRoots := append(append([]string{}, args...), cfg.AdditionalRoots...)
	var roots []string
	seenRoot := map[string]bool{}
	for _, r := range rawRoots {
		abs, aerr := filepath.Abs(r)
		if aerr != nil {
			return fmt.Errorf("resolving %s: %w", r, aerr)
		}
		if seenRoot[abs] {
			if !cfg.Quiet {
				fmt.Fprintf(os.Stderr, "note: duplicate root %s ignored\n", abs)
			}
			continue
		}
		seenRoot[abs] = true
		roots = append(roots, abs)
	}
	if len(roots) == 0 {
		return fmt.Errorf("no root directories to analyze")
	}

	// Per-root scan exclusions: when root j is nested inside root i, exclude j
	// from i's scan so each file is attributed to exactly one root/source (the
	// innermost), never double-counted under two sources.
	rootExcludes := make([][]string, len(roots))
	for i := range roots {
		for j := range roots {
			if i != j && IsSubdir(roots[i], roots[j]) {
				rootExcludes[i] = append(rootExcludes[i], roots[j])
				if !cfg.Quiet {
					fmt.Fprintf(os.Stderr, "note: %s is inside %s; excluding it from the latter's scan\n",
						roots[j], roots[i])
				}
			}
		}
	}

	// crossRoot: with >=2 roots we surface only duplicates that bridge different
	// roots (the point of comparing backups). --within-root disables the filter;
	// a single root always reports all its internal duplicates.
	crossRoot := len(roots) >= 2 && !cfg.WithinRoot

	// Scan
	status := func(format string, a ...any) {
		if !cfg.Quiet {
			fmt.Fprintf(os.Stderr, format, a...)
		}
	}

	// Live profiling endpoint (loopback-only, on by default). Lets a multi-hour
	// run be profiled while it runs; see pprof.go / DUP_DETECTOR_PPROF.
	startPprof(status)

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
		} else if c, cerr := OpenHashCache(cachePath, cfg.Rehash, cfg.CacheMaxAgeDur); cerr != nil {
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

	// Announce the roots to analyze (requirement: print the root list at start).
	status("Analyzing %d root(s):\n", len(roots))
	for i, r := range roots {
		status("  [%d] %s\n", i, r)
	}

	counts := make([]int, len(roots))
	for i, r := range roots {
		idx := i // capture for the closure
		status("Scanning %s ...\n", r)
		if err := ScanToStore(store, r, &cfg, rootExcludes[idx], seenInodes, idx,
			func(ScannedFile) { counts[idx]++ }); err != nil {
			return fmt.Errorf("scanning %s: %w", r, err)
		}
	}
	var totalFiles int
	for i, n := range counts {
		totalFiles += n
		if len(roots) > 1 {
			status("  [%d] %s: %d file(s)\n", i, roots[i], n)
		}
	}
	status("Found %d file(s) total\n", totalFiles)

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
	treeState.CoverageCheck = func(dirA, dirB string, index map[string][]string) (bool, int64, error) {
		return store.CoverageAndSize(dirA, dirB, index)
	}
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
	treeState.AddConfirmed(earlyTrees)

	// ── Phase 2: MD5 (only if -c flag set) ───────────────────────────────────
	if cfg.Checksum {
		status("Detecting duplicates (MD5 pass, largest first)...\n")
		err = ChecksumDupsStore(store, crossRoot, nil, cfg.Workers, cache,
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
		groups, gerr := MtimeDupsStore(store, crossRoot)
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
