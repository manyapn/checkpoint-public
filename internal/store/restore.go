package store

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/manyapn/checkpoint-public/internal/objstore"
)

// Restore materializes manifest m's PRIMARY tree into targetDir from the object
// store. Dirs are created parents-first; every referenced blob must be present or
// Restore errors (a manifest that cannot fully restore is not DURABLE).
// targetDir need not exist. Extra protected folders are NOT touched; they
// restore in place only, via RestoreExtras (an explicit, separate step: a caller
// restoring the workspace to an inspection copy must not clobber live external
// folders as a side effect).
func Restore(m *Manifest, oc *objstore.Store, targetDir string) error {
	return restoreTree(m.Entries, oc, targetDir)
}

// RestoreExact restores m's primary tree into targetDir AND removes paths the
// checkpoint does not contain, so the result is the tree byte-exact, the
// guarantee a whole-checkpoint restore makes. Returns the paths removed
// (relative to targetDir) so the caller can report them; the caller is
// expected to have cut a pre-restore checkpoint first, which is what makes
// removal recoverable. keptExceptions are the paths it deliberately did NOT
// touch because the manifest names them as exceptions. The caller must report
// them as "could not restore, left alone" (see pruneToManifest).
//
// What it must NEVER remove, and why:
//
//   - The default-skipped rebuildable directories (node_modules, .git, build, …)
//     are ABSENT FROM EVERY MANIFEST BY DESIGN, so "not in the manifest" does not
//     mean "not wanted" for them. Deleting them would destroy exactly the state
//     checkpoint promises never to touch. They are skipped whole-subtree.
//   - Anything the manifest NAMES as an exception (a fifo/device, an unreadable
//     file): also absent from Entries by design, and (unlike an ordinary
//     untracked file) NOT recoverable from the pre-restore checkpoint, which
//     records it as an exception too. Removing it would be irreversible.
//
// Use Restore (not RestoreExact) for a partial restore, where "not in the
// manifest" means nothing at all.
func RestoreExact(m *Manifest, oc *objstore.Store, targetDir string) (removed []string, keptExceptions []string, err error) {
	if err := restoreTree(m.Entries, oc, targetDir); err != nil {
		return nil, nil, err
	}
	return pruneToManifest(m, targetDir)
}

// pruneToManifest removes everything under targetDir that m.Entries does not
// describe, skipping default-excluded trees and every path m names as an
// exception (see RestoreExact). Directories that are themselves unwanted are
// removed whole and not descended into, unless they contain a named exception,
// in which case the directory survives (an exception must stay reachable) and
// its other children are pruned individually.
//
// Returns the removed paths and the named exceptions it found and preserved,
// both relative to targetDir, both sorted.
func pruneToManifest(m *Manifest, targetDir string) (removed []string, keptExceptions []string, err error) {
	entries := m.Entries
	protected := exceptionRels(m)
	sep := string(filepath.Separator)
	// protectedAt reports the exception covering rel (rel itself or an ancestor).
	protectedAt := func(rel string) (string, bool) {
		if protected[rel] {
			return rel, true
		}
		for p := range protected {
			if strings.HasPrefix(rel, p+sep) {
				return p, true
			}
		}
		return "", false
	}
	// holdsProtected reports whether rel is an ancestor DIRECTORY of an exception.
	holdsProtected := func(rel string) bool {
		for p := range protected {
			if strings.HasPrefix(p, rel+sep) {
				return true
			}
		}
		return false
	}

	var victims []string
	kept := map[string]bool{}
	err = filepath.WalkDir(targetDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == targetDir {
			return nil //nolint:nilerr // an unreadable entry is not ours to delete
		}
		rel, relErr := filepath.Rel(targetDir, p)
		if relErr != nil {
			return nil
		}
		// Never descend into a default-skipped tree, and never remove one.
		if d.IsDir() && defaultExcludes[filepath.Base(p)] {
			return filepath.SkipDir
		}
		// Never remove a NAMED exception (or anything beneath it): the manifest
		// says it exists and could not be captured, so deleting it is a loss no
		// checkpoint can undo.
		if ex, ok := protectedAt(rel); ok {
			kept[ex] = true
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if _, wanted := entries[rel]; wanted {
			return nil
		}
		if d.IsDir() && holdsProtected(rel) {
			return nil // keep the ancestor so the exception under it survives; prune its other children
		}
		victims = append(victims, rel)
		if d.IsDir() {
			return filepath.SkipDir // removed whole; children need no separate visit
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(victims)
	for _, rel := range victims {
		full, err := safeJoin(targetDir, rel)
		if err != nil {
			return nil, nil, err
		}
		if err := os.RemoveAll(full); err != nil {
			return nil, nil, fmt.Errorf("restore: removing %s: %w", rel, err)
		}
	}
	for rel := range kept {
		keptExceptions = append(keptExceptions, rel)
	}
	sort.Strings(keptExceptions)
	return victims, keptExceptions, nil
}

// exceptionRels is the set of PRIMARY-TREE paths m names as exceptions.
// Exceptions under an extra protected folder are recorded as ABSOLUTE paths
// (unambiguous across roots) and are irrelevant to a primary-tree prune, so
// they are dropped here, along with any path that would escape the target.
func exceptionRels(m *Manifest) map[string]bool {
	sep := string(filepath.Separator)
	out := map[string]bool{}
	for _, ex := range m.Exceptions {
		p := filepath.Clean(ex.Path)
		if p == "." || p == ".." || filepath.IsAbs(p) || strings.HasPrefix(p, ".."+sep) {
			continue
		}
		out[p] = true
	}
	return out
}

// RestoreExtras materializes every extra protected folder in m back to its
// absolute root (in place). No-op when the manifest has none. Deliberately
// materialize-only (no pruning): extra roots are live directories outside the
// workspace, where "absent from the manifest" is far weaker evidence of
// unwantedness than it is inside the project.
func RestoreExtras(m *Manifest, oc *objstore.Store) error {
	var roots []string
	for r := range m.Extra {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	for _, r := range roots {
		if err := restoreTree(m.Extra[r], oc, r); err != nil {
			return fmt.Errorf("extra root %s: %w", r, err)
		}
	}
	return nil
}

// safeJoin resolves rel under targetDir, refusing anything that escapes it.
// Manifests are built with filepath.Rel so entries are already contained; this
// is defense in depth against a hand-edited or hostile manifest.
func safeJoin(targetDir, rel string) (string, error) {
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", fmt.Errorf("refusing manifest entry %q: escapes the restore target", rel)
	}
	return filepath.Join(targetDir, clean), nil
}

// ensureDirPath makes every component of relDir under targetDir a REAL
// directory. A component that exists but is not a directory is removed first,
// notably a SYMLINK, which os.MkdirAll would happily follow, letting a restore
// write outside the target. The manifest is authoritative for the
// tree it describes: if it says this path is a directory, a symlink sitting
// there is stale state to replace, never a route to follow.
func ensureDirPath(targetDir, relDir string, mode fs.FileMode) error {
	if relDir == "." || relDir == "" {
		return os.MkdirAll(targetDir, 0o755)
	}
	cur := targetDir
	for _, comp := range strings.Split(filepath.Clean(relDir), string(filepath.Separator)) {
		if comp == "" || comp == "." {
			continue
		}
		cur = filepath.Join(cur, comp)
		fi, err := os.Lstat(cur)
		switch {
		case err == nil && fi.IsDir():
			// already a real directory
		case err == nil:
			if err := os.Remove(cur); err != nil { // symlink / file in the way
				return fmt.Errorf("restore: replacing non-directory %s: %w", cur, err)
			}
			if err := os.Mkdir(cur, mode); err != nil {
				return err
			}
		default:
			if err := os.Mkdir(cur, mode); err != nil && !os.IsExist(err) {
				return err
			}
		}
	}
	return nil
}

// clearDst removes whatever currently occupies dst so a file/symlink entry is
// written to the path itself rather than THROUGH an existing symlink (which
// would escape the target) or onto a directory.
func clearDst(dst string) error {
	fi, err := os.Lstat(dst)
	if err != nil {
		return nil // absent: nothing to clear
	}
	if fi.IsDir() {
		return os.RemoveAll(dst)
	}
	return os.Remove(dst)
}

func restoreTree(entries map[string]Entry, oc *objstore.Store, targetDir string) error {
	// A symlinked target root is refused for the same reason a symlinked
	// protected root is: every write (and, for RestoreExact, every removal)
	// would land somewhere other than the path the user named.
	if err := checkNotSymlinkRoot("restore target", targetDir); err != nil {
		return err
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}
	// dirs first, shallowest to deepest
	var dirs []string
	for rel, e := range entries {
		if e.Kind == KindDir {
			dirs = append(dirs, rel)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.Count(dirs[i], "/") < strings.Count(dirs[j], "/") })
	for _, rel := range dirs {
		if _, err := safeJoin(targetDir, rel); err != nil {
			return err
		}
		if err := ensureDirPath(targetDir, rel, fs.FileMode(entries[rel].Mode)&os.ModePerm|0o700); err != nil {
			return err
		}
	}
	for rel, e := range entries {
		dst, err := safeJoin(targetDir, rel)
		if err != nil {
			return err
		}
		switch e.Kind {
		case KindFile:
			content, err := oc.Get(e.Ref)
			if err != nil {
				return fmt.Errorf("restore %s: %w", rel, err)
			}
			if err := ensureDirPath(targetDir, filepath.Dir(rel), 0o755); err != nil {
				return err
			}
			if err := clearDst(dst); err != nil { // never write through a symlink
				return err
			}
			if err := os.WriteFile(dst, content, fs.FileMode(e.Mode)); err != nil {
				return err
			}
			if err := os.Chmod(dst, fs.FileMode(e.Mode)); err != nil { // WriteFile is umask'd
				return err
			}
		case KindSymlink:
			if err := ensureDirPath(targetDir, filepath.Dir(rel), 0o755); err != nil {
				return err
			}
			if err := clearDst(dst); err != nil {
				return err
			}
			if err := os.Symlink(e.Link, dst); err != nil {
				return err
			}
		}
	}
	// re-apply dir modes after children exist
	for _, rel := range dirs {
		if err := os.Chmod(filepath.Join(targetDir, rel), fs.FileMode(entries[rel].Mode)&os.ModePerm); err != nil {
			return err
		}
	}
	return nil
}
