package main

import (
	"path/filepath"
	"sort"
	"testing"
)

func groupSet(groups []DupGroup) map[string]bool {
	set := map[string]bool{}
	for _, g := range groups {
		set[joinSig(groupSig(g))] = true
	}
	return set
}

func pairSet(pairs []TreeDupPair) map[string]bool {
	set := map[string]bool{}
	for _, p := range pairs {
		s := []string{filepath.Base(p.DirA), filepath.Base(p.DirB)}
		sort.Strings(s)
		set[s[0]+"|"+s[1]] = true
	}
	return set
}

func sameSet(t *testing.T, name string, a, b map[string]bool) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: slice=%v store=%v", name, a, b)
	}
	for k := range a {
		if !b[k] {
			t.Fatalf("%s: store missing %q (slice=%v store=%v)", name, k, a, b)
		}
	}
}

func TestMtimeDupsStoreMatchesSlice(t *testing.T) {
	root := buildFixture(t)
	files := scanDir(t, root)
	sliceGroups := MtimeDups(files, nil)

	fs := buildStore(t, root)
	defer fs.Close()
	storeGroups, err := MtimeDupsStore(fs, false)
	if err != nil {
		t.Fatal(err)
	}
	sameSet(t, "MtimeDups", groupSet(sliceGroups), groupSet(storeGroups))
}

func TestChecksumDupsStoreMatchesSlice(t *testing.T) {
	root := buildFixture(t)
	files := scanDir(t, root)
	var sliceGroups []DupGroup
	if err := ChecksumDups(files, nil, false, nil, 2, nil, nil,
		func(ng []DupGroup) bool { sliceGroups = append(sliceGroups, ng...); return true }); err != nil {
		t.Fatal(err)
	}

	fs := buildStore(t, root)
	defer fs.Close()
	var storeGroups []DupGroup
	if err := ChecksumDupsStore(fs, false, nil, 2, nil, nil,
		func(ng []DupGroup) bool { storeGroups = append(storeGroups, ng...); return true }); err != nil {
		t.Fatal(err)
	}
	sameSet(t, "ChecksumDups", groupSet(sliceGroups), groupSet(storeGroups))
}

func TestFindTreeDupsStoreMatchesSlice(t *testing.T) {
	root := buildFixture(t)
	files := scanDir(t, root)
	SortFilesByPath(files)
	slicePairs := FindTreeDupsByHash(files, nil)

	fs := buildStore(t, root)
	defer fs.Close()
	storePairs, err := FindTreeDupsByHashStore(fs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sameSet(t, "FindTreeDups", pairSet(slicePairs), pairSet(storePairs))
}
