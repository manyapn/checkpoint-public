// Package undo implements checkpoint's revert: undo the agent's changes since a
// baseline checkpoint while preserving human work. The revert is scoped by
// AUTHOR rather than by whole checkpoint, which is possible because authorship
// is recorded per file: it is driven by the provenance ledger (the per-write
// record of who wrote what) rather than by diffing trees.
//
// The safety rule: only files whose every captured write in the window was the
// agent's are reverted. A file the human (or an unknown writer) also touched is
// a conflict: it is skipped whole-file and reported, never auto-merged. A file
// with no agent write at all is not the agent's change and is ignored. The
// safe direction is to under-revert (leave an agent file alone), never to
// over-revert (touch a human file).
//
// The caller MUST auto-checkpoint the present before Apply, so undo is itself
// undoable (restore-by-id back to the pre-undo snapshot).
//
// Boundary: deletions carry provenance only where the filesystem exposes a
// dirent change feed. Without one (overlayfs, for instance) an agent DELETION
// has no recorded writer, so it never becomes an undo candidate; restoring it
// requires naming the checkpoint explicitly.
package undo

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/oplog"
	"github.com/manyapn/checkpoint-public/internal/store"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

// Action is one path's planned treatment.
type Action string

const (
	Restore  Action = "restore"       // rewrite the baseline content (agent-only modify)
	Delete   Action = "delete"        // remove a path the agent created (absent at baseline)
	Conflict Action = "skip-conflict" // agent + human/unknown both wrote it: leave it, report it
)

// Other names who, besides the agent, wrote a conflicted path. It exists so
// reporting can stay truthful: telling a user "you also changed it" when the
// competing write came from a process checkpoint could not identify asserts
// something about them that is not known to be true.
type Other string

const (
	OtherHuman   Other = "human"   // a write attributed to the user
	OtherUnknown Other = "unknown" // a write checkpoint could not attribute
	OtherBoth    Other = "both"    // both kinds landed on the same path
)

// Entry is one path's planned treatment. Path is absolute; Rel is relative to the
// workspace root.
type Entry struct {
	Path   string
	Rel    string
	Action Action
	Target *store.Entry // baseline state for Restore; nil for Delete/Conflict
	// Other is set only for Conflict: who besides the agent wrote this path.
	Other Other
}

// Plan is a computed, not-yet-applied undo. Building it mutates nothing.
type Plan struct {
	Entries []Entry
}

// Result reports what Apply did, honestly.
type Result struct {
	Restored  []string `json:"restored"`
	Deleted   []string `json:"deleted"`
	Conflicts []string `json:"conflicts"`
	// ConflictWith maps each conflicted path to who else wrote it ("human",
	// "unknown", or "both"), so a caller can report the reason rather than
	// guessing at it. Never null.
	ConflictWith map[string]Other `json:"conflict_with"`
	Errors       []string         `json:"errors,omitempty"`
}

// BuildPlan computes the undo for the agent's changes. baseline is the pre-turn
// checkpoint (may be nil if the turn was the first checkpoint). window is the
// provenance ledger restricted to writes since baseline (the caller filters by
// time). root is the workspace root the version paths sit under. only, if
// non-empty, restricts the plan to those relative paths.
func BuildPlan(baseline *store.Manifest, window []versionlog.Version, root string, only []string) *Plan {
	type acc struct{ agent, human, unknown bool }
	byRel := map[string]*acc{}
	for _, v := range window {
		rel, ok := relOf(v.Path, root)
		if !ok {
			continue
		}
		a := byRel[rel]
		if a == nil {
			a = &acc{}
			byRel[rel] = a
		}
		switch v.Writer {
		case agentWriter:
			a.agent = true
		case selfWriter:
			// Checkpoint's OWN restore/undo write. Not the agent's change and
			// not the human's: counting it as "other" would conflict the very
			// file a previous `undo --only` just reverted, making selective
			// undo one-shot (a file could never be addressed again).
		case humanWriter:
			a.human = true
		default:
			// Unknown or unattributed. Treated exactly like a human write for
			// safety (the path is never touched), but reported differently:
			// checkpoint does not know that the user made this change.
			a.unknown = true
		}
	}

	keep := map[string]bool{}
	for _, p := range only {
		keep[p] = true
	}

	var rels []string
	for rel, a := range byRel {
		if !a.agent { // not the agent's change: not ours to undo
			continue
		}
		if len(keep) > 0 && !keep[rel] {
			continue
		}
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	p := &Plan{}
	for _, rel := range rels {
		a := byRel[rel]
		e := Entry{Path: filepath.Join(root, rel), Rel: rel}
		switch {
		case a.human || a.unknown:
			e.Action = Conflict
			switch {
			case a.human && a.unknown:
				e.Other = OtherBoth
			case a.human:
				e.Other = OtherHuman
			default:
				e.Other = OtherUnknown
			}
			// Carry the baseline version (when one exists) so save-both can
			// materialize the checkpoint side without re-deriving the plan,
			// for files and symlinks alike (materialize handles both kinds).
			// Apply ignores Target for conflicts, since the file is never
			// touched.
			if baseline != nil {
				if te, ok := baseline.Entries[rel]; ok &&
					(te.Kind == store.KindFile || te.Kind == store.KindSymlink) {
					t := te
					e.Target = &t
				}
			}
		default:
			if baseline != nil {
				if te, ok := baseline.Entries[rel]; ok && te.Kind == store.KindFile {
					t := te
					e.Action = Restore
					e.Target = &t
					break
				}
			}
			// agent-only and not a restorable file at baseline: the agent created
			// it (or the baseline held a non-file there) -> remove it.
			e.Action = Delete
		}
		p.Entries = append(p.Entries, e)
	}
	return p
}

const (
	agentWriter = "agent"
	// humanWriter matches provenance.Human.String(): a write attributed to a
	// process outside every registered agent tree.
	humanWriter = "human"
	// selfWriter matches provenance.Self.String(): a write made by checkpoint
	// itself while restoring or undoing.
	selfWriter = "checkpoint"
)

// Apply executes the plan against the workspace. The caller must have already
// snapshotted the present (auto-checkpoint-first) so this is undoable. Restores
// run before deletes; deletes go deepest-first so agent-created trees unwind
// child-before-parent. Partial failures land in Errors, never silently.
func Apply(p *Plan, oc *objstore.Store, root string) *Result {
	res := &Result{}
	var restores, deletes []Entry
	for _, e := range p.Entries {
		switch e.Action {
		case Conflict:
			res.Conflicts = append(res.Conflicts, e.Rel)
			if res.ConflictWith == nil {
				res.ConflictWith = map[string]Other{}
			}
			res.ConflictWith[e.Rel] = e.Other
		case Restore:
			restores = append(restores, e)
		case Delete:
			deletes = append(deletes, e)
		}
	}

	for _, e := range restores {
		if err := materialize(oc, *e.Target, e.Path); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", e.Rel, err))
			continue
		}
		res.Restored = append(res.Restored, e.Rel)
	}
	sort.Slice(deletes, func(i, j int) bool {
		return strings.Count(deletes[i].Rel, "/") > strings.Count(deletes[j].Rel, "/")
	})
	for _, e := range deletes {
		if err := os.Remove(e.Path); err != nil && !os.IsNotExist(err) {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", e.Rel, err))
			continue
		}
		res.Deleted = append(res.Deleted, e.Rel)
	}
	return res
}

// MaterializeConflicts implements save-both: for every Conflict in the plan
// that has a baseline version, the checkpoint side is written ALONGSIDE the
// live file as <path><suffix>. The live (human-touched) content is never
// modified, so both versions survive and the user decides later. An EXISTING
// sibling is never overwritten, because the user may have merged their
// resolution into it; it is reported in kept instead. Conflicts with no
// baseline version (the agent created the file this turn, then the human
// edited it) have nothing to materialize and are reported as skipped.
func MaterializeConflicts(p *Plan, oc *objstore.Store, suffix string) (saved, skipped, kept, errs []string) {
	for _, e := range p.Entries {
		if e.Action != Conflict {
			continue
		}
		if e.Target == nil {
			skipped = append(skipped, e.Rel)
			continue
		}
		dst := e.Path + suffix
		if _, err := os.Lstat(dst); err == nil {
			kept = append(kept, dst)
			continue
		}
		if err := materialize(oc, *e.Target, dst); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", e.Rel, err))
			continue
		}
		saved = append(saved, dst)
	}
	return saved, skipped, kept, errs
}

// materialize writes a baseline manifest entry to dst (file content + mode, or a
// symlink), replacing whatever is there. Never writes through an existing symlink
// or onto a directory.
func materialize(oc *objstore.Store, e store.Entry, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	switch e.Kind {
	case store.KindFile:
		content, err := oc.Get(e.Ref)
		if err != nil {
			return err
		}
		if fi, err := os.Lstat(dst); err == nil {
			if fi.IsDir() {
				return fmt.Errorf("a directory occupies %s; not replacing it", dst)
			}
			if err := os.Remove(dst); err != nil {
				return err
			}
		}
		if err := os.WriteFile(dst, content, fs.FileMode(e.Mode)); err != nil {
			return err
		}
		return os.Chmod(dst, fs.FileMode(e.Mode))
	case store.KindSymlink:
		if _, err := os.Lstat(dst); err == nil {
			if err := os.Remove(dst); err != nil {
				return err
			}
		}
		return os.Symlink(e.Link, dst)
	default:
		return fmt.Errorf("unrestorable baseline kind %q", e.Kind)
	}
}

// ReplayJournal finishes an interrupted operation from its journal alone:
// every action is executed idempotently from its journaled payload (a restore
// rewrites the same bytes, a delete of a gone path is a no-op, a save-both
// sibling that already exists is left untouched). No plan is recomputed: the
// journal is the authority, because the interrupted run's own writes have
// already polluted the provenance window.
func ReplayJournal(actions []oplog.Action, oc *objstore.Store) *Result {
	res := &Result{}
	for _, a := range actions {
		switch a.Do {
		case "delete":
			if err := os.Remove(a.Path); err != nil && !os.IsNotExist(err) {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", a.Path, err))
				continue
			}
			res.Deleted = append(res.Deleted, a.Path)
		case "restore-file", "restore-symlink", "save-both":
			kind := a.Kind
			if kind == "" {
				kind = store.KindFile
			}
			if a.Do == "save-both" {
				if _, err := os.Lstat(a.Path); err == nil {
					continue // sibling exists (possibly hand-merged): never overwrite
				}
			}
			e := store.Entry{Kind: kind, Ref: a.Ref, Mode: a.Mode, Link: a.Link}
			if err := materialize(oc, e, a.Path); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", a.Path, err))
				continue
			}
			res.Restored = append(res.Restored, a.Path)
		default:
			// restore-dir etc. from a restore journal: rerunning `restore` is
			// the sanctioned retry there; nothing to do per-action here.
		}
	}
	return res
}

// relOf returns abs relative to root, or ok=false if abs is not under root.
func relOf(abs, root string) (string, bool) {
	prefix := root + "/"
	if !strings.HasPrefix(abs, prefix) {
		return "", false
	}
	return abs[len(prefix):], true
}
