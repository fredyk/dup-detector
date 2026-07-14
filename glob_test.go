package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"*photorec*", "/a/b/photorec_198/f19.a", true},
		{"*/tmp/photorec_*", "/tank/secure4/to_compare/SECURE5/tmp/photorec_198/f.a", true},
		{"*photorec*", "/a/b/other/f", false},
		{"*/SECURE5/tmp/*", "/tank/secure4/to_compare/SECURE5/tmp/x/y", true},
		{"*/SECURE5/tmp/*", "/tank/secure4/to_compare/GAMES2/tmp/x", false},
		{"*.zip", "/a/b/c.zip", true},
		{"*.zip", "/a/b/c.tgz", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.glob, c.path); got != c.want {
			t.Errorf("matchGlob(%q,%q)=%v want %v", c.glob, c.path, got, c.want)
		}
	}
}

func TestSelectByGlob(t *testing.T) {
	glob := "*/tmp/photorec_*"
	// mezcla: 2 dentro del glob, 1 fuera -> borra los 2 del glob, conserva el de fuera
	paths := []string{
		"/x/tmp/photorec_1/a",
		"/x/tmp/photorec_2/a",
		"/x/restored/a",
	}
	del := selectByGlob(paths, glob)
	if len(del) != 2 {
		t.Fatalf("mezcla: esperaba borrar 2, got %d (%v)", len(del), del)
	}
	for _, i := range del {
		if !matchGlob(glob, paths[i]) {
			t.Errorf("selectByGlob marcó para borrar un path fuera del glob: %s", paths[i])
		}
	}

	// todas dentro del glob -> conserva 1, borra el resto
	allIn := []string{"/x/tmp/photorec_1/a", "/x/tmp/photorec_2/a"}
	del2 := selectByGlob(allIn, glob)
	if len(del2) != 1 {
		t.Fatalf("todas-en-glob: esperaba borrar 1 (conservar 1), got %d", len(del2))
	}

	// ninguna en el glob -> no borra nada
	noneIn := []string{"/x/restored/a", "/x/other/a"}
	if del3 := selectByGlob(noneIn, glob); len(del3) != 0 {
		t.Fatalf("ninguna-en-glob: esperaba 0 borrados, got %d", len(del3))
	}
}

func TestHeadlessRemoveByGlob(t *testing.T) {
	dir := t.TempDir()
	// dos copias idénticas: una en tmp/photorec_ (a borrar), otra en restored (a conservar)
	victim := filepath.Join(dir, "tmp", "photorec_1", "f.a")
	keep := filepath.Join(dir, "restored", "f.a")
	writeFile(t, victim, "same bytes")
	writeFile(t, keep, "same bytes")
	group := []DupGroup{{Size: 10, Files: []ScannedFile{{Path: victim, Size: 10}, {Path: keep, Size: 10}}}}

	deleted, err := HeadlessDelete(nil, nil, group, func(string) []ScannedFile { return nil },
		&Config{RemoveByGlob: "*/tmp/photorec_*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || !deleted[victim] {
		t.Fatalf("debía borrar solo el de photorec; deleted=%v", deleted)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("la copia fuera del glob debe conservarse: %v", err)
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Errorf("la copia en photorec debe borrarse")
	}
}
