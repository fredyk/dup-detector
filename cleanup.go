package main

import (
	"bufio"
	"fmt"
	"os"
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

	for i, t := range pairs {
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

	for i, g := range groups {
		if autoMode {
			for _, f := range g.Files[1:] {
				if !deleted[f.Path] {
					removeFile(f.Path, g.Size, deleted, cfg)
				}
			}
			continue
		}

		fmt.Fprintf(os.Stderr, "\n[%d/%d] %s\n", i+1, len(groups), FormatSize(g.Size))
		for j, f := range g.Files {
			fmt.Fprintf(os.Stderr, "  [%d] %s\n", j+1, f.Path)
		}

	prompt:
		for {
			if len(g.Files) == 2 {
				fmt.Fprint(os.Stderr, "  Delete [1], [2], [s]kip, [a]uto-keep-first, [q]uit, [?]help: ")
			} else {
				fmt.Fprintf(os.Stderr, "  Delete which? (e.g. 2,3), [s]kip, [a]uto, [q]uit, [?]help: ")
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

func parseIndices(input string, max int) ([]int, bool) {
	var result []int
	seen := make(map[int]bool)
	for _, part := range strings.Split(input, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > max {
			return nil, false
		}
		if !seen[n-1] {
			result = append(result, n-1)
			seen[n-1] = true
		}
	}
	return result, len(result) > 0
}

func printFileHelp() {
	fmt.Fprintln(os.Stderr, `
  File deletion commands:
    1        delete copy [1], keep the rest
    2        delete copy [2], keep the rest
    1,3      delete copies [1] and [3]
    s        skip this group
    a        auto mode: keep [1], delete rest for this and all remaining groups
    q        quit
    ?        show this help`)
}
