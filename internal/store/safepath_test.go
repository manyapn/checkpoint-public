package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSafeJoinUnderRefusesEscapes pins the containment helper the recover path
// needs: a salvaged path must land under the requested --to directory, never
// outside it, including through a HOSTILE INTERMEDIATE SYMLINK, where the
// joined path still looks contained.
func TestSafeJoinUnderRefusesEscapes(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()

	if got, err := SafeJoinUnder(base, "a/b/c.txt"); err != nil || got != filepath.Join(base, "a/b/c.txt") {
		t.Fatalf("an ordinary relative path must be joined: %q err=%v", got, err)
	}
	for _, bad := range []string{"", "..", "../outside", "a/../../outside", "/etc/passwd", "."} {
		if got, err := SafeJoinUnder(base, bad); err == nil {
			t.Fatalf("%q must be refused, got %q", bad, got)
		}
	}

	// Hostile intermediate directory symlink: base/link -> outside.
	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("MUST NOT CHANGE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := SafeJoinUnder(base, "link/sentinel")
	if err == nil {
		t.Fatalf("a symlinked intermediate component must be refused, got %q", got)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("refusal must name the symlink: %v", err)
	}
	// A symlink as the FINAL component is an escape too.
	if err := os.Symlink(filepath.Join(outside, "sentinel"), filepath.Join(base, "direct")); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeJoinUnder(base, "direct"); err == nil {
		t.Fatal("a symlink at the destination itself must be refused")
	}
	// Deeper components that do not exist yet are fine.
	if _, err := SafeJoinUnder(base, "brand/new/path.txt"); err != nil {
		t.Fatalf("nonexistent components are not an escape: %v", err)
	}
}

// TestWriteFileNoFollowRefusesExistingDestination: recovering a file must never
// overwrite an ordinary live file, and must never follow a destination SYMLINK
// to clobber a file outside the recovery target. Both are refusals the caller
// can report, never silent writes.
func TestWriteFileNoFollowRefusesExistingDestination(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	const sentinel = "OUTSIDE MUST NOT CHANGE\n"
	victim := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(victim, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fresh write, including missing parent directories.
	dst := filepath.Join(base, "sub/dir/recovered.txt")
	if err := WriteFileNoFollow(dst, []byte("salvaged\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "salvaged\n" {
		t.Fatalf("content: %q", b)
	}
	if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode must be honoured exactly: %v err=%v", fi, err)
	}

	// An existing ordinary file is skipped, not overwritten.
	if err := os.WriteFile(dst, []byte("live edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileNoFollow(dst, []byte("salvaged\n"), 0o640); !errors.Is(err, os.ErrExist) {
		t.Fatalf("an existing destination must be refused with ErrExist, got %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "live edit\n" {
		t.Fatalf("live content must be untouched: %q", b)
	}

	// A destination symlink is refused, never followed.
	linked := filepath.Join(base, "transient-file")
	if err := os.Symlink(victim, linked); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileNoFollow(linked, []byte("salvaged\n"), 0o644); !errors.Is(err, os.ErrExist) {
		t.Fatalf("a destination symlink must be refused with ErrExist, got %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != sentinel {
		t.Fatalf("write escaped through the destination symlink: %q", b)
	}
	if fi, err := os.Lstat(linked); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the destination symlink must be left alone, not replaced: %v err=%v", fi, err)
	}
	// A DANGLING destination symlink is refused too (O_EXCL sees it).
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(outside, "nothing-here"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileNoFollow(dangling, []byte("salvaged\n"), 0o644); !errors.Is(err, os.ErrExist) {
		t.Fatalf("a dangling destination symlink must be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "nothing-here")); !os.IsNotExist(err) {
		t.Fatalf("writing through a dangling symlink created a file outside the target: %v", err)
	}

	// A symlinked PARENT directory is refused rather than traversed.
	if err := os.Symlink(outside, filepath.Join(base, "parentlink")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileNoFollow(filepath.Join(base, "parentlink/new.txt"), []byte("x\n"), 0o644); err == nil {
		t.Fatal("a symlinked parent directory must be refused")
	}
	if _, err := os.Lstat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("write escaped through a symlinked parent directory")
	}
}
