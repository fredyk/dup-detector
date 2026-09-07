package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"
)

// dupCSVHeader is the row shape of --no-interactive: one line per duplicate
// FILE (not per group), keyed by the identifier that actually made it a
// duplicate — the MD5 under -c, the size+mtime pair otherwise. Grouping is
// recovered downstream by sorting on `id`, so the report survives being cut,
// filtered or joined without losing which copies belong together.
var dupCSVHeader = []string{"id", "id_source", "size_bytes", "mtime_utc", "copies", "root", "path"}

// GroupID is the identifier shared by every copy in the group: the content MD5
// when it was computed (-c), otherwise the size+mtime pair the match was made
// on. The "s…-m…" shape keeps the two id spaces from ever colliding with a hex
// digest, and encodes both halves so equal-sized groups stay distinct.
func (g DupGroup) GroupID() string {
	if g.Hash != "" {
		return g.Hash
	}
	var mtime int64
	if len(g.Files) > 0 {
		mtime = g.Files[0].ModTime
	}
	return fmt.Sprintf("s%d-m%d", g.Size, mtime)
}

// IDSource names what GroupID was derived from, so a consumer never has to
// guess whether the run was content-verified.
func (g DupGroup) IDSource() string {
	if g.Hash != "" {
		return "md5"
	}
	return "size+mtime"
}

// PrintDupCSV writes the complete duplicate list as CSV. roots maps a file's
// Source index back to the root it was found under; an index outside that list
// yields an empty root rather than a panic. The header is always written, so an
// empty report is distinguishable from a run that died before printing.
func PrintDupCSV(groups []DupGroup, roots []string, w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(dupCSVHeader); err != nil {
		return err
	}
	for _, g := range groups {
		if len(g.Files) == 0 {
			continue
		}
		id, source, copies := g.GroupID(), g.IDSource(), strconv.Itoa(len(g.Files))

		// Copy before sorting: g.Files is shared with the deletion queue, and
		// reordering it there would silently change which copy is kept.
		files := append([]ScannedFile(nil), g.Files...)
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

		for _, f := range files {
			root := ""
			if f.Source >= 0 && f.Source < len(roots) {
				root = roots[f.Source]
			}
			if err := cw.Write([]string{
				id,
				source,
				strconv.FormatInt(f.Size, 10),
				time.Unix(f.ModTime, 0).UTC().Format(time.RFC3339),
				copies,
				root,
				f.Path,
			}); err != nil {
				return err
			}
		}
	}
	cw.Flush()
	return cw.Error()
}

// silenceStdout repoints os.Stdout at stderr and hands back the real stdout.
// Everything that prints through the os.Stdout variable — status lines, prompts,
// and any call added by a future commit that never heard of --no-interactive —
// then lands on stderr automatically, instead of corrupting the CSV. Guarding
// each print site individually would leave the trap armed for the next one.
func silenceStdout() (*os.File, func()) {
	real := os.Stdout
	os.Stdout = os.Stderr
	return real, func() { os.Stdout = real }
}
