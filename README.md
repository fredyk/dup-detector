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

```bash
git clone <repo>
cd dup-detector
go build -o dup-detector .
cp dup-detector ~/bin/
```

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
| `--progress` | | false | Show progress during scan |
| `--exclude PATTERN` | | | Exclude files/dirs matching pattern (repeatable) |
| `--exclude-from FILE` | | | Read exclude patterns from file |
| `--include PATTERN` | | | Include files matching pattern, overriding excludes (repeatable) |
| `--include-from FILE` | | | Read include patterns from file |
| `--min-size SIZE` | | | Skip files smaller than SIZE (e.g. `1k`, `10M`, `1G`) |
| `--max-size SIZE` | | | Skip files larger than SIZE |
| `--format FORMAT` | | `columns` | Output format: `columns`, `json`, `csv`, `simple` |

## Comparison modes

**Default (size + mtime):** fast, good for comparing backups where files were copied preserving timestamps. May produce false positives for files with identical size and modification time but different content.

**Checksum mode (`-c`, size + MD5):** slower but accurate. MD5 is only computed for files that share the same size (pre-filtered), so it's much faster than computing MD5 for everything.

## Interactive deletion

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
