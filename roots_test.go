package main

import (
	"sync"
	"testing"
)

// TestMemoizeDirCounter: the underlying counter is called at most once per
// distinct dir (the #18 CPU fix — CountUnderDir was recomputed per candidate
// pair), and values are returned correctly.
func TestMemoizeDirCounter(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	base := func(d string) int {
		mu.Lock()
		calls[d]++
		mu.Unlock()
		return len(d)
	}
	memo := memoizeDirCounter(base)
	for i := 0; i < 50; i++ {
		if memo("/a") != 2 {
			t.Fatalf("memo(/a) = %d, want 2", memo("/a"))
		}
		if memo("/bbb") != 4 {
			t.Fatalf("memo(/bbb) = %d, want 4", memo("/bbb"))
		}
	}
	if calls["/a"] != 1 || calls["/bbb"] != 1 {
		t.Fatalf("underlying called more than once per dir: %v", calls)
	}
	if memoizeDirCounter(nil) != nil {
		t.Fatal("memoizeDirCounter(nil) should be nil")
	}
}

// TestSpansMultipleSources locks the N-roots generalization of the old
// two-source "has both sides" check. The previous implementation special-cased
// Source==0, so a dup group living entirely in roots 2 and 3 (no Source 0) was
// wrongly reported as single-source — breaking cross-root detection for >2 roots.
func TestSpansMultipleSources(t *testing.T) {
	mk := func(srcs ...int) []ScannedFile {
		fs := make([]ScannedFile, len(srcs))
		for i, s := range srcs {
			fs[i] = ScannedFile{Source: s}
		}
		return fs
	}
	cases := []struct {
		name string
		in   []ScannedFile
		want bool
	}{
		{"empty", mk(), false},
		{"single", mk(0), false},
		{"same-source-0", mk(0, 0, 0), false},
		{"same-source-2", mk(2, 2), false},
		{"0-and-1", mk(0, 1), true},
		{"2-and-3-no-zero", mk(2, 3), true}, // the N-roots case the old code missed
		{"3-3-5", mk(3, 3, 5), true},
	}
	for _, c := range cases {
		if got := spansMultipleSources(c.in); got != c.want {
			t.Errorf("%s: spansMultipleSources=%v want %v", c.name, got, c.want)
		}
	}
}
