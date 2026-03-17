package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// ParseSize parses rsync-style size strings: 100, 1k, 1.5M, 2G, 1T
// Returns 0 for empty string.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	multiplier := int64(1)
	switch strings.ToLower(string(s[len(s)-1])) {
	case "k":
		multiplier = 1024
		s = s[:len(s)-1]
	case "m":
		multiplier = 1024 * 1024
		s = s[:len(s)-1]
	case "g":
		multiplier = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	case "t":
		multiplier = 1024 * 1024 * 1024 * 1024
		s = s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * float64(multiplier)), nil
}

// FormatSize returns a human-readable size string.
func FormatSize(bytes int64) string {
	if bytes < 0 {
		return "?"
	}
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// IsSubdir returns true if child is inside parent (not equal).
// Assumes both paths are absolute and clean (no /../ components).
func IsSubdir(parent, child string) bool {
	if len(child) <= len(parent) {
		return false
	}
	return strings.HasPrefix(child, parent) && child[len(parent)] == filepath.Separator
}
