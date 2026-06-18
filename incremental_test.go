package main

import "testing"

// TestAllCoveredByIndexSkipsSelf locks the self-exclusion in allCoveredByIndex.
// dupIndex now stores each file's whole group as a shared slice (O(k) memory),
// which includes the file itself. A file must NOT be treated as "covered" by its
// own presence under dir — coverage requires a *different* group member. Without
// the skip, a nested candidate pair (dir an ancestor of the file's dir) would
// falsely confirm, risking deletion of non-duplicate trees.
func TestAllCoveredByIndexSkipsSelf(t *testing.T) {
	files := []ScannedFile{{Path: "/a/sub/x"}}

	// Only member under /a/sub is the file itself → NOT covered.
	idxSelfOnly := map[string][]string{"/a/sub/x": {"/a/sub/x"}}
	if allCoveredByIndex(files, "/a/sub", idxSelfOnly) {
		t.Fatal("a file must not count as its own duplicate under dir")
	}

	// A genuine different duplicate under /a/sub → covered.
	idxReal := map[string][]string{"/a/sub/x": {"/a/sub/x", "/a/sub/y"}}
	if !allCoveredByIndex(files, "/a/sub", idxReal) {
		t.Fatal("a distinct group member under dir should cover the file")
	}
}

// buildNestedTreeFixture creates two byte-identical trees P and Q with a nested
// structure, so tree-dup detection has both a top-level pair (P,Q) and nested
// sub-pairs (P/common,Q/common), (P/sub,Q/sub) where the top pair dominates.
//
//	root/{P,Q}/
//	  common/{x,y}
//	  sub/{m,n}
func buildNestedTreeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, top := range []string{"P", "Q"} {
		writeFile(t, root+"/"+top+"/common/x", "alpha-content")
		writeFile(t, root+"/"+top+"/common/y", "beta-content-2")
		writeFile(t, root+"/"+top+"/sub/m", "gamma-content-3")
		writeFile(t, root+"/"+top+"/sub/n", "delta-content-44")
	}
	return root
}

// splitGroupsBySegment partitions content-dup groups into those whose files sit
// under .../seg/... and the rest, preserving order — used to drive AddGroups in
// a controlled multi-batch order.
func splitGroupsBySegment(groups []DupGroup, seg string) (match, rest []DupGroup) {
	needle := "/" + seg + "/"
	for _, g := range groups {
		under := len(g.Files) > 0
		for _, f := range g.Files {
			if !contains(f.Path, needle) {
				under = false
				break
			}
		}
		if under {
			match = append(match, g)
		} else {
			rest = append(rest, g)
		}
	}
	return match, rest
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func checksumGroupsOf(t *testing.T, files []ScannedFile) []DupGroup {
	t.Helper()
	var groups []DupGroup
	if err := ChecksumDups(files, nil, false, nil, 2, nil, nil,
		func(ng []DupGroup) bool { groups = append(groups, ng...); return true }); err != nil {
		t.Fatal(err)
	}
	return groups
}

// TestAddGroupsIncrementalOneShot pins the one-shot AddGroups result: when every
// dup group is known up front, the top-level pair dominates and the nested
// sub-pairs are removed. This is the dedup logic shared by FindTreeDups.
func TestAddGroupsIncrementalOneShot(t *testing.T) {
	root := buildNestedTreeFixture(t)
	files := scanDir(t, root)
	SortFilesByPath(files)
	groups := checksumGroupsOf(t, files)

	pairs := FindTreeDups(groups, files)
	got := pairSet(pairs)
	want := map[string]bool{"P|Q": true}
	sameSet(t, "one-shot tree dups", want, got)
}

// TestAddGroupsIncrementalAcrossBatches characterizes the incremental behavior
// and is the regression lock for the #13 perf refactor (verified to produce the
// identical Confirmed set as the pre-refactor code). Feeding "common" groups
// before "sub" groups yields {(P/common,Q/common), (P/sub,Q/sub)} — NOT the
// dominating top pair (P,Q). Why: candidate generation walks up ancestors, so
// (P,Q) is already proposed and verified in batch 1, fails (dupIndex doesn't yet
// cover sub/*), and is marked in checkedPairs — so it is never re-verified in
// batch 2 even though dupIndex is then complete. This order-dependent limitation
// is pre-existing; the refactor preserves it exactly. (One-shot, where dupIndex
// is complete up front, correctly collapses to (P,Q) — see the test above.)
func TestAddGroupsIncrementalAcrossBatches(t *testing.T) {
	root := buildNestedTreeFixture(t)
	files := scanDir(t, root)
	SortFilesByPath(files)
	lookup := SliceLookup(files)
	groups := checksumGroupsOf(t, files)

	common, sub := splitGroupsBySegment(groups, "common")
	if len(common) == 0 || len(sub) == 0 {
		t.Fatalf("fixture split failed: common=%d sub=%d (groups=%d)", len(common), len(sub), len(groups))
	}

	st := NewTreeDupState()
	st.Workers = 2
	st.AddGroups(common, lookup, true)
	st.AddGroups(sub, lookup, true)

	got := pairSet(st.Confirmed)
	want := map[string]bool{"common|common": true, "sub|sub": true}
	sameSet(t, "incremental common-then-sub", want, got)
}

// TestAddConfirmedSeedsDominators verifies that pairs seeded via AddConfirmed
// (the early mtime/content tree pass) act as dominators for later AddGroups: a
// sub-pair discovered in the MD5 phase is dropped if its parent tree pair was
// already confirmed by the early pass.
func TestAddConfirmedSeedsDominators(t *testing.T) {
	root := buildNestedTreeFixture(t)
	files := scanDir(t, root)
	SortFilesByPath(files)
	lookup := SliceLookup(files)
	groups := checksumGroupsOf(t, files)

	st := NewTreeDupState()
	st.Workers = 2
	// Seed the dominating top pair as if the early pass had found it.
	st.AddConfirmed([]TreeDupPair{{DirA: root + "/P", DirB: root + "/Q", FileCount: 4}})

	newPairs := st.AddGroups(groups, lookup, true)
	if len(newPairs) != 0 {
		t.Fatalf("expected no new pairs (all dominated by seeded P|Q), got %v", pairSet(newPairs))
	}
	got := pairSet(st.Confirmed)
	want := map[string]bool{"P|Q": true}
	sameSet(t, "seeded dominator", want, got)
}
