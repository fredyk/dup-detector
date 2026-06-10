package main

import (
	"runtime"
	"sort"
	"sync"
)

// ProgressiveHasher computes MD5s for size-colliding files *as they are
// discovered* during the directory walk, overlapping hashing I/O with the
// traversal instead of waiting for the whole walk to finish.
//
// It preserves the size-prefilter optimization: a file is only hashed once a
// second file of the same size appears (at which point the first is hashed
// retroactively). Files with a unique size are never read.
//
// OnFile must be called from a single goroutine (the walk). Worker goroutines
// only touch the result slice, guarded by mu — so the size-tracking maps need
// no locking.
type ProgressiveHasher struct {
	cache  *HashCache
	twoDir bool

	jobs chan ScannedFile
	wg   sync.WaitGroup

	// Walk-goroutine-only state (no locking needed):
	sizeFirst map[int64]ScannedFile // first file seen at a size, pending activation
	active    map[int64]bool        // size has ≥2 files → all get hashed

	mu      sync.Mutex
	results []hashedFile
	errs    int
}

type hashedFile struct {
	f   ScannedFile
	md5 string
}

// NewProgressiveHasher starts a worker pool and returns the hasher. Call OnFile
// for each discovered file (with its source: 0=dir A, 1=dir B), then Close to
// drain and collect.
func NewProgressiveHasher(cache *HashCache, workers int, twoDir bool) *ProgressiveHasher {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	ph := &ProgressiveHasher{
		cache:     cache,
		twoDir:    twoDir,
		jobs:      make(chan ScannedFile, 1024),
		sizeFirst: make(map[int64]ScannedFile),
		active:    make(map[int64]bool),
	}
	for i := 0; i < workers; i++ {
		ph.wg.Add(1)
		go ph.worker()
	}
	return ph
}

func (ph *ProgressiveHasher) worker() {
	defer ph.wg.Done()
	for f := range ph.jobs {
		h, err := ph.cache.Hash(f)
		ph.mu.Lock()
		if err != nil {
			ph.errs++
		} else {
			ph.results = append(ph.results, hashedFile{f, h})
		}
		ph.mu.Unlock()
	}
}

// OnFile gates a freshly discovered file on size collisions and dispatches
// hashing when warranted. source is 0 for dir A, 1 for dir B.
func (ph *ProgressiveHasher) OnFile(f ScannedFile, source int) {
	f.Source = source
	if ph.active[f.Size] {
		ph.jobs <- f
		return
	}
	if first, ok := ph.sizeFirst[f.Size]; ok {
		// Second file at this size → activate and hash both.
		ph.active[f.Size] = true
		delete(ph.sizeFirst, f.Size)
		ph.jobs <- first
		ph.jobs <- f
		return
	}
	ph.sizeFirst[f.Size] = f
}

// Close stops accepting work, waits for all hashing to finish, and returns the
// duplicate groups (files grouped by identical size+MD5). In two-dir mode only
// groups spanning both sides are kept.
func (ph *ProgressiveHasher) Close() []DupGroup {
	close(ph.jobs)
	ph.wg.Wait()

	type key struct {
		size int64
		md5  string
	}
	byKey := make(map[key][]ScannedFile)
	for _, r := range ph.results {
		k := key{r.f.Size, r.md5}
		byKey[k] = append(byKey[k], r.f)
	}

	var groups []DupGroup
	for k, fs := range byKey {
		if len(fs) < 2 {
			continue
		}
		if ph.twoDir && !hasBothSides(fs) {
			continue
		}
		groups = append(groups, DupGroup{Size: k.size, Files: fs})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Size > groups[j].Size })
	return groups
}
