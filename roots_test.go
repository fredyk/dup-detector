package main

import "testing"

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
