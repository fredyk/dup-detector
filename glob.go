package main

import (
	"regexp"
	"strings"
	"sync"
)

var globCache sync.Map // glob string -> *regexp.Regexp

// matchGlob reports whether path matches a shell-like glob where '*' matches any
// run of characters INCLUDING '/', and '?' matches a single character. The
// pattern is anchored (it must match the whole path), so use leading/trailing
// '*' for substring matches, e.g. "*photorec*" or "*/SECURE5/tmp/*".
func matchGlob(glob, path string) bool {
	return compileGlob(glob).MatchString(path)
}

func compileGlob(glob string) *regexp.Regexp {
	if v, ok := globCache.Load(glob); ok {
		return v.(*regexp.Regexp)
	}
	var b strings.Builder
	b.WriteByte('^')
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteByte('.')
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteByte('$')
	re := regexp.MustCompile(b.String())
	globCache.Store(glob, re)
	return re
}

// selectByGlob picks which copies of a duplicate group to delete when removing
// by glob: every path matching glob is a deletion target, EXCEPT that at least
// one copy always survives.
//   - some paths don't match  → delete ALL matching ones (a non-matching copy survives)
//   - ALL paths match         → keep the first, delete the rest
//   - none match              → delete nothing
//
// Returns 0-based indices into paths to delete.
func selectByGlob(paths []string, glob string) []int {
	var matched, unmatched []int
	for i, p := range paths {
		if matchGlob(glob, p) {
			matched = append(matched, i)
		} else {
			unmatched = append(unmatched, i)
		}
	}
	switch {
	case len(matched) == 0:
		return nil
	case len(unmatched) >= 1:
		return matched
	default:
		return matched[1:] // all match — keep the first
	}
}
