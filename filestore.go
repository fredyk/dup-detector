package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	_ "modernc.org/sqlite"
)

// FileStore is an on-disk SQLite table of scanned files. It replaces holding
// the full []ScannedFile in RAM: the walk streams rows in, and the dedup phases
// query them back (size-collision groups, files-under-dir prefix ranges, an
// ordered full scan for tree hashing). Peak RAM becomes O(working set) instead
// of O(total files) — the whole point of this store.
//
// It is a throwaway scratch DB (journal/sync OFF): if the process dies the data
// is meaningless, so we trade durability for speed.
type FileStore struct {
	db     *sql.DB
	path   string
	insert *sql.Stmt
	tx     *sql.Tx
	n      int
}

const fsBatch = 50000 // rows per insert transaction

// CleanStaleStores removes leftover scan DBs from earlier runs that died before
// Close() (kill/OOM). A file dup-detector-scan-<pid>.db is stale iff <pid> is no
// longer running. Best-effort; ignores errors.
func CleanStaleStores(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "dup-detector-scan-*.db"))
	for _, m := range matches {
		var pid int
		if _, err := fmt.Sscanf(filepath.Base(m), "dup-detector-scan-%d.db", &pid); err != nil {
			continue
		}
		if pid > 0 && processAlive(pid) {
			continue // owned by a running scan
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(m + suffix)
		}
	}
}

// processAlive reports whether pid is a live process (Linux /proc check).
func processAlive(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

// NewFileStore creates a fresh scratch DB at path (removing any prior file).
func NewFileStore(path string) (*FileStore, error) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	// temp_store(FILE): the CREATE INDEX at Finalize sorts the whole table; with
	// temp_store(MEMORY) that sort would spike RAM on a 10M+ row scan — exactly
	// what this store exists to avoid. Keep temp on disk.
	db, err := sql.Open("sqlite",
		"file:"+path+"?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)&_pragma=temp_store(FILE)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; avoids OFF-journal concurrency issues
	if _, err := db.Exec(`CREATE TABLE files(
		path TEXT NOT NULL, relpath TEXT, size INTEGER NOT NULL,
		mtime INTEGER, inode INTEGER, source INTEGER)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create table: %w", err)
	}
	insert, err := db.Prepare(`INSERT INTO files(path,relpath,size,mtime,inode,source) VALUES(?,?,?,?,?,?)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	fs := &FileStore{db: db, path: path, insert: insert}
	if fs.tx, err = db.Begin(); err != nil {
		db.Close()
		return nil, err
	}
	return fs, nil
}

// Add streams one file into the store. Inserts are batched in a transaction and
// committed every fsBatch rows to bound the WAL/dirty-page memory.
func (fs *FileStore) Add(f ScannedFile) error {
	if _, err := fs.tx.Stmt(fs.insert).Exec(f.Path, f.RelPath, f.Size, f.ModTime, f.Inode, f.Source); err != nil {
		return err
	}
	fs.n++
	if fs.n%fsBatch == 0 {
		if err := fs.tx.Commit(); err != nil {
			return err
		}
		var err error
		if fs.tx, err = fs.db.Begin(); err != nil {
			return err
		}
	}
	return nil
}

// Finalize commits the last batch and builds the query indexes. Indexes are
// created AFTER the bulk insert (much faster than maintaining them per-row).
func (fs *FileStore) Finalize() error {
	if fs.tx != nil {
		if err := fs.tx.Commit(); err != nil {
			return err
		}
		fs.tx = nil
	}
	if _, err := fs.db.Exec(`CREATE INDEX idx_size ON files(size)`); err != nil {
		return err
	}
	if _, err := fs.db.Exec(`CREATE INDEX idx_path ON files(path)`); err != nil {
		return err
	}
	// Write phase is over (read-only from here): allow concurrent connections so
	// parallel tree-pair verification isn't serialized on a single conn.
	fs.db.SetMaxOpenConns(runtime.NumCPU())
	return nil
}

// Count returns the number of stored files.
func (fs *FileStore) Count() (int, error) {
	var n int
	err := fs.db.QueryRow(`SELECT count(*) FROM files`).Scan(&n)
	return n, err
}

func scanRows(rows *sql.Rows) ([]ScannedFile, error) {
	defer rows.Close()
	var out []ScannedFile
	for rows.Next() {
		var f ScannedFile
		if err := rows.Scan(&f.Path, &f.RelPath, &f.Size, &f.ModTime, &f.Inode, &f.Source); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CollisionSizes returns every size shared by ≥2 files, largest first.
// (Files with a unique size can never be content duplicates.)
func (fs *FileStore) CollisionSizes() ([]int64, error) {
	rows, err := fs.db.Query(`SELECT size FROM files GROUP BY size HAVING count(*) >= 2 ORDER BY size DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sizes []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		sizes = append(sizes, s)
	}
	return sizes, rows.Err()
}

// FilesWithSize returns all files of exactly the given size.
func (fs *FileStore) FilesWithSize(size int64) ([]ScannedFile, error) {
	rows, err := fs.db.Query(
		`SELECT path,relpath,size,mtime,inode,source FROM files WHERE size = ?`, size)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// IterAllByPath streams every file ordered by path (for tree-hash accumulation).
// Only one row is materialized at a time → constant RAM.
func (fs *FileStore) IterAllByPath(fn func(ScannedFile) error) error {
	rows, err := fs.db.Query(`SELECT path,relpath,size,mtime,inode,source FROM files ORDER BY path`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var f ScannedFile
		if err := rows.Scan(&f.Path, &f.RelPath, &f.Size, &f.ModTime, &f.Inode, &f.Source); err != nil {
			return err
		}
		if err := fn(f); err != nil {
			return err
		}
	}
	return rows.Err()
}

// FilesUnderDir returns every file whose path is under dir/ (a prefix range
// query, indexed on path). Mirrors the slice-based filesUnderDir.
func (fs *FileStore) FilesUnderDir(dir string) ([]ScannedFile, error) {
	prefix := filepath.Clean(dir) + "/"
	// Upper bound: replace the trailing '/' (0x2F) with '0' (0x30) so the half-open
	// range [prefix, hi) captures exactly the paths beginning with prefix.
	hi := prefix[:len(prefix)-1] + "0"
	rows, err := fs.db.Query(
		`SELECT path,relpath,size,mtime,inode,source FROM files
		 WHERE path >= ? AND path < ? ORDER BY path`, prefix, hi)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// Close drops the connection and deletes the scratch file.
func (fs *FileStore) Close() error {
	if fs.insert != nil {
		fs.insert.Close()
	}
	err := fs.db.Close()
	_ = os.Remove(fs.path)
	_ = os.Remove(fs.path + "-wal")
	_ = os.Remove(fs.path + "-shm")
	return err
}

// SizeCount pairs a colliding size with how many files share it.
type SizeCount struct {
	Size  int64
	Count int
}

// CollisionSizeCounts returns (size,count) for every size shared by ≥2 files,
// largest first. Used to compute total bytes for progress without loading files.
func (fs *FileStore) CollisionSizeCounts() ([]SizeCount, error) {
	rows, err := fs.db.Query(
		`SELECT size, count(*) FROM files GROUP BY size HAVING count(*) >= 2 ORDER BY size DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SizeCount
	for rows.Next() {
		var sc SizeCount
		if err := rows.Scan(&sc.Size, &sc.Count); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ScanToStore walks root exactly like Scan but streams accepted files into the
// store instead of accumulating a slice. source (0=dir A, 1=dir B) is stamped on
// every row. onFile still fires for progressive work.
func ScanToStore(store *FileStore, root string, cfg *Config, absExcludes []string,
	seenInodes map[[2]uint64]struct{}, source int, onFile func(ScannedFile)) error {
	return scanWalk(root, cfg, absExcludes, seenInodes, func(sf ScannedFile) error {
		sf.Source = source
		if err := store.Add(sf); err != nil {
			return err
		}
		if onFile != nil {
			onFile(sf)
		}
		return nil
	})
}
