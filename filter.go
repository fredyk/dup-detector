package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// FilterRule is an rsync-compatible include/exclude rule.
type FilterRule struct {
	Pattern string
	Exclude bool // true = exclude, false = include (overrides a previous exclude)
}

// ShouldExclude returns true if relPath should be excluded.
// Rules are evaluated in order; the last matching rule wins (rsync semantics).
func ShouldExclude(relPath string, rules []FilterRule) bool {
	if len(rules) == 0 {
		return false
	}
	relPath = filepath.ToSlash(relPath)
	excluded := false
	for _, r := range rules {
		if r.matches(relPath) {
			excluded = r.Exclude
		}
	}
	return excluded
}

func (r FilterRule) matches(relPath string) bool {
	pattern := strings.TrimSuffix(r.Pattern, "/")
	if pattern == "" {
		return false
	}

	// Leading / → anchored to transfer root
	if strings.HasPrefix(pattern, "/") {
		m, _ := globMatch(pattern[1:], relPath)
		return m
	}

	// Pattern contains / → match against full relative path
	if strings.Contains(pattern, "/") {
		m, _ := globMatch(pattern, relPath)
		return m
	}

	// No slash → match against each path component (and full path for dirs)
	parts := strings.Split(relPath, "/")
	for _, part := range parts {
		if m, _ := globMatch(pattern, part); m {
			return true
		}
	}
	return false
}

// globMatch implements glob matching with ** support.
func globMatch(pattern, name string) (bool, error) {
	if !strings.Contains(pattern, "**") {
		return filepath.Match(pattern, name)
	}
	return matchDoubleStar(pattern, name), nil
}

func matchDoubleStar(pattern, name string) bool {
	if pattern == "**" {
		return true
	}
	if !strings.Contains(pattern, "**") {
		m, _ := filepath.Match(pattern, name)
		return m
	}

	idx := strings.Index(pattern, "**")
	before := strings.TrimSuffix(pattern[:idx], "/")
	after := strings.TrimPrefix(pattern[idx+2:], "/")

	nameParts := strings.Split(name, "/")

	if before != "" {
		beforeParts := strings.Split(before, "/")
		if len(beforeParts) > len(nameParts) {
			return false
		}
		for i, p := range beforeParts {
			if m, _ := filepath.Match(p, nameParts[i]); !m {
				return false
			}
		}
		remaining := strings.Join(nameParts[len(beforeParts):], "/")
		if after == "" {
			return true
		}
		return matchDoubleStar(after, remaining)
	}

	// No before: ** matches any prefix
	if after == "" {
		return true
	}
	for i := range nameParts {
		if matchDoubleStar(after, strings.Join(nameParts[i:], "/")) {
			return true
		}
	}
	return false
}

// LoadRulesFromFile reads filter rules from a file.
// Lines starting with # are comments.
// Lines like "+ pattern" = include rule; "- pattern" = exclude rule.
// Plain lines are treated as exclude (if defaultExclude=true) or include rules.
func LoadRulesFromFile(filename string, defaultExclude bool) ([]FilterRule, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var rules []FilterRule
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "+ "):
			rules = append(rules, FilterRule{Pattern: line[2:], Exclude: false})
		case strings.HasPrefix(line, "- "):
			rules = append(rules, FilterRule{Pattern: line[2:], Exclude: true})
		default:
			rules = append(rules, FilterRule{Pattern: line, Exclude: defaultExclude})
		}
	}
	return rules, sc.Err()
}
