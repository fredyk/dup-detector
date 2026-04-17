package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// OfferTreeDeletions interactively offers deletion of a batch of tree dup pairs
// discovered during progressive scanning. Returns the set of deleted paths.
func OfferTreeDeletions(pairs []TreeDupPair, allFiles []ScannedFile, cfg *Config) map[string]bool {
	if len(pairs) == 0 {
		return map[string]bool{}
	}
	deleted := make(map[string]bool)
	reader := bufio.NewReader(os.Stdin)

	var treeTotal int64
	for _, t := range pairs {
		treeTotal += t.TotalSize
	}
	fmt.Fprintf(os.Stderr,
		"\n%d new tree duplicate(s) found (%s reclaimable each copy).\n",
		len(pairs), FormatSize(treeTotal))

	_ = handleTreeDups(pairs, allFiles, reader, deleted, cfg)
	return deleted
}

// InteractiveDelete first offers bulk deletion of complete tree duplicates,
// then handles remaining file-level duplicates (skipping any files already deleted).
func InteractiveDelete(treePairs []TreeDupPair, groups []DupGroup, allFiles []ScannedFile, cfg *Config) error {
	if len(treePairs) == 0 && len(groups) == 0 {
		return nil
	}

	reader := bufio.NewReader(os.Stdin)
	deleted := make(map[string]bool) // tracks paths already removed

	// ── Phase 1: tree duplicates ────────────────────────────────────────────
	if len(treePairs) > 0 {
		var treeTotal int64
		for _, t := range treePairs {
			treeTotal += t.TotalSize
		}
		fmt.Fprintf(os.Stderr,
			"\n%d complete tree duplicate(s) found (%s reclaimable each copy).\n",
			len(treePairs), FormatSize(treeTotal))
		fmt.Fprint(os.Stderr, "Handle tree duplicates first? [Y/n] ")
		ans, _ := reader.ReadString('\n')
		if strings.ToLower(strings.TrimSpace(ans)) != "n" {
			if err := handleTreeDups(treePairs, allFiles, reader, deleted, cfg); err != nil {
				return err
			}
		}
	}

	// ── Phase 2: file-level duplicates ──────────────────────────────────────
	// Filter groups: remove files already deleted and skip groups with < 2 survivors
	remaining := filterGroups(groups, deleted)
	if len(remaining) == 0 {
		if len(groups) > 0 {
			fmt.Fprintln(os.Stderr, "\nAll file-level duplicates already handled via tree deletion.")
		}
		printGrandTotal(deleted)
		return nil
	}

	// Order by reclaimable bytes desc (size × extra copies), so groups that free
	// the most space come first regardless of individual file size.
	sort.Slice(remaining, func(i, j int) bool {
		return remaining[i].WastedBytes() > remaining[j].WastedBytes()
	})

	var fileTotal int64
	for _, g := range remaining {
		fileTotal += g.WastedBytes()
	}
	fmt.Fprintf(os.Stderr,
		"\n%d file-level duplicate group(s) remaining (%s reclaimable).\n",
		len(remaining), FormatSize(fileTotal))
	fmt.Fprint(os.Stderr, "Proceed to interactive file-level deletion? [y/N] ")
	ans, _ := reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(ans)) == "y" {
		if err := handleFileDups(remaining, reader, deleted, cfg); err != nil {
			return err
		}
	}

	printGrandTotal(deleted)
	return nil
}

// ── Tree deletion ────────────────────────────────────────────────────────────

func handleTreeDups(pairs []TreeDupPair, allFiles []ScannedFile, reader *bufio.Reader, deleted map[string]bool, cfg *Config) error {
	autoMode := false

	// Orient pairs so the less-frequent backup cadence (quarterly > monthly >
	// weekly > daily) sits in slot A, which is the one `auto` keeps.
	reorientPairsByBackupCadence(pairs)

	for i, t := range pairs {
		// Skip pairs where a side has already been fully deleted in this session.
		if dirFullyDeleted(t.DirA, allFiles, deleted) || dirFullyDeleted(t.DirB, allFiles, deleted) {
			continue
		}

		if autoMode {
			deleteTree(t.DirB, allFiles, deleted, cfg)
			continue
		}

		fmt.Fprintf(os.Stderr, "\n[T%d/%d] %s  (%d files identical)\n",
			i+1, len(pairs), FormatSize(t.TotalSize), t.FileCount)
		fmt.Fprintf(os.Stderr, "  [A] %s\n", t.DirA)
		fmt.Fprintf(os.Stderr, "  [B] %s\n", t.DirB)

	prompt:
		for {
			fmt.Fprint(os.Stderr, "  Delete [A], [B], [s]kip, [a]uto-delete-B, [q]uit, [?]help: ")
			line, _ := reader.ReadString('\n')
			line = strings.TrimSpace(line) // preserve case: A ≠ a

			switch line {
			case "?":
				printTreeHelp()
			case "", "s", "S":
				break prompt
			case "q", "Q":
				return nil
			case "A":
				confirmAndDeleteTree("A", t.DirA, allFiles, reader, deleted, cfg)
				break prompt
			case "B", "b":
				confirmAndDeleteTree("B", t.DirB, allFiles, reader, deleted, cfg)
				break prompt
			case "a":
				autoMode = true
				deleteTree(t.DirB, allFiles, deleted, cfg)
				break prompt
			default:
				fmt.Fprintln(os.Stderr, "  Invalid input. Enter A, B, s, a, q, or ?")
			}
		}
	}
	return nil
}

func confirmAndDeleteTree(label, dir string, allFiles []ScannedFile, reader *bufio.Reader, deleted map[string]bool, cfg *Config) {
	files := filesUnderDir(dir, allFiles)
	fmt.Fprintf(os.Stderr, "  Will delete entire tree [%s]: %s  (%d files)\n", label, dir, len(files))
	fmt.Fprint(os.Stderr, "  Confirm? [Y/n] ")
	confirm, _ := reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(confirm)) == "n" {
		return
	}
	deleteTree(dir, allFiles, deleted, cfg)
}

func deleteTree(dir string, allFiles []ScannedFile, deleted map[string]bool, cfg *Config) {
	files := filesUnderDir(dir, allFiles)
	for _, f := range files {
		if deleted[f.Path] {
			continue
		}
		if err := os.Remove(f.Path); err != nil {
			fmt.Fprintf(os.Stderr, "  error: %v\n", err)
		} else {
			deleted[f.Path] = true
			if cfg.Verbose {
				fmt.Fprintf(os.Stderr, "  deleted: %s\n", f.Path)
			}
		}
	}
	// Remove empty directories bottom-up
	pruneEmptyDirs(dir)
	fmt.Fprintf(os.Stderr, "  Deleted %d files from %s\n", len(files), dir)
}

// pruneEmptyDirs removes empty directories inside root, bottom-up.
func pruneEmptyDirs(root string) {
	// Walk in reverse to clean up empty subdirs before parents
	var dirs []string
	_ = walkDirs(root, &dirs)
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // only succeeds if empty
	}
	_ = os.Remove(root) // remove root itself if empty
}

func walkDirs(root string, dirs *[]string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			sub := root + string(os.PathSeparator) + e.Name()
			*dirs = append(*dirs, sub)
			walkDirs(sub, dirs)
		}
	}
	return nil
}

func printTreeHelp() {
	fmt.Fprintln(os.Stderr, `
  Tree deletion commands:
    A    delete directory [A] entirely (keeping [B])
    B    delete directory [B] entirely (keeping [A])
    s    skip this pair
    a    auto mode: delete [B] for this and all remaining tree pairs
    q    quit tree phase (proceed to file-level)
    ?    show this help`)
}

// ── File-level deletion ──────────────────────────────────────────────────────

func handleFileDups(groups []DupGroup, reader *bufio.Reader, deleted map[string]bool, cfg *Config) error {
	autoMode := false

	for i := range groups {
		reorientFilesByBackupCadence(groups[i].Files)
	}

	for i, g := range groups {
		if autoMode {
			for _, f := range g.Files[1:] {
				if !deleted[f.Path] {
					removeFile(f.Path, g.Size, deleted, cfg)
				}
			}
			continue
		}

		fmt.Fprintf(os.Stderr, "\n[%d/%d] %s reclaimable (%s × %d extra copies)\n",
			i+1, len(groups), FormatSize(g.WastedBytes()), FormatSize(g.Size), len(g.Files)-1)
		for j, f := range g.Files {
			fmt.Fprintf(os.Stderr, "  [%d] %s\n", j+1, f.Path)
		}

	prompt:
		for {
			if len(g.Files) == 2 {
				fmt.Fprint(os.Stderr, "  Delete [1], [2], [s]kip, [a]uto-keep-first, [q]uit, [?]help: ")
			} else {
				fmt.Fprintf(os.Stderr, "  Delete which? (e.g. 2,3 or 2-4,6), [s]kip, [a]uto, [q]uit, [?]help: ")
			}

			line, _ := reader.ReadString('\n')
			line = strings.TrimSpace(strings.ToLower(line))

			switch line {
			case "?":
				printFileHelp()
			case "", "s":
				break prompt
			case "q":
				return nil
			case "a":
				autoMode = true
				for _, f := range g.Files[1:] {
					if !deleted[f.Path] {
						removeFile(f.Path, g.Size, deleted, cfg)
					}
				}
				break prompt
			default:
				indices, ok := parseIndices(line, len(g.Files))
				if !ok {
					fmt.Fprintln(os.Stderr, "  Invalid input. Enter number(s), s, a, q, or ?")
					continue
				}
				if len(indices) >= len(g.Files) {
					fmt.Fprintf(os.Stderr, "  Refusing: would delete all %d copies (no survivor left). Pick fewer.\n", len(g.Files))
					continue
				}
				fmt.Fprintln(os.Stderr, "  Will delete:")
				for _, idx := range indices {
					fmt.Fprintf(os.Stderr, "    %s\n", g.Files[idx].Path)
				}
				fmt.Fprint(os.Stderr, "  Confirm? [Y/n] ")
				confirm, _ := reader.ReadString('\n')
				if strings.ToLower(strings.TrimSpace(confirm)) == "n" {
					continue
				}
				for _, idx := range indices {
					if !deleted[g.Files[idx].Path] {
						removeFile(g.Files[idx].Path, g.Size, deleted, cfg)
					}
				}
				break prompt
			}
		}
	}
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func removeFile(path string, _ int64, deleted map[string]bool, cfg *Config) {
	if err := os.Remove(path); err != nil {
		fmt.Fprintf(os.Stderr, "  error: %v\n", err)
		return
	}
	deleted[path] = true
	if cfg.Verbose {
		fmt.Fprintf(os.Stderr, "  deleted: %s\n", path)
	}
}

// filterGroups removes already-deleted files and drops groups with < 2 survivors.
func filterGroups(groups []DupGroup, deleted map[string]bool) []DupGroup {
	var result []DupGroup
	for _, g := range groups {
		var survivors []ScannedFile
		for _, f := range g.Files {
			if !deleted[f.Path] {
				survivors = append(survivors, f)
			}
		}
		if len(survivors) >= 2 {
			result = append(result, DupGroup{Size: g.Size, Files: survivors})
		}
	}
	return result
}

func printGrandTotal(deleted map[string]bool) {
	if len(deleted) > 0 {
		fmt.Fprintf(os.Stderr, "\nTotal deleted: %d file(s)\n", len(deleted))
	} else {
		fmt.Fprintln(os.Stderr, "\nNo files deleted.")
	}
}

// parseIndices accepts comma-separated numbers and ranges, e.g. "1,3-5,8".
// Indices are 1-based in the input; returned slice is 0-based.
func parseIndices(input string, max int) ([]int, bool) {
	var result []int
	seen := make(map[int]bool)
	add := func(n int) {
		if !seen[n-1] {
			result = append(result, n-1)
			seen[n-1] = true
		}
	}
	for _, part := range strings.Split(input, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, false
		}
		if i := strings.Index(part, "-"); i > 0 {
			lo, err1 := strconv.Atoi(strings.TrimSpace(part[:i]))
			hi, err2 := strconv.Atoi(strings.TrimSpace(part[i+1:]))
			if err1 != nil || err2 != nil || lo < 1 || hi > max || lo > hi {
				return nil, false
			}
			for n := lo; n <= hi; n++ {
				add(n)
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > max {
			return nil, false
		}
		add(n)
	}
	return result, len(result) > 0
}

func printFileHelp() {
	fmt.Fprintln(os.Stderr, `
  File deletion commands:
    1        delete copy [1], keep the rest
    2        delete copy [2], keep the rest
    1,3      delete copies [1] and [3]
    2-5      delete copies [2] through [5] inclusive
    1-3,5    mix ranges and singles (skips 4)
    s        skip this group
    a        auto mode: keep [1], delete rest for this and all remaining groups
    q        quit
    ?        show this help
  (selecting all copies is rejected — at least one must survive)`)
}

// Keep daily < weekly < monthly < quarterly — lower = more frequent =
// shorter retention = safer to delete.
var cadenceRank = map[string]int{
	"daily":     0,
	"weekly":    1,
	"monthly":   2,
	"quarterly": 3,
}

// Match the cadence word plus whatever trails in the same path segment
// (e.g. "daily-1-sunday", "weekly-2", "monthly-04-april", "quarterly-2026-1").
// Group 1 = prefix before cadence, 2 = cadence word, 3 = trailing path from
// the next '/' onwards (or empty if cadence ends the path).
var cadenceRegex = regexp.MustCompile(`(?i)^(.*?)\b(daily|weekly|monthly|quarterly)\b[^/]*(/.*)?$`)

// reorientPairsByBackupCadence swaps DirA/DirB in-place when both paths share
// the same prefix and trailing path and differ only within the cadence segment
// (cadence word + per-cadence numeric suffix). Ensures the less-frequent
// cadence ends up in slot A (the one auto-mode preserves).
func reorientPairsByBackupCadence(pairs []TreeDupPair) {
	for i := range pairs {
		a := cadenceRegex.FindStringSubmatch(pairs[i].DirA)
		b := cadenceRegex.FindStringSubmatch(pairs[i].DirB)
		if a == nil || b == nil {
			continue
		}
		if a[1] != b[1] || a[3] != b[3] {
			continue
		}
		rankA := cadenceRank[strings.ToLower(a[2])]
		rankB := cadenceRank[strings.ToLower(b[2])]
		if rankA < rankB {
			pairs[i].DirA, pairs[i].DirB = pairs[i].DirB, pairs[i].DirA
		}
	}
}

// reorientFilesByBackupCadence reorders a dup group's files so that, when all
// paths differ only within a cadence segment (same prefix and trailing path),
// the least-frequent cadence ends up first. Auto mode keeps index [0], so the
// longer-retention copy survives. No-op when any file fails to match or the
// surrounding context isn't uniform.
func reorientFilesByBackupCadence(files []ScannedFile) {
	if len(files) < 2 {
		return
	}
	ranks := make([]int, len(files))
	var prefix, suffix string
	for i, f := range files {
		sub := cadenceRegex.FindStringSubmatch(f.Path)
		if sub == nil {
			return
		}
		if i == 0 {
			prefix, suffix = sub[1], sub[3]
		} else if sub[1] != prefix || sub[3] != suffix {
			return
		}
		ranks[i] = cadenceRank[strings.ToLower(sub[2])]
	}
	idx := make([]int, len(files))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return ranks[idx[a]] > ranks[idx[b]] })
	reordered := make([]ScannedFile, len(files))
	for i, j := range idx {
		reordered[i] = files[j]
	}
	copy(files, reordered)
}

// dirFullyDeleted returns true if every file under dir (as recorded in allFiles)
// has been deleted in this session. Used to skip pairs involving already-gone dirs.
func dirFullyDeleted(dir string, allFiles []ScannedFile, deleted map[string]bool) bool {
	files := filesUnderDir(dir, allFiles)
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		if !deleted[f.Path] {
			return false
		}
	}
	return true
}
