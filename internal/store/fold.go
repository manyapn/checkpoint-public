package store

import (
	"github.com/manyapn/checkpoint-public/internal/objstore"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SnapshotFold builds the next manifest by FOLDING a change-set over prev
// instead of walking the whole tree: only paths named in dirtyByRoot (absolute,
// keyed by protected root: the primary root and any extras) are re-examined;
// everything else carries over from prev unchanged. O(changes), not O(tree).
//
// Correctness contract (pinned by TestFoldMatchesFullScan): given a COMPLETE
// change-set (every path created, written, deleted, or renamed since prev,
// including both names of a rename), the fold equals a full Snapshot. The
// change-feed guarantees completeness or reports a hole (Overflowed), in which
// case the caller must full-scan instead; folding over a change-set with a
// known hole is how deletions get silently resurrected.
//
// A dirty path that is now a directory has its whole subtree rescanned (a
// renamed-in directory's children generate no events of their own); a dirty
// path that is gone removes its entry and every descendant entry.
func SnapshotFold(root string, oc *objstore.Store, prev *Manifest, id int, timeNS int64, cov Coverage, missed int, dirtyByRoot map[string][]string, extras ...string) (*Manifest, error) {
	if err := checkNotSymlinkRoot("protected root", root); err != nil {
		return nil, err
	}
	for _, ex := range extras {
		if err := checkNotSymlinkRoot("extra protected folder", ex); err != nil {
			return nil, err
		}
	}
	m := &Manifest{ID: id, TimeNS: timeNS, Root: root, Coverage: cov, Missed: missed, Entries: map[string]Entry{}}
	except := func(path, reason string) {
		m.Exceptions = append(m.Exceptions, Exception{Path: path, Reason: reason})
	}

	var prevScanNS int64
	if prev != nil {
		prevScanNS = prev.ScanNS
	}

	foldOne := func(r string, prevEntries map[string]Entry, out map[string]Entry, exceptRel func(rel, reason string)) error {
		for rel, e := range prevEntries {
			out[rel] = e
		}
		dirty := append([]string(nil), dirtyByRoot[r]...)
		sort.Strings(dirty) // parents before children: a created dir's entry lands before its files'
		for _, abs := range dirty {
			rel, ok := relUnder(abs, r)
			if !ok || excludedRel(rel) {
				continue
			}
			// Credential material is never captured, on THIS path too. The
			// fold is what runs on change-feed filesystems (the recommended
			// configuration), so a denylist enforced only in scanTree protects
			// exactly the users who took the slower option. Skipped loudly, as
			// in scanTree: an unprotected file must be visible, never assumed.
			if IsSecretPath(abs) || IsSecretPath(rel) {
				exceptRel(rel, secretReason)
				continue
			}
			fi, err := os.Lstat(abs)
			if err != nil {
				// Gone: remove the entry and every descendant.
				delete(out, rel)
				pfx := rel + "/"
				for k := range out {
					if strings.HasPrefix(k, pfx) {
						delete(out, k)
					}
				}
				continue
			}
			switch {
			case fi.Mode()&fs.ModeSymlink != 0:
				target, err := os.Readlink(abs)
				if err != nil {
					exceptRel(rel, "unreadable symlink: "+err.Error())
					continue
				}
				out[rel] = Entry{Kind: KindSymlink, Link: target}
			case fi.IsDir():
				// Replace the whole subtree: rescan it (children of a moved-in
				// dir have no events of their own).
				pfx := rel + "/"
				for k := range out {
					if strings.HasPrefix(k, pfx) {
						delete(out, k)
					}
				}
				sub := map[string]Entry{}
				subPrev := map[string]Entry{}
				for k, e := range prevEntries {
					if strings.HasPrefix(k, pfx) {
						subPrev[strings.TrimPrefix(k, pfx)] = e
					}
				}
				if err := scanTree(abs, oc, subPrev, prevScanNS, sub, func(srel, reason string) {
					exceptRel(rel+"/"+srel, reason)
				}, nil); err != nil {
					return err
				}
				out[rel] = Entry{Kind: KindDir, Mode: unixMode(fi.Mode())}
				for k, e := range sub {
					out[pfx+k] = e
				}
			case fi.Mode().IsRegular():
				// No stat shortcut here, deliberately. Being in the dirty set IS
				// evidence that this path was written, so reusing prev's ref
				// because size+mtime happen to match would checkpoint content we
				// have positive reason to believe is stale (a same-length rewrite
				// with the timestamp put back is all it takes), silently. The
				// dirty set is the change-set, so reading it is O(what actually
				// changed), which is the fold's whole budget anyway.
				e := fileEntry(fi)
				content, err := readFile(abs)
				if err != nil {
					exceptRel(rel, "unreadable: "+err.Error())
					delete(out, rel)
					continue
				}
				ref, _, err := oc.Put(content)
				if err != nil {
					return err
				}
				e.Ref = ref
				out[rel] = e
			default:
				delete(out, rel)
				exceptRel(rel, "unsupported kind: "+fi.Mode().Type().String())
			}
		}
		return nil
	}

	var prevEntries map[string]Entry
	if prev != nil {
		prevEntries = prev.Entries
	}
	if err := foldOne(root, prevEntries, m.Entries, func(rel, reason string) { except(rel, reason) }); err != nil {
		return nil, err
	}
	for _, ex := range extras {
		var prevExtra map[string]Entry
		if prev != nil {
			prevExtra = prev.Extra[ex]
		}
		entries := map[string]Entry{}
		exRoot := ex
		if err := foldOne(ex, prevExtra, entries, func(rel, reason string) {
			except(filepath.Join(exRoot, rel), reason)
		}); err != nil {
			return nil, err
		}
		if m.Extra == nil {
			m.Extra = map[string]map[string]Entry{}
		}
		m.Extra[ex] = entries
	}
	sort.Slice(m.Exceptions, func(i, j int) bool { return m.Exceptions[i].Path < m.Exceptions[j].Path })
	m.ScanNS = time.Now().UnixNano() // capture finished: the next scan's trust anchor
	return m, nil
}

func relUnder(abs, root string) (string, bool) {
	if !strings.HasPrefix(abs, root+"/") {
		return "", false
	}
	return abs[len(root)+1:], true
}

// excludedRel reports whether any path component is bulk-excluded (mirrors
// scanTree's SkipDir on defaultExcludes).
func excludedRel(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if defaultExcludes[seg] {
			return true
		}
	}
	return false
}
