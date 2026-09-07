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

// DupCSVWriter emits the report as the scan discovers it, instead of holding
// everything back until the end.
//
// The report is meant to be redirected to a file, and a multi-hour scan over a
// multi-terabyte tree gets killed often enough (earlyoom) that "at the end"
// frequently means "never". A zero-byte file with the name of a good report
// reads exactly like "no duplicates found". So the header goes out the moment
// the writer exists, and every batch as soon as it is known: an interrupted run
// then leaves a short report, which is honest, instead of an empty one, which
// lies.
type DupCSVWriter struct {
	cw    *csv.Writer
	roots []string
}

// NewDupCSVWriter writes the header immediately and returns a writer for the
// rows. roots maps a file's Source index back to the root it was found under.
func NewDupCSVWriter(w io.Writer, roots []string) (*DupCSVWriter, error) {
	cw := csv.NewWriter(w)
	if err := cw.Write(dupCSVHeader); err != nil {
		return nil, err
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return nil, err
	}
	return &DupCSVWriter{cw: cw, roots: roots}, nil
}

// WriteGroups appends one batch and flushes it. Flushing per batch is what makes
// the partial report real: buffered rows die with the process.
func (d *DupCSVWriter) WriteGroups(groups []DupGroup) error {
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
			if f.Source >= 0 && f.Source < len(d.roots) {
				root = d.roots[f.Source]
			}
			if err := d.cw.Write([]string{
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
	d.cw.Flush()
	return d.cw.Error()
}

// PrintDupCSV writes a complete duplicate list in one go: the header plus every
// group. It is the whole report for the runs that have one to hand (the fast
// size+mtime pass), and the empty report otherwise — the header alone, which is
// how "no duplicates" is told apart from a run that died before printing.
func PrintDupCSV(groups []DupGroup, roots []string, w io.Writer) error {
	d, err := NewDupCSVWriter(w, roots)
	if err != nil {
		return err
	}
	return d.WriteGroups(groups)
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
