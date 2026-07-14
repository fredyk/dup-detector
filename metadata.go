package main

import (
	"encoding/json"
	"os"
)

// sidecarSuffix is appended to a file's name to form its metadata sidecar, e.g.
// "photo.jpg" -> "photo.jpg.dup-detector-metadata.json". One sidecar per file,
// co-located with it, so it travels with the file and is self-describing.
const sidecarSuffix = ".dup-detector-metadata.json"

// FileMetadata is the trusted, precomputed record stored in a file's sidecar.
// It lets -c dedupe (and other passes) avoid re-reading — crucially, avoid
// re-downloading from a remote mount — a file whose size+mtime still match.
type FileMetadata struct {
	Size      int64          `json:"size"`
	ModTime   int64          `json:"mtime"` // Unix seconds, matches ScannedFile.ModTime
	MD5       string         `json:"md5"`
	ChunkSize int64          `json:"chunk_size,omitempty"`
	ChunkMD5  []string       `json:"chunk_md5,omitempty"`
	Media     *MediaMetadata `json:"media,omitempty"`
}

func sidecarPathFor(file string) string { return file + sidecarSuffix }

// writeSidecar writes meta next to file (file + sidecarSuffix), pretty-printed.
func writeSidecar(file string, meta FileMetadata) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(sidecarPathFor(file), b, 0o644)
}

// readSidecar loads and parses the sidecar for file.
func readSidecar(file string) (FileMetadata, error) {
	var meta FileMetadata
	b, err := os.ReadFile(sidecarPathFor(file))
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

// sidecarMD5 returns the recorded MD5 for f when a FRESH sidecar exists — fresh
// meaning its recorded size and mtime match the file the scan actually saw.
// Inode is deliberately NOT checked: sidecars are meant for remote mounts
// (rclone/gdrive/S3) where inode numbers are synthetic and unstable. A mismatch
// (or missing/corrupt sidecar) returns ok=false so the caller reads the file.
func sidecarMD5(f ScannedFile) (string, bool) {
	meta, err := readSidecar(f.Path)
	if err != nil {
		return "", false
	}
	if meta.MD5 != "" && meta.Size == f.Size && meta.ModTime == f.ModTime {
		return meta.MD5, true
	}
	return "", false
}
