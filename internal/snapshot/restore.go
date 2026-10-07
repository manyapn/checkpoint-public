package snapshot

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/manyapn/checkpoint-public/internal/objects"
)

// Restore writes every entry of c into target. With exact, anything under
// target the checkpoint does not describe is removed too, except excluded
// dirs (never captured, so absence means nothing) and named exceptions
// (could not be captured, so nothing could bring them back).
func Restore(c *Checkpoint, objs *objects.Store, target string, exact bool) (removed []string, err error) {
	if err := refuseSymlinkRoot(target); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, err
	}
	rels := make([]string, 0, len(c.Entries))
	for rel := range c.Entries {
		rels = append(rels, rel)
	}
	sort.Strings(rels) // parents before children
	for _, rel := range rels {
		if err := WriteEntry(objs, c.Entries[rel], target, rel); err != nil {
			return nil, fmt.Errorf("restore %s: %w", rel, err)
		}
	}
	if exact {
		removed, err = removeExtras(c, target)
		if err != nil {
			return nil, err
		}
	}
	// a dir's mode is applied last so a read-only dir does not block its children
	for _, rel := range rels {
		if e := c.Entries[rel]; e.Kind == Dir {
			os.Chmod(filepath.Join(target, rel), fs.FileMode(e.Mode))
		}
	}
	return removed, nil
}

// WriteEntry puts one entry at target/rel, replacing whatever is there. It
// never writes through an existing symlink.
func WriteEntry(objs *objects.Store, e Entry, target, rel string) error {
	dst, err := safeJoin(target, rel)
	if err != nil {
		return err
	}
	if err := ensureRealDirs(target, filepath.Dir(rel)); err != nil {
		return err
	}
	switch e.Kind {
	case Dir:
		return ensureRealDirs(target, rel)
	case File:
		content, err := objs.Get(e.Ref)
		if err != nil {
			return err
		}
		removeAt(dst)
		if err := os.WriteFile(dst, content, fs.FileMode(e.Mode)); err != nil {
			return err
		}
		return os.Chmod(dst, fs.FileMode(e.Mode))
	case Symlink:
		removeAt(dst)
		return os.Symlink(e.Link, dst)
	}
	return fmt.Errorf("unknown entry kind %q", e.Kind)
}

func removeAt(dst string) {
	if fi, err := os.Lstat(dst); err == nil {
		if fi.IsDir() {
			os.RemoveAll(dst)
		} else {
			os.Remove(dst)
		}
	}
}

func safeJoin(target, rel string) (string, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("entry %q escapes the target", rel)
	}
	return filepath.Join(target, clean), nil
}

// ensureRealDirs makes each component of target/rel a real directory; a
// symlink in the way is replaced, never followed.
func ensureRealDirs(target, rel string) error {
	cur := target
	for _, comp := range strings.Split(filepath.Clean(rel), "/") {
		if comp == "" || comp == "." {
			continue
		}
		cur = filepath.Join(cur, comp)
		fi, err := os.Lstat(cur)
		if err == nil && fi.IsDir() {
			continue
		}
		if err == nil {
			if err := os.Remove(cur); err != nil {
				return err
			}
		}
		if err := os.Mkdir(cur, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func removeExtras(c *Checkpoint, target string) ([]string, error) {
	exceptions := map[string]bool{}
	for _, ex := range c.Exceptions {
		exceptions[ex.Path] = true
	}
	var victims []string
	err := filepath.WalkDir(target, func(abs string, d fs.DirEntry, err error) error {
		if err != nil || abs == target {
			return nil
		}
		rel, _ := filepath.Rel(target, abs)
		if d.IsDir() && ExcludedDir(d.Name()) {
			return filepath.SkipDir
		}
		if exceptions[rel] || holdsException(exceptions, rel) {
			return nil
		}
		if _, wanted := c.Entries[rel]; wanted {
			return nil
		}
		victims = append(victims, rel)
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, rel := range victims {
		if err := os.RemoveAll(filepath.Join(target, rel)); err != nil {
			return nil, err
		}
	}
	return victims, nil
}

func holdsException(exceptions map[string]bool, rel string) bool {
	for ex := range exceptions {
		if strings.HasPrefix(ex, rel+"/") || strings.HasPrefix(rel, ex+"/") {
			return true
		}
	}
	return false
}
