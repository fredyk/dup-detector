package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// --no-interactive turns the tool into a pure reporter: stdout carries ONE CSV
// and nothing else, so a caller can pipe it straight into a file. These tests
// lock the three contracts: the row shape (one line per duplicate file, with
// the identifier that made it a duplicate), stdout being reserved for that CSV
// alone, and the run touching nothing on disk.

func csvRows(t *testing.T, s string) [][]string {
	t.Helper()
	rows, err := csv.NewReader(strings.NewReader(s)).ReadAll()
	if err != nil {
		t.Fatalf("stdout is not valid CSV: %v\n---\n%s\n---", err, s)
	}
	return rows
}

func columnOfRow(t *testing.T, header, row []string, name string) string {
	t.Helper()
	for i, h := range header {
		if h == name {
			if i >= len(row) {
				t.Fatalf("row %v has no column %q", row, name)
			}
			return row[i]
		}
	}
	t.Fatalf("header %v has no column %q", header, name)
	return ""
}

func TestNoInteractiveCSVUsesMD5AsIDWhenChecksummed(t *testing.T) {
	groups := []DupGroup{{
		Size: 12,
		Hash: "0cbc6611f5540bd0809a388dc95a615b",
		Files: []ScannedFile{
			{Path: "/a/one.txt", Size: 12, ModTime: 1700000000, Source: 0},
			{Path: "/b/two.txt", Size: 12, ModTime: 1700009999, Source: 1},
		},
	}}

	var buf bytes.Buffer
	if err := PrintDupCSV(groups, []string{"/a", "/b"}, &buf); err != nil {
		t.Fatal(err)
	}
	rows := csvRows(t, buf.String())
	if len(rows) != 3 {
		t.Fatalf("expected header + 2 file rows, got %d rows: %v", len(rows), rows)
	}
	header := rows[0]
	for _, r := range rows[1:] {
		if got := columnOfRow(t, header, r, "id"); got != "0cbc6611f5540bd0809a388dc95a615b" {
			t.Errorf("id column = %q, want the group MD5", got)
		}
		if got := columnOfRow(t, header, r, "id_source"); got != "md5" {
			t.Errorf("id_source = %q, want md5", got)
		}
		if got := columnOfRow(t, header, r, "copies"); got != "2" {
			t.Errorf("copies = %q, want 2", got)
		}
	}
	if got := columnOfRow(t, header, rows[1], "path"); got != "/a/one.txt" {
		t.Errorf("first row path = %q", got)
	}
	if got := columnOfRow(t, header, rows[1], "root"); got != "/a" {
		t.Errorf("first row root = %q, want /a", got)
	}
	if got := columnOfRow(t, header, rows[2], "root"); got != "/b" {
		t.Errorf("second row root = %q, want /b", got)
	}
}

func TestNoInteractiveCSVFallsBackToSizeMtimeID(t *testing.T) {
	// No -c: the files matched on size+mtime, so that pair IS the identifier.
	groups := []DupGroup{{
		Size: 12,
		Files: []ScannedFile{
			{Path: "/a/one.txt", Size: 12, ModTime: 1700000000},
			{Path: "/a/copy.txt", Size: 12, ModTime: 1700000000},
		},
	}}

	var buf bytes.Buffer
	if err := PrintDupCSV(groups, []string{"/a"}, &buf); err != nil {
		t.Fatal(err)
	}
	rows := csvRows(t, buf.String())
	if len(rows) != 3 {
		t.Fatalf("expected header + 2 file rows, got %v", rows)
	}
	header := rows[0]
	want := "s12-m1700000000"
	for _, r := range rows[1:] {
		if got := columnOfRow(t, header, r, "id"); got != want {
			t.Errorf("id = %q, want %q", got, want)
		}
		if got := columnOfRow(t, header, r, "id_source"); got != "size+mtime" {
			t.Errorf("id_source = %q, want size+mtime", got)
		}
		if got := columnOfRow(t, header, r, "mtime_utc"); got != "2023-11-14T22:13:20Z" {
			t.Errorf("mtime_utc = %q", got)
		}
	}
}

// Two groups that share a size but not an mtime must NOT collapse into one id.
func TestNoInteractiveCSVIDSeparatesGroupsOfEqualSize(t *testing.T) {
	groups := []DupGroup{
		{Size: 12, Files: []ScannedFile{
			{Path: "/a/x1", Size: 12, ModTime: 100},
			{Path: "/a/x2", Size: 12, ModTime: 100},
		}},
		{Size: 12, Files: []ScannedFile{
			{Path: "/a/y1", Size: 12, ModTime: 200},
			{Path: "/a/y2", Size: 12, ModTime: 200},
		}},
	}
	var buf bytes.Buffer
	if err := PrintDupCSV(groups, []string{"/a"}, &buf); err != nil {
		t.Fatal(err)
	}
	rows := csvRows(t, buf.String())
	header := rows[0]
	ids := map[string]bool{}
	for _, r := range rows[1:] {
		ids[columnOfRow(t, header, r, "id")] = true
	}
	if len(ids) != 2 {
		t.Fatalf("two distinct groups must get two distinct ids, got %v", ids)
	}
}

// The CSV must be usable as a whole-run report even when a group is empty or a
// file's Source points outside the root list (defensive: never panic mid-write).
func TestNoInteractiveCSVSurvivesOddGroups(t *testing.T) {
	groups := []DupGroup{
		{Size: 5, Files: nil},
		{Size: 5, Files: []ScannedFile{
			{Path: "/z/a", Size: 5, ModTime: 1, Source: 7},
			{Path: "/z/b", Size: 5, ModTime: 1, Source: -1},
		}},
	}
	var buf bytes.Buffer
	if err := PrintDupCSV(groups, []string{"/a"}, &buf); err != nil {
		t.Fatal(err)
	}
	rows := csvRows(t, buf.String())
	if len(rows) != 3 {
		t.Fatalf("expected header + 2 rows, got %v", rows)
	}
	if got := columnOfRow(t, rows[0], rows[1], "root"); got != "" {
		t.Errorf("out-of-range source must yield an empty root, got %q", got)
	}
}

// silenceStdout is the structural guarantee: once armed, ANY future write to
// os.Stdout — including one added by a later commit that never heard of this
// mode — lands on stderr, and the CSV goes to the real stdout it hands back.
func TestSilenceStdoutSendsEverythingElseToStderr(t *testing.T) {
	realOut, restore := silenceStdout()
	if os.Stdout != os.Stderr {
		restore()
		t.Fatal("silenceStdout must repoint os.Stdout at stderr")
	}
	if realOut == os.Stderr {
		restore()
		t.Fatal("the returned writer must be the ORIGINAL stdout, not stderr")
	}
	restore()
	if os.Stdout == os.Stderr {
		t.Fatal("restore must put os.Stdout back")
	}
}

// End-to-end: the real binary, on a real tree, must print a CSV and only a CSV,
// and must leave every file in place.
func TestNoInteractiveEndToEndStdoutIsOnlyCSV(t *testing.T) {
	bin := buildDupDetector(t)

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a/one.txt"), "duplicated content")
	writeFile(t, filepath.Join(dir, "b/one-copy.txt"), "duplicated content")
	writeFile(t, filepath.Join(dir, "b/unique.txt"), "not a duplicate at all")

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "-c", "--no-interactive", "--progress", dir)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = strings.NewReader("") // no tty, no answers
	if err := cmd.Run(); err != nil {
		t.Fatalf("run failed: %v\nstderr:\n%s", err, stderr.String())
	}

	rows := csvRows(t, stdout.String())
	if len(rows) != 3 {
		t.Fatalf("expected header + the 2 duplicate files, got %d rows:\n%s", len(rows), stdout.String())
	}
	header := rows[0]
	var paths []string
	for _, r := range rows[1:] {
		paths = append(paths, columnOfRow(t, header, r, "path"))
		id := columnOfRow(t, header, r, "id")
		if len(id) != 32 {
			t.Errorf("with -c the id must be an MD5 hex digest, got %q", id)
		}
		if got := columnOfRow(t, header, r, "id_source"); got != "md5" {
			t.Errorf("id_source = %q, want md5", got)
		}
	}
	for _, want := range []string{filepath.Join(dir, "a/one.txt"), filepath.Join(dir, "b/one-copy.txt")} {
		if !hasPath(paths, want) {
			t.Errorf("CSV is missing %s; rows: %v", want, paths)
		}
	}
	if hasPath(paths, filepath.Join(dir, "b/unique.txt")) {
		t.Error("the non-duplicate file must not appear")
	}
	if stderr.Len() == 0 {
		t.Error("the status chatter must still be visible — on stderr")
	}

	// Read-only: nothing was deleted, nothing was trashed.
	for _, sub := range []string{"a/one.txt", "b/one-copy.txt", "b/unique.txt"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
			t.Errorf("--no-interactive must not touch %s: %v", sub, err)
		}
	}
}

// Without -c the same run must still emit a CSV, keyed by size+mtime.
func TestNoInteractiveEndToEndWithoutChecksum(t *testing.T) {
	bin := buildDupDetector(t)

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a/one.txt"), "duplicated content")
	writeFile(t, filepath.Join(dir, "b/one-copy.txt"), "duplicated content")
	sameMtime(t, filepath.Join(dir, "a/one.txt"), filepath.Join(dir, "b/one-copy.txt"))

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "--no-interactive", dir)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run failed: %v\nstderr:\n%s", err, stderr.String())
	}
	rows := csvRows(t, stdout.String())
	if len(rows) != 3 {
		t.Fatalf("expected header + 2 rows, got:\n%s", stdout.String())
	}
	if got := columnOfRow(t, rows[0], rows[1], "id_source"); got != "size+mtime" {
		t.Errorf("id_source = %q, want size+mtime", got)
	}
}

// A run that finds nothing must still emit the header, so a consumer can tell
// "no duplicates" from "the tool died before writing anything".
func TestNoInteractiveEmitsHeaderWhenNoDuplicates(t *testing.T) {
	bin := buildDupDetector(t)

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "only.txt"), "alone in the world")

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "-c", "--no-interactive", dir)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run failed: %v\nstderr:\n%s", err, stderr.String())
	}
	rows := csvRows(t, stdout.String())
	if len(rows) != 1 {
		t.Fatalf("expected the header alone, got:\n%s", stdout.String())
	}
}

// --no-interactive is read-only by contract, so pairing it with a flag that
// deletes must fail loudly instead of quietly picking a winner.
func TestNoInteractiveRefusesDestructiveFlags(t *testing.T) {
	bin := buildDupDetector(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "x")

	for _, extra := range [][]string{
		{"--headless"},
		{"--remove-by-glob", "*/tmp/*"},
	} {
		args := append([]string{"--no-interactive"}, extra...)
		args = append(args, dir)
		cmd := exec.Command(bin, args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err == nil {
			t.Errorf("--no-interactive %v must be rejected, it exited 0", extra)
		}
		if stdout.Len() != 0 {
			t.Errorf("a rejected run must not write to stdout, got %q", stdout.String())
		}
		if !strings.Contains(stderr.String(), "--no-interactive") {
			t.Errorf("the error must name the offending flag, got %q", stderr.String())
		}
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

// buildDupDetector compiles the real binary once per test run so the
// end-to-end tests exercise the actual command, flags and stdio wiring.
func buildDupDetector(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "dup-detector-e2e")
		if err != nil {
			buildErr = err
			return
		}
		buildPath = filepath.Join(dir, "dup-detector")
		out, err := exec.Command("go", "build", "-o", buildPath, ".").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildPath
}

func sameMtime(t *testing.T, ref, other string) {
	t.Helper()
	fi, err := os.Stat(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func hasPath(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// --- escritura incremental --------------------------------------------------
//
// El informe se redirige a un fichero (`> tank_duplicates.csv`), y un scan de
// varias horas sobre un arbol grande se lo lleva earlyoom con frecuencia. Si el
// CSV solo se escribe al final, lo que queda es un fichero de CERO bytes con
// nombre de resultado bueno, que se lee exactamente igual que "no hay
// duplicados". La cabecera tiene que estar desde el principio, y cada lote en
// cuanto se conoce.

func TestDupCSVWriterEmitsHeaderBeforeAnyGroup(t *testing.T) {
	var buf bytes.Buffer
	if _, err := NewDupCSVWriter(&buf, nil); err != nil {
		t.Fatalf("NewDupCSVWriter: %v", err)
	}
	got := buf.String()
	want := strings.Join(dupCSVHeader, ",") + "\n"
	if got != want {
		t.Fatalf("sin escribir ningun grupo el buffer deberia tener solo la cabecera\n got: %q\nwant: %q", got, want)
	}
}

func TestDupCSVWriterStreamsEachBatchAsItArrives(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewDupCSVWriter(&buf, []string{"/rootA"})
	if err != nil {
		t.Fatalf("NewDupCSVWriter: %v", err)
	}

	lote1 := []DupGroup{{Size: 10, Hash: "aaa", Files: []ScannedFile{
		{Path: "/rootA/1", Size: 10, ModTime: 1700000000},
		{Path: "/rootA/2", Size: 10, ModTime: 1700000000},
	}}}
	if err := w.WriteGroups(lote1); err != nil {
		t.Fatalf("WriteGroups lote1: %v", err)
	}
	tras1 := buf.String()
	if !strings.Contains(tras1, "/rootA/1") {
		t.Fatalf("el primer lote no llego al fichero hasta el final: %q", tras1)
	}

	lote2 := []DupGroup{{Size: 20, Hash: "bbb", Files: []ScannedFile{
		{Path: "/rootA/3", Size: 20, ModTime: 1700000001},
		{Path: "/rootA/4", Size: 20, ModTime: 1700000001},
	}}}
	if err := w.WriteGroups(lote2); err != nil {
		t.Fatalf("WriteGroups lote2: %v", err)
	}
	if !strings.Contains(buf.String(), "/rootA/3") {
		t.Fatalf("el segundo lote no se escribio: %q", buf.String())
	}
	// Y lo del primer lote sigue donde estaba, en su orden.
	if strings.Index(buf.String(), "/rootA/1") > strings.Index(buf.String(), "/rootA/3") {
		t.Fatal("los lotes no salieron en el orden en que se descubrieron")
	}
}

// Escribir por lotes no puede cambiar ni una coma del informe: quien lo lea
// despues tiene que encontrar lo mismo que producia el volcado de una vez.
func TestDupCSVWriterMatchesSingleShotOutput(t *testing.T) {
	groups := []DupGroup{
		{Size: 10, Hash: "aaa", Files: []ScannedFile{
			{Path: "/rootA/b", Size: 10, ModTime: 1700000000, Source: 0},
			{Path: "/rootA/a", Size: 10, ModTime: 1700000000, Source: 0},
		}},
		{Size: 20, Files: []ScannedFile{
			{Path: "/rootB/x", Size: 20, ModTime: 1700000001, Source: 1},
			{Path: "/rootB/y", Size: 20, ModTime: 1700000001, Source: 1},
		}},
	}
	roots := []string{"/rootA", "/rootB"}

	var deUnaVez bytes.Buffer
	if err := PrintDupCSV(groups, roots, &deUnaVez); err != nil {
		t.Fatalf("PrintDupCSV: %v", err)
	}

	var porLotes bytes.Buffer
	w, err := NewDupCSVWriter(&porLotes, roots)
	if err != nil {
		t.Fatalf("NewDupCSVWriter: %v", err)
	}
	for _, g := range groups {
		if err := w.WriteGroups([]DupGroup{g}); err != nil {
			t.Fatalf("WriteGroups: %v", err)
		}
	}
	if porLotes.String() != deUnaVez.String() {
		t.Fatalf("el informe por lotes difiere del de una vez\n lotes: %q\n  unico: %q",
			porLotes.String(), deUnaVez.String())
	}
}
