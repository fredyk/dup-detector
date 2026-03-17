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
	Checksum    bool
	Recursive   bool
	Verbose     bool
	Quiet       bool
	DryRun      bool
	Progress    bool
	Excludes    []string
	ExcludeFrom string
	Includes    []string
	IncludeFrom string
	MinSizeStr  string
	MaxSizeStr  string

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

	status("Scanning %s ...\n", dirA)
	filesA, err := Scan(dirA, &cfg, excludeFromA)
	if err != nil {
		return fmt.Errorf("scanning %s: %w", dirA, err)
	}

	var filesB []ScannedFile
	if twoDir {
		status("Scanning %s ...\n", dirB)
		filesB, err = Scan(dirB, &cfg, excludeFromB)
		if err != nil {
			return fmt.Errorf("scanning %s: %w", dirB, err)
		}
		status("Found %d files in A, %d files in B\n", len(filesA), len(filesB))
	} else {
		status("Found %d files\n", len(filesA))
	}

	// Merge and sort all files for tree dup analysis
	allFiles := make([]ScannedFile, 0, len(filesA)+len(filesB))
	allFiles = append(allFiles, filesA...)
	allFiles = append(allFiles, filesB...)
	SortFilesByPath(allFiles)

	// ── Phase 1: fast tree detection via directory hashing ───────────────────
	status("Detecting duplicates (fast pass)...\n")
	mtimeGroups := MtimeDups(filesA, filesB)

	treeState := NewTreeDupState()
	treeState.Workers = cfg.Workers
	var allGroups []DupGroup

	var hashProgressFn func(done, total int)
	if cfg.Progress {
		hashProgressFn = func(done, total int) {
			fmt.Fprintf(os.Stderr, "\r  hashing dirs: %d / %d files  ", done, total)
		}
	}
	earlyTrees := FindTreeDupsByHash(allFiles, hashProgressFn)
	if cfg.Progress {
		fmt.Fprintln(os.Stderr)
	}

	// Register early trees so MD5 pass doesn't re-report them
	treeState.Confirmed = append(treeState.Confirmed, earlyTrees...)

	if !cfg.DryRun && len(earlyTrees) > 0 {
		deleted := OfferTreeDeletions(earlyTrees, allFiles, &cfg)
		for p := range deleted {
			treeState.MarkDeleted(p)
		}
	}
	treeState.MarkHandled()

	// ── Phase 2: MD5 streaming (only if -c flag set) ─────────────────────────
	if cfg.Checksum {
		status("Detecting duplicates (MD5 pass, largest first)...\n")

		// Skip files already deleted in phase 1
		skip := treeState.DeletedPaths()

		err = ChecksumDups(filesA, filesB, twoDir, skip, cfg.Workers,
			func(done, total int64) {
				if cfg.Progress {
					pct := int(100 * done / (total + 1))
					fmt.Fprintf(os.Stderr, "\r  MD5: %d%%  (%s / %s)  ",
						pct, FormatSize(done), FormatSize(total))
				}
			},
			func(newGroups []DupGroup) bool {
				allGroups = append(allGroups, newGroups...)
				if cfg.DryRun {
					return true
				}
				// Check for new confirmed tree dups from this MD5 batch
				newTrees := treeState.AddGroups(newGroups, allFiles, true)
				if len(newTrees) > 0 {
					if cfg.Progress {
						fmt.Fprintln(os.Stderr)
					}
					deleted := OfferTreeDeletions(newTrees, allFiles, &cfg)
					for p := range deleted {
						treeState.MarkDeleted(p)
						skip[p] = true
					}
					treeState.MarkHandled()
				}
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
		allGroups = mtimeGroups
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

	// Interactive deletion: any remaining unhandled trees + file-level dups
	if !cfg.DryRun {
		unhandledTrees := treeState.UnhandledTrees()
		if err := InteractiveDelete(unhandledTrees, allGroups, allFiles, &cfg); err != nil {
			return err
		}
	}

	return nil
}
