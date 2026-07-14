package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// trashFile moves path into the freedesktop.org trash of the filesystem that
// actually holds it, writing the matching .trashinfo record, and returns the
// destination path inside the trash.
//
// The trash is chosen PER MOUNT so the move is always a same-filesystem rename
// (fast, atomic, never a cross-device copy):
//   - files on the same filesystem as $HOME  → $XDG_DATA_HOME/Trash (home trash)
//   - files on any other mounted volume       → <mountpoint>/.Trash-$uid
//
// That per-drive detection is the whole point: deleting a duplicate on /tank
// must land in /tank's trash, not the home trash on another device.
func trashFile(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	loc, err := trashLocationFor(abs)
	if err != nil {
		return "", err
	}
	filesDir := filepath.Join(loc.dir, "files")
	infoDir := filepath.Join(loc.dir, "info")
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(infoDir, 0o700); err != nil {
		return "", err
	}

	// Pick a name that collides with neither an existing trashed file nor its
	// info record (two duplicates often share a basename).
	base := filepath.Base(abs)
	name := base
	dest := filepath.Join(filesDir, name)
	infoPath := filepath.Join(infoDir, name+".trashinfo")
	for i := 1; ; i++ {
		_, e1 := os.Lstat(dest)
		_, e2 := os.Lstat(infoPath)
		if os.IsNotExist(e1) && os.IsNotExist(e2) {
			break
		}
		name = fmt.Sprintf("%s.%d", base, i)
		dest = filepath.Join(filesDir, name)
		infoPath = filepath.Join(infoDir, name+".trashinfo")
	}

	// Write the info record BEFORE moving (spec: a crash must never leave a
	// trashed file with no way to know where it came from).
	record := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n",
		loc.recordPath(abs), time.Now().Format("2006-01-02T15:04:05"))
	if err := os.WriteFile(infoPath, []byte(record), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(abs, dest); err != nil {
		os.Remove(infoPath) // roll back the now-orphaned record
		return "", fmt.Errorf("moving to trash %s: %w", loc.dir, err)
	}
	return dest, nil
}

// trashLocation is a resolved trash directory. topDir is empty for the home
// trash and the mountpoint for a per-volume trash; it decides whether the
// recorded Path is absolute (home) or relative to the mount (spec requirement).
type trashLocation struct {
	dir    string
	topDir string
}

func (l trashLocation) recordPath(abs string) string {
	p := abs
	if l.topDir != "" {
		if rel, err := filepath.Rel(l.topDir, abs); err == nil {
			p = rel
		}
	}
	return urlEncodePath(p)
}

// trashLocationFor resolves the correct trash directory for abs.
func trashLocationFor(abs string) (trashLocation, error) {
	home, _ := os.UserHomeDir()
	if home != "" && sameFilesystem(abs, home) {
		return trashLocation{dir: filepath.Join(xdgDataHome(home), "Trash")}, nil
	}
	top, err := mountPointOf(abs)
	if err != nil {
		return trashLocation{}, err
	}
	return trashLocation{
		dir:    filepath.Join(top, fmt.Sprintf(".Trash-%d", os.Getuid())),
		topDir: top,
	}, nil
}

func xdgDataHome(home string) string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return x
	}
	return filepath.Join(home, ".local", "share")
}

// urlEncodePath percent-encodes each path segment while preserving the '/'
// separators, as the freedesktop trash spec requires for the Path field.
func urlEncodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// deviceOf returns the st_dev of path, falling back to its parent dir when the
// path itself no longer exists (e.g. resolving the mount of a to-be-created file).
func deviceOf(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		if err2 := syscall.Stat(filepath.Dir(path), &st); err2 != nil {
			return 0, err
		}
	}
	return uint64(st.Dev), nil
}

func sameFilesystem(a, b string) bool {
	da, err1 := deviceOf(a)
	db, err2 := deviceOf(b)
	return err1 == nil && err2 == nil && da == db
}

// mountPointOf ascends from path while the parent stays on the same device; the
// last directory before the device changes (or "/") is the filesystem's mount root.
func mountPointOf(path string) (string, error) {
	dev, err := deviceOf(path)
	if err != nil {
		return "", err
	}
	cur := filepath.Clean(path)
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur, nil // reached "/"
		}
		pdev, perr := deviceOf(parent)
		if perr != nil || pdev != dev {
			return cur, nil // parent is a different filesystem → cur is the mount root
		}
		cur = parent
	}
}
