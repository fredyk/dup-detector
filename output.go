package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// PrintTreeDups formats and writes tree-level duplicate pairs to w.
func PrintTreeDups(pairs []TreeDupPair, format string, w io.Writer) error {
	switch format {
	case "json":
		type jsonTree struct {
			TotalSize int64    `json:"total_size"`
			FileCount int      `json:"file_count"`
			Dirs      []string `json:"dirs"`
		}
		out := make([]jsonTree, len(pairs))
		for i, p := range pairs {
			out[i] = jsonTree{TotalSize: p.TotalSize, FileCount: p.FileCount, Dirs: []string{p.DirA, p.DirB}}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"tree_duplicates": out})
	case "csv":
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"type", "size_bytes", "size_human", "file_count", "dir_a", "dir_b"})
		for _, p := range pairs {
			_ = cw.Write([]string{"tree", fmt.Sprintf("%d", p.TotalSize), FormatSize(p.TotalSize),
				fmt.Sprintf("%d", p.FileCount), p.DirA, p.DirB})
		}
		cw.Flush()
		return cw.Error()
	default: // columns, simple
		fmt.Fprintln(w, "# === Tree duplicates ===")
		for i, p := range pairs {
			fmt.Fprintf(w, "# [T%d] %s  (%d files)\n", i+1, FormatSize(p.TotalSize), p.FileCount)
			fmt.Fprintf(w, "  %s\n  %s\n", p.DirA, p.DirB)
		}
		fmt.Fprintln(w)
		return nil
	}
}

// PrintGroups formats and writes duplicate groups to w.
func PrintGroups(groups []DupGroup, format string, w io.Writer) error {
	switch format {
	case "columns":
		return printColumns(groups, w)
	case "json":
		return printJSON(groups, w)
	case "csv":
		return printCSV(groups, w)
	case "simple":
		return printSimple(groups, w)
	default:
		return fmt.Errorf("unknown format %q (valid: columns, json, csv, simple)", format)
	}
}

func printColumns(groups []DupGroup, w io.Writer) error {
	for _, g := range groups {
		if len(g.Files) == 2 {
			fmt.Fprintf(w, "%s  |  %s  |  %s\n",
				g.Files[0].Path, g.Files[1].Path, FormatSize(g.Size))
		} else {
			fmt.Fprintf(w, "# %d copies, %s each\n", len(g.Files), FormatSize(g.Size))
			for _, f := range g.Files {
				fmt.Fprintf(w, "  %s\n", f.Path)
			}
		}
	}
	return nil
}

type jsonGroup struct {
	Size  int64    `json:"size"`
	Files []string `json:"files"`
}

func printJSON(groups []DupGroup, w io.Writer) error {
	out := make([]jsonGroup, 0, len(groups))
	for _, g := range groups {
		paths := make([]string, len(g.Files))
		for i, f := range g.Files {
			paths[i] = f.Path
		}
		out = append(out, jsonGroup{Size: g.Size, Files: paths})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printCSV(groups []DupGroup, w io.Writer) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"size_bytes", "size_human", "files"})
	for _, g := range groups {
		paths := make([]string, len(g.Files))
		for i, f := range g.Files {
			paths[i] = f.Path
		}
		_ = cw.Write([]string{
			fmt.Sprintf("%d", g.Size),
			FormatSize(g.Size),
			strings.Join(paths, "|"),
		})
	}
	cw.Flush()
	return cw.Error()
}

func printSimple(groups []DupGroup, w io.Writer) error {
	for _, g := range groups {
		for _, f := range g.Files {
			fmt.Fprintln(w, f.Path)
		}
		fmt.Fprintln(w)
	}
	return nil
}
