package store

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SafeJoinUnder resolves rel beneath base and returns the destination path,
// refusing anything that could place the write outside base:
//
//   - rel is absolute, or escapes upward (".." / "../x" after cleaning);
//   - any EXISTING component of rel (intermediate directory or final name) is
//     a symlink. A symlinked intermediate directory is the interesting case: the
//     path still looks contained ("base/sub/file"), but writing to it lands
//     wherever sub points.
//
// It is validation, not creation: components that do not exist yet are fine
// (their parents cannot be traversed to anywhere else). Pair it with
// WriteFileNoFollow, which re-checks the destination at open time.
func SafeJoinUnder(base, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("refusing empty path under %s", base)
	}
	sep := string(filepath.Separator)
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+sep) {
		return "", fmt.Errorf("refusing %q: it escapes %s", rel, base)
	}
	cur := base
	for _, comp := range strings.Split(clean, sep) {
		if comp == "" || comp == "." {
			continue
		}
		cur = filepath.Join(cur, comp)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				continue // not there yet: nothing to follow
			}
			return "", fmt.Errorf("refusing %q: cannot inspect %s: %w", rel, cur, err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing %q: %s is a symlink, so writing there would leave %s", rel, cur, base)
		}
	}
	return cur, nil
}

// WriteFileNoFollow writes content to dst, never THROUGH whatever may already
// be there. If anything exists at dst, whether a regular file, a directory, or
// a symlink (dangling or not), it REFUSES with an error wrapping os.ErrExist
// (test it with errors.Is, not os.IsExist, which does not unwrap) rather than
// truncating or replacing it, so the caller can skip that entry and report it.
// Silently overwriting is wrong twice over: it destroys live work, and through
// a hostile symlink it writes outside the requested destination entirely.
//
// Missing parent directories are created, but only ones that do not exist: the
// deepest existing ancestor must be a real directory (never a symlink), and the
// components below it are made fresh. Callers should derive dst with
// SafeJoinUnder so every component under their base is checked too.
//
// The open uses O_CREATE|O_EXCL, which the kernel does not resolve through a
// final symlink. The EEXIST refusal happens in the syscall, not only in the
// Lstat above it, so there is no window to lose.
func WriteFileNoFollow(dst string, content []byte, mode os.FileMode) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("refusing to write %s: something already exists there: %w", dst, os.ErrExist)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := ensureParentNoFollow(dst); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("refusing to write %s: something already exists there: %w", dst, os.ErrExist)
		}
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode) // OpenFile's mode is umask'd
}

// ensureParentNoFollow makes dst's parent directory exist without traversing a
// symlink into it: it walks up to the deepest EXISTING ancestor, requires that
// ancestor to be a real directory, then creates the missing components downward.
func ensureParentNoFollow(dst string) error {
	dir := filepath.Dir(dst)
	var missing []string
	cur := dir
	for {
		fi, err := os.Lstat(cur)
		if err == nil {
			if fi.Mode()&fs.ModeSymlink != 0 {
				return fmt.Errorf("refusing to write under %s: it is a symlink", cur)
			}
			if !fi.IsDir() {
				return fmt.Errorf("refusing to write under %s: it is not a directory", cur)
			}
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		missing = append(missing, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			return fmt.Errorf("refusing to write %s: no existing ancestor directory", dst)
		}
		cur = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o755); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}
