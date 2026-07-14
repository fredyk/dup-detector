package main

import (
	"fmt"
	"os"
)

// HeadlessDelete applies the same keep-first policy as the interactive "auto"
// mode, but without any prompts or stdin — for non-interactive / orchestrated
// runs (--headless). It honors cfg.DryRun (report only, touch nothing) and
// cfg.Trash (move to the per-filesystem trash instead of unlinking), both via
// the shared disposeFile primitive. Returns the set of disposed (or, in
// dry-run, would-be-disposed) paths so callers/tests can inspect the outcome.
func HeadlessDelete(treePairs []TreeDupPair, blocks []dirOverlapBlock, groups []DupGroup, lookup DirLookup, cfg *Config) (map[string]bool, error) {
	deleted := make(map[string]bool)
	if len(treePairs) == 0 && len(blocks) == 0 && len(groups) == 0 {
		return deleted, nil
	}
	actions := buildCleanupActions(treePairs, blocks, groups)

	if !cfg.Quiet {
		fmt.Fprintf(os.Stderr, "\nHeadless: keeping one copy per group across %d action(s) [%s].\n",
			len(actions), disposeModeLabel(cfg))
	}

	var skipped int
	for i := range actions {
		a := &actions[i]
		if reresolveAction(a, lookup, deleted) {
			skipped++
			continue
		}
		if cfg.RemoveByGlob != "" {
			applyGlobRemoval(a, lookup, deleted, cfg)
		} else {
			applyAuto(a, lookup, deleted, cfg)
		}
	}

	if !cfg.Quiet {
		if skipped > 0 {
			fmt.Fprintf(os.Stderr, "(%d action(s) skipped — already resolved by earlier deletions.)\n", skipped)
		}
		fmt.Fprintf(os.Stderr, "%s %d file(s) total.\n", disposedVerb(cfg), len(deleted))
	}
	return deleted, nil
}

// disposeModeLabel / disposedVerb describe the active disposal mode for logs.
func disposeModeLabel(cfg *Config) string {
	switch {
	case cfg.DryRun && cfg.Trash:
		return "dry-run, trash"
	case cfg.DryRun:
		return "dry-run"
	case cfg.Trash:
		return "move to trash"
	default:
		return "delete"
	}
}

func disposedVerb(cfg *Config) string {
	switch {
	case cfg.DryRun:
		return "Would dispose"
	case cfg.Trash:
		return "Trashed"
	default:
		return "Deleted"
	}
}
