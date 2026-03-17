package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

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
