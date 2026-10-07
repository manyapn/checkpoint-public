package snapshot

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/manyapn/checkpoint-public/internal/objects"
)

// Scan walks root and records every entry. Files whose size, mtime and ctime
// match prev are not re-read.
func Scan(root string, objs *objects.Store, prev *Checkpoint) (*Checkpoint, error) {
	if err := refuseSymlinkRoot(root); err != nil {
		return nil, err
	}
	c := newCheckpoint(root)
	prevEntries, prevScan := previous(prev)
	err := scanTree(root, "", objs, prevEntries, prevScan, c)
	if err != nil {
		return nil, err
	}
	c.finish()
	return c, nil
}

// Fold starts from prev and re-examines only the changed paths (absolute).
// Correct only when changed is the complete set since prev, which is what
// the change feed promises when it has not overflowed.
func Fold(root string, objs *objects.Store, prev *Checkpoint, changed []string) (*Checkpoint, error) {
	if err := refuseSymlinkRoot(root); err != nil {
		return nil, err
	}
	c := newCheckpoint(root)
	prevEntries, prevScan := previous(prev)
	for rel, e := range prevEntries {
		c.Entries[rel] = e
	}
	sort.Strings(changed)
	for _, abs := range changed {
		rel, ok := strings.CutPrefix(abs, root+"/")
		if !ok || excludedRel(rel) {
			continue
		}
		dropSubtree(c.Entries, rel)
		if IsSecret(abs) {
			c.except(rel, secretReason)
			continue
		}
		fi, err := os.Lstat(abs)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			c.Entries[rel] = Entry{Kind: Dir, Mode: unixMode(fi.Mode())}
			if err := scanTree(root, rel, objs, prevEntries, prevScan, c); err != nil {
				return nil, err
			}
			continue
		}
		c.record(abs, rel, fi, objs, nil, 0)
	}
	c.finish()
	return c, nil
}

func newCheckpoint(root string) *Checkpoint {
	return &Checkpoint{Root: root, TimeNS: time.Now().UnixNano(), Entries: map[string]Entry{}}
}

func previous(prev *Checkpoint) (map[string]Entry, int64) {
	if prev == nil {
		return nil, 0
	}
	return prev.Entries, prev.ScanNS
}

func (c *Checkpoint) finish() {
	sort.Slice(c.Exceptions, func(i, j int) bool { return c.Exceptions[i].Path < c.Exceptions[j].Path })
	c.ScanNS = time.Now().UnixNano()
}

func (c *Checkpoint) except(rel, reason string) {
	c.Exceptions = append(c.Exceptions, Exception{Path: rel, Reason: reason})
}

// scanTree walks root/sub and records what it finds under sub (sub may be "").
func scanTree(root, sub string, objs *objects.Store, prevEntries map[string]Entry, prevScan int64, c *Checkpoint) error {
	start := filepath.Join(root, sub)
	return filepath.WalkDir(start, func(abs string, d fs.DirEntry, err error) error {
		if abs == start {
			return nil
		}
		rel, _ := filepath.Rel(root, abs)
		if err != nil {
			c.except(rel, "unreadable: "+err.Error())
			return nil
		}
		if d.IsDir() && ExcludedDir(d.Name()) {
			return filepath.SkipDir
		}
		if IsSecret(abs) {
			c.except(rel, secretReason)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			c.except(rel, "unreadable: "+err.Error())
			return nil
		}
		return c.record(abs, rel, fi, objs, prevEntries, prevScan)
	})
}

func (c *Checkpoint) record(abs, rel string, fi fs.FileInfo, objs *objects.Store, prevEntries map[string]Entry, prevScan int64) error {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			c.except(rel, "unreadable symlink: "+err.Error())
			return nil
		}
		c.Entries[rel] = Entry{Kind: Symlink, Link: target}
	case fi.IsDir():
		c.Entries[rel] = Entry{Kind: Dir, Mode: unixMode(fi.Mode())}
	case fi.Mode().IsRegular():
		e := Entry{Kind: File, Mode: unixMode(fi.Mode()), Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano(), CtimeNS: ctimeNS(fi)}
		if pe, ok := prevEntries[rel]; ok && reusable(pe, e, prevScan, objs) {
			e.Ref = pe.Ref
			c.Entries[rel] = e
			return nil
		}
		content, err := os.ReadFile(abs)
		if err != nil {
			c.except(rel, "unreadable: "+err.Error())
			return nil
		}
		ref, err := objs.Put(content)
		if err != nil {
			return err
		}
		e.Ref = ref
		c.Entries[rel] = e
	default:
		c.except(rel, "unsupported kind: "+fi.Mode().Type().String())
	}
	return nil
}

// mtime can be set by any program (touch -r), so it alone cannot prove a
// file is unchanged. ctime is kernel-only. Both come from a coarse clock, so
// a ctime within a second of the previous scan is re-read rather than trusted.
func reusable(prev, cur Entry, prevScan int64, objs *objects.Store) bool {
	return prev.Kind == File &&
		prev.Size == cur.Size && prev.MtimeNS == cur.MtimeNS && prev.CtimeNS == cur.CtimeNS &&
		cur.CtimeNS != 0 && prevScan != 0 && cur.CtimeNS < prevScan-int64(time.Second) &&
		objs.Has(prev.Ref)
}

func dropSubtree(entries map[string]Entry, rel string) {
	delete(entries, rel)
	for k := range entries {
		if strings.HasPrefix(k, rel+"/") {
			delete(entries, k)
		}
	}
}

func unixMode(m fs.FileMode) uint32 {
	bits := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}
