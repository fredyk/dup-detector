package main

import (
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/spf13/cobra"
)

// defaultChunkBytes is the streaming buffer and per-chunk hashing granularity.
const defaultChunkBytes int64 = 64 << 20 // 64 MiB

// copyFileWithMetadata copies src → dst while hashing the stream once, verifies
// the destination by reading it back through a byte-range read (cheap on
// rclone/gdrive/S3), and only on success writes dst's trusted sidecar. mtime is
// preserved so a later scan validates the sidecar by size+mtime. Returns the
// metadata written.
//
// verifyFull=false re-reads only the LAST chunk of the destination (confirms the
// upload completed and its tail is intact) — the cheap default; true re-reads
// every chunk (dobles the transfer but proves the whole object byte-for-byte).
func copyFileWithMetadata(src, dst string, chunkSize int64, verifyFull bool) (FileMetadata, error) {
	if chunkSize <= 0 {
		chunkSize = defaultChunkBytes
	}
	in, err := os.Open(src)
	if err != nil {
		return FileMetadata{}, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return FileMetadata{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return FileMetadata{}, err
	}
	out, err := os.Create(dst)
	if err != nil {
		return FileMetadata{}, err
	}

	global := md5.New()
	var chunkMD5 []string
	buf := make([]byte, chunkSize)
	copyErr := func() error {
		for {
			n, rerr := io.ReadFull(in, buf)
			if n > 0 {
				chunk := buf[:n]
				if _, werr := out.Write(chunk); werr != nil {
					return werr
				}
				global.Write(chunk)
				chunkMD5 = append(chunkMD5, fmt.Sprintf("%x", md5.Sum(chunk)))
			}
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				return nil
			}
			if rerr != nil {
				return rerr
			}
		}
	}()
	if copyErr != nil {
		out.Close()
		os.Remove(dst)
		return FileMetadata{}, copyErr
	}
	if err := out.Close(); err != nil { // flush; on rclone this drives the upload
		os.Remove(dst)
		return FileMetadata{}, err
	}
	// Preserve mtime so the sidecar validates by size+mtime later.
	if err := os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
		// Non-fatal: some backends reject atime; the mtime is what matters and
		// most rclone backends honor it. If it didn't stick, validation later
		// simply falls back to reading the file — safe degradation.
		_ = err
	}

	if err := verifyReadback(dst, chunkSize, chunkMD5, verifyFull); err != nil {
		os.Remove(dst)
		return FileMetadata{}, err
	}

	meta := FileMetadata{
		Size:      info.Size(),
		ModTime:   info.ModTime().Unix(),
		MD5:       fmt.Sprintf("%x", global.Sum(nil)),
		ChunkSize: chunkSize,
		ChunkMD5:  chunkMD5,
		Media:     extractMediaMetadata(dst),
	}
	if err := writeSidecar(dst, meta); err != nil {
		return FileMetadata{}, err
	}
	return meta, nil
}

// verifyReadback re-reads the destination through ReadAt (a byte-range read,
// which rclone translates into a ranged GET so only the requested bytes are
// fetched) and checks each read chunk's md5 against the value computed while
// writing. In tail mode only the final chunk is re-read.
func verifyReadback(dst string, chunkSize int64, chunkMD5 []string, full bool) error {
	if len(chunkMD5) == 0 {
		return nil
	}
	f, err := os.Open(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, chunkSize)
	check := func(i int) error {
		off := int64(i) * chunkSize
		n, err := f.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		got := fmt.Sprintf("%x", md5.Sum(buf[:n]))
		if got != chunkMD5[i] {
			return fmt.Errorf("read-back verification failed: chunk %d of %s (got %s, want %s)",
				i, dst, got, chunkMD5[i])
		}
		return nil
	}
	if full {
		for i := range chunkMD5 {
			if err := check(i); err != nil {
				return err
			}
		}
		return nil
	}
	return check(len(chunkMD5) - 1)
}

// copyPath copies a file or directory tree from src to dst, writing a verified
// sidecar for every regular file. Existing sidecar files in src are skipped
// (they are regenerated for the destination).
func copyPath(src, dst string, chunkSize int64, verifyFull bool, transfers int, onFile func(string, FileMetadata)) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		meta, err := copyFileWithMetadata(src, dst, chunkSize, verifyFull)
		if err == nil && onFile != nil {
			onFile(dst, meta)
		}
		return err
	}
	if transfers < 1 {
		transfers = 1
	}

	// Fixed pool of `transfers` workers copying files concurrently. On a remote
	// mount each file is a round-trip with real latency, so overlapping N copies
	// is ~N× faster. A single file's failure is logged and skipped (not fatal):
	// a whole snapshot shouldn't abort for one bad object — the caller can retry.
	type job struct{ path, target string }
	jobs := make(chan job, transfers*2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i := 0; i < transfers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				meta, err := copyFileWithMetadata(j.path, j.target, chunkSize, verifyFull)
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					fmt.Fprintf(os.Stderr, "  copy error: %s: %v\n", j.path, err)
				} else if onFile != nil {
					onFile(j.target, meta) // serialized under mu → onFile need not be thread-safe
				}
				mu.Unlock()
			}
		}()
	}

	walkErr := filepath.WalkDir(src, func(path string, d os.DirEntry, we error) error {
		if we != nil {
			return we
		}
		if d.IsDir() || !d.Type().IsRegular() || hasSidecarSuffix(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		jobs <- job{path, filepath.Join(dst, rel)}
		return nil
	})
	close(jobs)
	wg.Wait()

	if walkErr != nil {
		return walkErr
	}
	return firstErr
}

func hasSidecarSuffix(name string) bool {
	return len(name) > len(sidecarSuffix) && name[len(name)-len(sidecarSuffix):] == sidecarSuffix
}

// ── cobra subcommand ─────────────────────────────────────────────────────────

var (
	copyVerifyFull bool
	copyChunkMiB   int64
	copyTransfers  int
)

var copyCmd = &cobra.Command{
	Use:   "copy SRC DST",
	Short: "Copy a file/dir, writing a trusted .dup-detector-metadata.json sidecar per file",
	Long: `Copy SRC to DST, hashing each file once during the copy and verifying the
destination by reading it back (a cheap byte-range read on rclone/gdrive/S3).
On success each destination file gets a .dup-detector-metadata.json sidecar with
its size, mtime, global MD5 and per-chunk MD5s — plus width/height/duration/
capture-time for images and videos when ffprobe (or the Go image decoders) can
read them. A later 'dup-detector -c' trusts that sidecar (when size+mtime still
match) instead of re-reading — so cloud files dedupe without being downloaded.`,
	Args:          cobra.ExactArgs(2),
	RunE:          runCopy,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func runCopy(_ *cobra.Command, args []string) error {
	src, dst := args[0], args[1]
	chunk := copyChunkMiB << 20
	if chunk <= 0 {
		chunk = defaultChunkBytes
	}
	var n int
	var bytes int64
	err := copyPath(src, dst, chunk, copyVerifyFull, copyTransfers, func(target string, meta FileMetadata) {
		n++
		bytes += meta.Size
		if !cfg.Quiet {
			extra := ""
			if meta.Media != nil {
				if meta.Media.Width > 0 {
					extra = fmt.Sprintf("  [%dx%d]", meta.Media.Width, meta.Media.Height)
				} else if meta.Media.DurationSec > 0 {
					extra = fmt.Sprintf("  [%.1fs]", meta.Media.DurationSec)
				}
			}
			fmt.Fprintf(os.Stderr, "  copied %s%s\n", target, extra)
		}
	})
	if !cfg.Quiet {
		fmt.Fprintf(os.Stderr, "Copied %d file(s), %s, each with a verified sidecar.\n", n, FormatSize(bytes))
	}
	return err
}
