package main

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// HashCache memoizes file MD5s keyed by path, validated by (size, mtime, inode).
// A cached hash is reused as long as those three are unchanged, so repeated -c
// runs over the same tree skip re-reading file contents entirely. This is a
// per-file hash cache, not a per-command result cache: a file hashed once stays
// valid across every invocation regardless of flags (--min-size, two-dir, etc.).
//
// Access is per-operation against a WAL-mode SQLite database: every lookup is a
// SELECT and every freshly computed hash is UPSERTed immediately. Because WAL
// makes committed writes visible to other connections at once, concurrent
// dup-detector processes share hashes live — a file hashed by one run is reused
// by another seconds later, with SQLite (not a custom daemon) serializing the
// writes. busy_timeout (set per connection via the DSN) lets concurrent writers
// wait their turn instead of failing; an in-process write mutex keeps this
// process's own worker goroutines from contending with each other.
type HashCache struct {
	db     *sql.DB
	path   string
	rehash bool
	now    int64

	sel *sql.Stmt // SELECT md5,size,mtime,inode WHERE path=?
	ups *sql.Stmt // INSERT ... ON CONFLICT(path) DO UPDATE

	wmu sync.Mutex // serialize this process's writes

	cmu    sync.Mutex // guards counters
	hits   int
	misses int
}

// DefaultCachePath returns ~/.cache/dup-detector/hashes.db, honoring
// XDG_CACHE_HOME. When invoked through sudo (so the effective HOME is /root),
// it resolves the *invoking* user's home from $SUDO_USER instead, so the cache
// lives in the real user's ~/.cache and is shared across runs.
func DefaultCachePath() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home := invokingUserHome()
		if home == "" {
			return ""
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "dup-detector", "hashes.db")
}

// invokingUserHome returns the home dir of the real invoking user, preferring
// $SUDO_USER when running as root under sudo.
func invokingUserHome() string {
	if os.Geteuid() == 0 {
		if su := os.Getenv("SUDO_USER"); su != "" && su != "root" {
			if u, err := user.Lookup(su); err == nil && u.HomeDir != "" {
				return u.HomeDir
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// sudoOwner returns the uid/gid of the invoking sudo user, or ok=false if not
// running under sudo. Used to chown cache files back to the real user.
func sudoOwner() (uid, gid int, ok bool) {
	if os.Geteuid() != 0 {
		return 0, 0, false
	}
	us, gs := os.Getenv("SUDO_UID"), os.Getenv("SUDO_GID")
	if us == "" || gs == "" {
		return 0, 0, false
	}
	u, err1 := strconv.Atoi(us)
	g, err2 := strconv.Atoi(gs)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return u, g, true
}

// chownToInvoker best-effort chowns a path (and SQLite sidecar files) back to
// the sudo-invoking user, so a root run doesn't leave root-owned files in the
// user's home. Errors are ignored.
func chownToInvoker(path string) {
	uid, gid, ok := sudoOwner()
	if !ok {
		return
	}
	for _, p := range []string{path, path + "-wal", path + "-shm", filepath.Dir(path)} {
		_ = os.Chown(p, uid, gid)
	}
}

// isBusyErr reports whether err is a transient SQLite lock/busy condition.
func isBusyErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") ||
		strings.Contains(s, "database is locked") ||
		strings.Contains(s, "(5)") || strings.Contains(s, "(517)")
}

// execRetry runs a statement, retrying for up to ~15s while the database is
// locked by another connection or process. Used only for the cold-start init
// steps (journal_mode switch, schema create) that race across processes.
func execRetry(db *sql.DB, query string) error {
	var err error
	for i := 0; i < 60; i++ {
		if _, err = db.Exec(query); err == nil || !isBusyErr(err) {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return err
}

// OpenHashCache opens (creating if needed) the WAL-mode SQLite hash cache at
// path. rehash=true forces recomputation of all hashes (existing rows ignored
// on lookup but still overwritten). The DSN sets WAL + busy_timeout on every
// pooled connection so concurrent writers wait rather than error.
func OpenHashCache(path string, rehash bool) (*HashCache, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	chownToInvoker(path) // hand the cache dir back to the sudo-invoking user

	// busy_timeout/synchronous are set per connection via the DSN so every
	// pooled connection inherits them. journal_mode=WAL is a *persistent*
	// database property, so we set it once below (with retry) rather than in
	// the DSN — only the first cold opener needs to switch it.
	dsn := "file:" + path +
		"?_busy_timeout=10000" +
		"&_synchronous=NORMAL"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// Switching journal mode and creating the schema both take a write lock.
	// On a cold start with several dup-detector processes racing, one can hit
	// SQLITE_BUSY before busy_timeout engages — retry those init steps.
	if err := execRetry(db, `PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, err
	}
	if err := execRetry(db, `CREATE TABLE IF NOT EXISTS hashes(
		path  TEXT PRIMARY KEY,
		size  INTEGER NOT NULL,
		mtime INTEGER NOT NULL,
		inode INTEGER NOT NULL,
		md5   TEXT NOT NULL,
		seen  INTEGER NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}

	sel, err := db.Prepare(`SELECT md5, size, mtime, inode FROM hashes WHERE path = ?`)
	if err != nil {
		db.Close()
		return nil, err
	}
	ups, err := db.Prepare(`INSERT INTO hashes(path,size,mtime,inode,md5,seen)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET
			size=excluded.size, mtime=excluded.mtime, inode=excluded.inode,
			md5=excluded.md5, seen=excluded.seen`)
	if err != nil {
		sel.Close()
		db.Close()
		return nil, err
	}

	return &HashCache{
		db:     db,
		path:   path,
		rehash: rehash,
		now:    time.Now().Unix(),
		sel:    sel,
		ups:    ups,
	}, nil
}

// Hash returns the MD5 of f, reusing the cached value when the file is unchanged
// (same path, size, mtime, inode). On a miss it computes the hash and writes it
// back immediately so concurrent processes can reuse it. Safe for concurrent
// use; a nil *HashCache falls back to an uncached read.
func (c *HashCache) Hash(f ScannedFile) (string, error) {
	if c == nil {
		return md5File(f.Path)
	}

	if !c.rehash {
		var md5 string
		var size, mtime, inode int64
		err := c.sel.QueryRow(f.Path).Scan(&md5, &size, &mtime, &inode)
		switch {
		case err == nil:
			if size == f.Size && mtime == f.ModTime && inode == f.Inode {
				c.bump(&c.hits)
				return md5, nil
			}
			// stale row — fall through to recompute and overwrite
		case errors.Is(err, sql.ErrNoRows):
			// not cached — fall through to compute
		default:
			// Any DB read error: degrade gracefully to an uncached compute.
		}
	}

	h, err := md5File(f.Path)
	if err != nil {
		return "", err
	}
	c.store(f, h)
	c.bump(&c.misses)
	return h, nil
}

// store UPSERTs a freshly computed hash. Writes are serialized in-process by
// wmu; cross-process contention is absorbed by busy_timeout. A transient write
// failure is non-fatal: the hash is still returned, just not persisted.
func (c *HashCache) store(f ScannedFile, md5 string) {
	c.wmu.Lock()
	_, err := c.ups.Exec(f.Path, f.Size, f.ModTime, f.Inode, md5, c.now)
	c.wmu.Unlock()
	if err != nil && !errors.Is(err, driver.ErrBadConn) {
		// Best-effort cache; ignore write errors (e.g. SQLITE_BUSY past timeout).
		_ = err
	}
}

func (c *HashCache) bump(p *int) {
	c.cmu.Lock()
	*p++
	c.cmu.Unlock()
}

// Stats returns the number of cache hits and misses this run.
func (c *HashCache) Stats() (hits, misses int) {
	if c == nil {
		return 0, 0
	}
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return c.hits, c.misses
}

// Close releases prepared statements and the database. Writes are already
// durable (committed per-operation), so there is nothing to flush.
func (c *HashCache) Close() error {
	if c == nil {
		return nil
	}
	if c.sel != nil {
		c.sel.Close()
	}
	if c.ups != nil {
		c.ups.Close()
	}
	err := c.db.Close()
	chownToInvoker(c.path) // -wal/-shm may have appeared during writes
	return err
}
