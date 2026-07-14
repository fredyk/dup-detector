# dup-detector

rsync-like CLI tool to detect (and interactively delete) duplicate files between or within directories.

## Features

- **Single-dir mode** — find duplicates within one directory
- **Two-dir mode** — find files whose content exists in both directories (order doesn't matter)
- **Smart subdir detection** — if one dir is inside the other, it's automatically excluded from the parent scan to avoid false positives
- **rsync-compatible flags** — `--exclude`, `--exclude-from`, `--include`, `-c`, `-r`, `-v`, `-q`, `-n`, `--min-size`, `--max-size`, `--progress`
- **Two comparison modes** — size+mtime (fast, default) or size+MD5 (accurate, `-c`)
- **Multiple output formats** — `columns` (default), `json`, `csv`, `simple`
- **Interactive deletion** — after detection, prompts to delete duplicates sorted largest-first (e2fsck-style)

## Install

The SQLite driver is `mattn/go-sqlite3` (C-SQLite), so a C toolchain (gcc +
libc headers) is required — CGo must be enabled.

```bash
git clone <repo>
cd dup-detector
./install.sh          # builds with CGO_ENABLED=1 and runs `go install .`
```

## Profiling

The binary serves `net/http/pprof` (loopback-only, on by default) so a long run
can be profiled while it runs. It binds `127.0.0.1:8158` when free, otherwise an
OS-assigned free port — so multiple concurrent runs each get their own endpoint.
List the live ones:

```bash
dup-detector pprof-list
# pid=12345  http://127.0.0.1:8158/debug/pprof/   started=...
#     cmd: dup-detector /backups -c
```

Then profile a specific endpoint:

```bash
go tool pprof http://127.0.0.1:8158/debug/pprof/profile?seconds=30   # CPU
go tool pprof http://127.0.0.1:8158/debug/pprof/heap                 # RAM
curl  http://127.0.0.1:8158/debug/pprof/goroutine?debug=2            # stacks
```

Force a fixed address with `DUP_DETECTOR_PPROF=":6060"`, or disable with
`DUP_DETECTOR_PPROF=off`.

## Usage

```
dup-detector [OPTIONS] DIR_A [DIR_B]
```

### Examples

```bash
# Find duplicates within a single directory
dup-detector /media/backup

# Find duplicates between two directories (order doesn't matter)
dup-detector /media/backup/dev /media/projects/dev

# One dir is a subdirectory of the other — handled automatically
dup-detector /media/data /media/data/archive

# Use MD5 checksum for accurate comparison (no false positives)
dup-detector -c /media/backup /media/nas

# Exclude patterns (rsync-style)
dup-detector --exclude "*.tmp" --exclude ".git" --exclude "node_modules" DIR_A DIR_B

# Load exclude patterns from a file
dup-detector --exclude-from ~/.dup-ignore DIR_A DIR_B

# Only scan files larger than 10 MB
dup-detector --min-size 10M -c DIR_A DIR_B

# Detect only, no deletion prompt
dup-detector -n DIR_A DIR_B

# Output as JSON and pipe to a file
dup-detector -n --format json DIR_A DIR_B > duplicates.json

# Verbose with progress bar
dup-detector --progress -v -c DIR_A DIR_B
```

## Flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--checksum` | `-c` | false | Compare by size+MD5 instead of size+mtime |
| `--recursive` | `-r` | true | Recurse into subdirectories |
| `--verbose` | `-v` | false | Increase verbosity |
| `--quiet` | `-q` | false | Suppress status output |
| `--dry-run` | `-n` | false | Scan and report only; skip deletion prompt |
| `--headless` | | false | Non-interactive: auto keep-first, dispose the rest without prompts (combine with `-n` to preview) |
| `--trash` | | false | Move duplicates to the freedesktop trash of their own filesystem instead of unlinking (reversible) |
| `--remove-by-glob PATTERN` | | | Headless: delete the copies whose path matches the glob (e.g. `*/tmp/photorec_*`), always keeping ≥1 copy outside the glob. `*` spans `/`. Instead of keep-first, you choose which side dies. |
| `--progress` | | false | Show progress during scan |
| `--exclude PATTERN` | | | Exclude files/dirs matching pattern (repeatable) |
| `--exclude-from FILE` | | | Read exclude patterns from file |
| `--include PATTERN` | | | Include files matching pattern, overriding excludes (repeatable) |
| `--include-from FILE` | | | Read include patterns from file |
| `--min-size SIZE` | | | Skip files smaller than SIZE (e.g. `1k`, `10M`, `1G`) |
| `--max-size SIZE` | | | Skip files larger than SIZE |
| `--format FORMAT` | | `columns` | Output format: `columns`, `json`, `csv`, `simple` |
| `--no-cache` | | false | Disable the on-disk MD5 cache (always re-read file contents) |
| `--rehash` | | false | Ignore cached MD5s and recompute them (refreshes the cache) |
| `--cache-path FILE` | | | Path to the MD5 cache DB (default: `~/.cache/dup-detector/hashes.db`) |
| `--no-progressive` | | false | In `-c` mode, hash only after the full walk (don't overlap with it) |

## Metadata sidecars & `copy` (dedupe remote files without downloading)

Hashing a file for `-c` normally means reading its whole content — ruinous when
the files live on a remote mount (rclone/gdrive/S3), where every read is a
download. A **metadata sidecar** breaks that: a `<file>.dup-detector-metadata.json`
holding the file's `size`, `mtime`, global `md5` and per-chunk `md5`s (plus
`width`/`height`/`duration`/`capture_time` for images and videos). When `-c`
scans a file that has a **fresh** sidecar — one whose recorded `size`+`mtime`
still match — it trusts the recorded MD5 instead of reading the file. Stale or
missing sidecars are ignored (the file is read normally), so it is always safe.

Sidecars themselves are excluded from the scan, never treated as duplicates.

### `dup-detector copy SRC DST`

Generates those sidecars reliably, at the one moment the bytes are already
flowing — the copy:

```bash
dup-detector copy /local/photos /mnt/gdrive/photos
```

For each file it streams SRC → DST through a 64 MiB buffer, computing the global
and per-chunk MD5 as it goes; then it **reads the destination back** via a
byte-range read (a ranged GET on rclone/gdrive/S3, so only the needed bytes are
fetched) and checks it against the per-chunk hashes; only if that passes does it
write the sidecar. mtime is preserved so the sidecar validates later. For images
and videos it also records dimensions / duration / capture time (via `ffprobe`
when present, else the Go image decoders) so nothing has to re-parse them.

| Flag | Default | Description |
|------|---------|-------------|
| `--verify-full` | false | Read back and verify EVERY chunk of the destination (default: only the last chunk) |
| `--chunk-mib` | 64 | Chunk size in MiB for the streaming hash and read-back verification |
| `--transfers` | 1 | Copy this many files in parallel — hides per-file round-trip latency on remote mounts (rclone/gdrive/S3), ~N× faster. A single file's failure is logged and skipped, not fatal. |

A subsequent `dup-detector -c /mnt/gdrive/photos` then dedupes entirely from the
sidecars — it reports `N file(s) hashed without reading contents`.

## Comparison modes

**Default (size + mtime):** fast, good for comparing backups where files were copied preserving timestamps. **Misses real duplicates whose content is identical but whose mtime differs** (e.g. files re-downloaded, re-archived, or copied without `-p`/`--times`) — and may produce false positives for files with identical size and mtime but different content. Use `-c` for accuracy.

**Checksum mode (`-c`, size + MD5):** slower but accurate. MD5 is only computed for files that share the same size (pre-filtered), so it's much faster than computing MD5 for everything.

### Progressive hashing

In `-c` mode, hashing **overlaps with the directory walk** (and with the
tree-duplicate pass): as soon as a second file of a given size is discovered,
both are dispatched to a worker pool, so MD5 I/O runs concurrently with the
traversal instead of waiting for it to finish. The size pre-filter is preserved
— files with a unique size are never read. This shortens wall-clock most on a
cold cache; on a warm cache the walk dominates anyway. Disable with
`--no-progressive` to hash strictly after the walk (largest-first streaming).

## MD5 cache

In `-c` mode, every computed MD5 is cached in a small SQLite database
(`~/.cache/dup-detector/hashes.db` by default). A cached hash is keyed by file
path and validated by `(size, mtime, inode)`: on the next run, any file whose
metadata is unchanged is **not re-read** — its hash comes straight from the
cache. On a large, slow disk this turns a multi-hour second pass into little
more than the directory walk.

This is a **per-file hash cache, not a per-command result cache**: a file hashed
once stays valid across every invocation, regardless of flags (`--min-size`,
single- vs two-dir, different roots…). Concurrent runs share the cache safely
(WAL mode). Use `--rehash` to force recomputation (e.g. if you suspect an
in-place edit preserved size+mtime), or `--no-cache` to bypass it entirely.

## Interactive deletion

### Non-interactive (`--headless`)

For orchestrated/scripted runs where no TTY is available, `--headless` applies
the same **keep-first** policy the interactive `a` (auto) mode uses — keep one
copy per group (the safer backup-cadence side when paths differ only by cadence),
dispose the rest — with no prompts. It composes with the disposal modifiers:

```bash
# Preview exactly what would go, touching nothing:
dup-detector -c --headless --dry-run /backups

# Then do it for real, moving dupes to each filesystem's trash (reversible):
dup-detector -c --headless --trash /backups
```

`--trash` (usable in interactive mode too) moves each duplicate into the
freedesktop trash **of the filesystem that holds it** — `<mountpoint>/.Trash-$uid`
for a mounted volume, `$XDG_DATA_HOME/Trash` for the home filesystem — writing a
spec-compliant `.trashinfo` so a file manager can restore it. The move stays a
same-filesystem rename (never a cross-device copy).

### Interactive

After displaying results, you'll be prompted to delete duplicates interactively, starting from the largest files:

```
[1/42] 2.1 GB
  [1] /media/backup/dev/project.tar.gz
  [2] /media/nas/archive/project.tar.gz
  Delete [1], [2], [s]kip, [a]uto-keep-first, [q]uit, [?]help:
```

| Key | Action |
|-----|--------|
| `1`, `2`, ... | Delete that copy (keep the rest) |
| `1,3` | Delete copies 1 and 3 |
| `s` | Skip this group |
| `a` | Auto mode: keep [1], delete rest for this and all remaining groups |
| `q` | Quit without deleting further |
| `?` | Show help |

## Exclude file format

Same as rsync `--exclude-from` / `--filter` files:

```
# Comments start with #
*.log
*.tmp
.git
- node_modules      # explicit exclude
+ important.log     # include (overrides a previous exclude)
/absolute/from/root
**/any/depth
```
