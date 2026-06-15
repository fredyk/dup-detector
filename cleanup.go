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

// actionKind distinguishes tree-level from file-level deletions in the
// unified interactive queue.
type actionKind int

const (
	actionTree actionKind = iota
	actionFileGroup
)

// cleanupAction is a single interactive unit: either a tree dup pair or a
// file-level dup group. Actions are sorted by reclaimable bytes descending
// and presented together with a uniform numeric prompt, so the user always
// faces the biggest win next regardless of kind.
type cleanupAction struct {
	kind  actionKind
	tree  TreeDupPair // valid when kind == actionTree
	group DupGroup    // valid when kind == actionFileGroup
}

func (a *cleanupAction) waste() int64 {
	if a.kind == actionTree {
		return a.tree.TotalSize
	}
	return a.group.WastedBytes()
}

func (a *cleanupAction) items() []string {
	if a.kind == actionTree {
		return []string{a.tree.DirA, a.tree.DirB}
	}
	paths := make([]string, len(a.group.Files))
	for i, f := range a.group.Files {
		paths[i] = f.Path
	}
	return paths
}

// InteractiveDelete presents all tree + file-level dup actions in a single
// merged queue sorted by reclaimable bytes descending, with a uniform
// numeric prompt shared by both kinds.
func InteractiveDelete(treePairs []TreeDupPair, groups []DupGroup, lookup DirLookup, cfg *Config) error {
	if len(treePairs) == 0 && len(groups) == 0 {
		return nil
	}

	// Orient so the less-frequent backup cadence sits at index [1] — auto
	// mode preserves that slot in both kinds.
	reorientPairsByBackupCadence(treePairs)
	for i := range groups {
		reorientFilesByBackupCadence(groups[i].Files)
	}

	actions := make([]cleanupAction, 0, len(treePairs)+len(groups))
	for _, t := range treePairs {
		actions = append(actions, cleanupAction{kind: actionTree, tree: t})
	}
	for _, g := range groups {
		actions = append(actions, cleanupAction{kind: actionFileGroup, group: g})
	}
	sort.SliceStable(actions, func(i, j int) bool {
		return actions[i].waste() > actions[j].waste()
	})

	reader := bufio.NewReader(os.Stdin)
	deleted := make(map[string]bool)

	var treeWaste, fileWaste int64
	var treeCount, fileCount int
	for i := range actions {
		if actions[i].kind == actionTree {
			treeWaste += actions[i].waste()
			treeCount++
		} else {
			fileWaste += actions[i].waste()
			fileCount++
		}
	}
	fmt.Fprintf(os.Stderr,
		"\n%d action(s) to review — %d tree(s) (%s) + %d file group(s) (%s). Sorted by reclaimable bytes desc.\n",
		len(actions), treeCount, FormatSize(treeWaste), fileCount, FormatSize(fileWaste))
	fmt.Fprintln(os.Stderr, "(Tree and file-level totals may overlap when the same bytes appear in both.)")
	fmt.Fprint(os.Stderr, "Proceed to interactive deletion? [y/N] ")
	ans, _ := reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(ans)) != "y" {
		printGrandTotal(deleted)
		return nil
	}

	autoMode := false

	var skipped int
	for i := range actions {
		a := &actions[i]

		// Re-resolve survivors against already-deleted paths (prior actions
		// in this loop may have removed files that also belong here).
		switch a.kind {
		case actionTree:
			if dirFullyDeleted(a.tree.DirA, lookup, deleted) || dirFullyDeleted(a.tree.DirB, lookup, deleted) {
				skipped++
				continue
			}
		case actionFileGroup:
			var survivors []ScannedFile
			for _, f := range a.group.Files {
				if !deleted[f.Path] {
					survivors = append(survivors, f)
				}
			}
			if len(survivors) < 2 {
				skipped++
				continue
			}
			a.group.Files = survivors
		}

		if autoMode {
			applyAuto(a, lookup, deleted, cfg)
			continue
		}

		stop := promptAction(i+1, len(actions), a, lookup, reader, deleted, cfg, &autoMode)
		if stop {
			break
		}
	}

	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "\n(%d action(s) skipped automatically — already resolved by earlier deletions.)\n", skipped)
	}
	printGrandTotal(deleted)
	return nil
}

// promptAction renders one action and handles user input. Returns true if the
// user requested to quit.
func promptAction(idx, total int, a *cleanupAction, lookup DirLookup, reader *bufio.Reader, deleted map[string]bool, cfg *Config, autoMode *bool) bool {
	items := a.items()
	kindLabel := "file group"
	if a.kind == actionTree {
		kindLabel = "tree"
	}

	fmt.Fprintf(os.Stderr, "\n[%d/%d] %s reclaimable", idx, total, FormatSize(a.waste()))
	if a.kind == actionTree {
		fmt.Fprintf(os.Stderr, " (%s, %d files identical)\n", kindLabel, a.tree.FileCount)
	} else {
		fmt.Fprintf(os.Stderr, " (%s, %s × %d extra copies)\n",
			kindLabel, FormatSize(a.group.Size), len(a.group.Files)-1)
	}
	for i, p := range items {
		fmt.Fprintf(os.Stderr, "  [%d] %s\n", i+1, p)
	}

	for {
		if len(items) == 2 {
			fmt.Fprint(os.Stderr, "  Delete [1], [2], [s]kip, [a]uto-keep-first, [q]uit, [?]help: ")
		} else {
			fmt.Fprint(os.Stderr, "  Delete which? (e.g. 2,3 or 2-4,6), [s]kip, [a]uto, [q]uit, [?]help: ")
		}
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(strings.ToLower(line))

		switch line {
		case "?":
			printActionHelp()
		case "", "s":
			return false
		case "q":
			return true
		case "a":
			*autoMode = true
			applyAuto(a, lookup, deleted, cfg)
			return false
		default:
			indices, ok := parseIndices(line, len(items))
			if !ok {
				fmt.Fprintln(os.Stderr, "  Invalid input. Enter number(s), range(s), s, a, q, or ?")
				continue
			}
			if len(indices) >= len(items) {
				fmt.Fprintf(os.Stderr, "  Refusing: would delete all %d copies (no survivor left). Pick fewer.\n", len(items))
				continue
			}
			fmt.Fprintln(os.Stderr, "  Will delete:")
			for _, j := range indices {
				fmt.Fprintf(os.Stderr, "    %s\n", items[j])
			}
			fmt.Fprint(os.Stderr, "  Confirm? [Y/n] ")
			confirm, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(confirm)) == "n" {
				continue
			}
			applyIndices(a, indices, lookup, deleted, cfg)
			return false
		}
	}
}

// applyAuto keeps index [0] and deletes the rest (for trees, index [0] is
// the less-frequent cadence when cadence reordering applied).
func applyAuto(a *cleanupAction, lookup DirLookup, deleted map[string]bool, cfg *Config) {
	n := 2
	if a.kind == actionFileGroup {
		n = len(a.group.Files)
	}
	if n < 2 {
		return
	}
	rest := make([]int, 0, n-1)
	for i := 1; i < n; i++ {
		rest = append(rest, i)
	}
	applyIndices(a, rest, lookup, deleted, cfg)
}

// applyIndices runs the actual deletions for the selected 0-based indices.
func applyIndices(a *cleanupAction, indices []int, lookup DirLookup, deleted map[string]bool, cfg *Config) {
	switch a.kind {
	case actionTree:
		items := a.items()
		for _, j := range indices {
			deleteTree(items[j], lookup, deleted, cfg)
		}
	case actionFileGroup:
		for _, j := range indices {
			if !deleted[a.group.Files[j].Path] {
				removeFile(a.group.Files[j].Path, a.group.Size, deleted, cfg)
			}
		}
	}
}

func printActionHelp() {
	fmt.Fprintln(os.Stderr, `
  Deletion commands:
    1        delete copy [1], keep the rest
    2        delete copy [2], keep the rest
    1,3      delete copies [1] and [3]
    2-5      delete copies [2] through [5] inclusive
    1-3,5    mix ranges and singles (skips 4)
    s        skip this action
    a        auto mode: keep [1], delete rest for this and all remaining actions
    q        quit
    ?        show this help
  (selecting all copies is rejected — at least one must survive)
  Tree actions: [N] is a directory; deleting removes the entire subtree.`)
}

// ── Deletion primitives ─────────────────────────────────────────────────────

func deleteTree(dir string, lookup DirLookup, deleted map[string]bool, cfg *Config) {
	files := lookup(dir)
	var removed int
	for _, f := range files {
		if deleted[f.Path] {
			continue
		}
		if err := os.Remove(f.Path); err != nil {
			fmt.Fprintf(os.Stderr, "  error: %v\n", err)
		} else {
			deleted[f.Path] = true
			removed++
			if cfg.Verbose {
				fmt.Fprintf(os.Stderr, "  deleted: %s\n", f.Path)
			}
		}
	}
	pruneEmptyDirs(dir)
	fmt.Fprintf(os.Stderr, "  Deleted %d of %d file(s) from %s\n", removed, len(files), dir)
}

// pruneEmptyDirs removes empty directories inside root, bottom-up.
func pruneEmptyDirs(root string) {
	var dirs []string
	_ = walkDirs(root, &dirs)
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}
	_ = os.Remove(root)
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

// dirFullyDeleted returns true if every file under dir has been removed.
func dirFullyDeleted(dir string, lookup DirLookup, deleted map[string]bool) bool {
	files := lookup(dir)
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

// ── Backup-cadence reorientation ────────────────────────────────────────────

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
// the same prefix and trailing path and differ only within the cadence
// segment, so the less-frequent cadence ends up in slot A (auto preserves it).
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
// the least-frequent cadence ends up first. No-op when any file fails to
// match or the surrounding context isn't uniform.
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
