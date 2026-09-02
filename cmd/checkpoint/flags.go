package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// flagWasSet reports whether the user actually passed a flag, as opposed to it
// carrying its zero default. Go's flag package cannot distinguish `--only ”`
// from an absent --only by value alone, and that distinction is load-bearing:
// an explicitly empty selector must refuse, never widen into a full destructive
// operation.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// parseOnly turns a --only value into the selected relative paths. It is the
// single gate for both `undo --only` and `restore --only`, because the failure
// modes are shared and destructive:
//
//   - An EMPTY selector (`--only ”`, trivially produced by an unset shell or
//     CI variable) must NEVER silently widen into "operate on everything": a
//     flag that says "limit this" turning into a full destructive operation is
//     the worst possible reading of the user's intent.
//   - A path outside the workspace must be refused, not silently ignored:
//     `--only ../outside` reporting "nothing to revert" reads as "that file was
//     clean", which is a lie about a path we never considered.
//
// present tells the caller whether --only was given at all (absent = whole
// operation, which is a different thing from an empty selector).
func parseOnly(flagVal string, given bool, root string) (only []string, present bool, err error) {
	if !given {
		return nil, false, nil
	}
	for _, raw := range strings.Split(flagVal, ",") {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if filepath.IsAbs(p) {
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, true, fmt.Errorf("--only %s is outside the workspace %s", p, root)
			}
			p = rel
		}
		clean := filepath.Clean(p)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, true, fmt.Errorf("--only %s escapes the workspace %s (paths are workspace-relative)", raw, root)
		}
		only = append(only, clean)
	}
	if len(only) == 0 {
		return nil, true, fmt.Errorf("--only was given but names no paths; refusing to widen an " +
			"explicitly limited operation into a full one (omit --only to operate on everything)")
	}
	return only, true, nil
}

// parseCheckpointID accepts a COMPLETE unsigned decimal id and nothing else.
// fmt.Sscanf("%d") stops at the first non-digit, so "0junk", "1.0", "1/2" and
// "0x1" would silently parse as a numeric prefix and restore the WRONG
// checkpoint destructively. A typo must fail, not guess.
func parseCheckpointID(s string) (int, error) {
	n, err := strconv.ParseUint(s, 10, 31)
	if err != nil {
		return 0, fmt.Errorf("bad checkpoint id %q: expected a whole number like 0, 1, 2", s)
	}
	return int(n), nil
}
