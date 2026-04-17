//go:build unix

package main

import (
	"os"
	"syscall"
)

// inodeKey returns the (Dev, Ino) tuple that uniquely identifies a file on
// disk. Two paths with the same key are hardlinks to the same inode —
// deleting one doesn't reclaim space while another link exists.
// Returns ok=false on platforms where stat info isn't a *syscall.Stat_t.
func inodeKey(info os.FileInfo) ([2]uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return [2]uint64{}, false
	}
	return [2]uint64{uint64(st.Dev), uint64(st.Ino)}, true
}
