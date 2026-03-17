package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// InteractiveDelete prompts the user to selectively delete duplicates.
// Groups must be sorted by size descending (largest first).
func InteractiveDelete(groups []DupGroup, cfg *Config) error {
	if len(groups) == 0 {
		return nil
	}

	var totalReclaimable int64
	for _, g := range groups {
		totalReclaimable += g.WastedBytes()
	}

	fmt.Fprintf(os.Stderr, "\nProceed to interactive deletion? (%s reclaimable) [y/N] ",
		FormatSize(totalReclaimable))

	reader := bufio.NewReader(os.Stdin)
	ans, _ := reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(ans)) != "y" {
		return nil
	}
	fmt.Fprintln(os.Stderr)

	autoMode := false // when true: always keep [1], delete rest
	totalDeleted := int64(0)
	deletedCount := 0

	for i, g := range groups {
		if autoMode {
			for _, f := range g.Files[1:] {
				if err := os.Remove(f.Path); err != nil {
					fmt.Fprintf(os.Stderr, "  error: %v\n", err)
				} else {
					totalDeleted += g.Size
					deletedCount++
					if cfg.Verbose {
						fmt.Fprintf(os.Stderr, "  deleted: %s\n", f.Path)
					}
				}
			}
			continue
		}

		// Print group header
		if len(g.Files) == 2 {
			fmt.Fprintf(os.Stderr, "[%d/%d] %s\n", i+1, len(groups), FormatSize(g.Size))
		} else {
			fmt.Fprintf(os.Stderr, "[%d/%d] %s × %d copies (%s reclaimable)\n",
				i+1, len(groups), FormatSize(g.Size), len(g.Files), FormatSize(g.WastedBytes()))
		}
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
				printDeleteHelp()
			case "", "s":
				break prompt
			case "q":
				printDeleteSummary(deletedCount, totalDeleted)
				return nil
			case "a":
				autoMode = true
				deleteFiles(g.Files[1:], g.Size, cfg, &totalDeleted, &deletedCount)
				break prompt
			default:
				indices, ok := parseIndices(line, len(g.Files))
				if !ok {
					fmt.Fprintln(os.Stderr, "  Invalid input. Enter number(s), s, a, q, or ?")
					continue
				}
				// Show confirmation
				fmt.Fprintln(os.Stderr, "  Will delete:")
				for _, idx := range indices {
					fmt.Fprintf(os.Stderr, "    %s\n", g.Files[idx].Path)
				}
				fmt.Fprint(os.Stderr, "  Confirm? [Y/n] ")
				confirm, _ := reader.ReadString('\n')
				if strings.ToLower(strings.TrimSpace(confirm)) == "n" {
					continue
				}
				toDelete := make([]ScannedFile, 0, len(indices))
				for _, idx := range indices {
					toDelete = append(toDelete, g.Files[idx])
				}
				deleteFiles(toDelete, g.Size, cfg, &totalDeleted, &deletedCount)
				break prompt
			}
		}
	}

	printDeleteSummary(deletedCount, totalDeleted)
	return nil
}

func deleteFiles(files []ScannedFile, size int64, cfg *Config, totalDeleted *int64, count *int) {
	for _, f := range files {
		if err := os.Remove(f.Path); err != nil {
			fmt.Fprintf(os.Stderr, "  error deleting %s: %v\n", f.Path, err)
		} else {
			*totalDeleted += size
			*count++
			if cfg.Verbose {
				fmt.Fprintf(os.Stderr, "  deleted: %s\n", f.Path)
			}
		}
	}
}

func printDeleteSummary(count int, freed int64) {
	if count > 0 {
		fmt.Fprintf(os.Stderr, "\nDeleted %d file(s), freed %s\n", count, FormatSize(freed))
	} else {
		fmt.Fprintln(os.Stderr, "\nNo files deleted.")
	}
}

// parseIndices parses "1", "2,3", "1,3" into 0-based indices.
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

func printDeleteHelp() {
	fmt.Fprintln(os.Stderr, `
  Deletion commands:
    1        delete copy [1], keep the rest
    2        delete copy [2], keep the rest
    1,3      delete copies [1] and [3]
    s        skip this group (keep all copies)
    a        auto mode: keep [1], delete rest for this and all remaining groups
    q        quit (stop here, keep remaining groups untouched)
    ?        show this help`)
}
