package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestRestoreExactPreservesNamedExceptions: an exact restore prunes paths
// missing from Entries, but must never prune the ones the manifest deliberately
// NAMES as exceptions (a fifo, an unreadable file). Those are unrecoverable by
// design (the automatic pre-restore checkpoint records them as exceptions too),
// so deleting one is irreversible, while the CLI is telling the user the
// pre-restore checkpoint can bring everything back.
func TestRestoreExactPreservesNamedExceptions(t *testing.T) {
	src := t.TempDir()
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(src, "data"), "tracked\n", 0o644)
	if err := syscall.Mkfifo(filepath.Join(src, "important.pipe"), 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	// An unreadable file: the other exception flavour, via the readFile seam
	// (tests may run as root, which ignores permission bits).
	writeFile(t, filepath.Join(src, "secret.txt"), "cannot read\n", 0o600)
	mkdir(t, filepath.Join(src, "sub"), 0o755)
	writeFile(t, filepath.Join(src, "sub/also-secret.txt"), "cannot read\n", 0o600)
	orig := readFile
	readFile = func(p string) ([]byte, error) {
		if strings.Contains(filepath.Base(p), "secret") {
			return nil, os.ErrPermission
		}
		return orig(p)
	}
	defer func() { readFile = orig }()

	oc := mustObj(t, storeDir)
	m, err := Snapshot(src, oc, nil, 0, 1, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Exceptions) != 3 {
		t.Fatalf("expected fifo + two unreadable files as named exceptions, got %+v", m.Exceptions)
	}

	// An ordinary untracked file must still be pruned: preserving exceptions
	// must not turn RestoreExact into a no-op.
	writeFile(t, filepath.Join(src, "untracked.txt"), "junk\n", 0o644)

	removed, kept, err := RestoreExact(m, oc, src)
	if err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(filepath.Join(src, "important.pipe"))
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("a named unsupported-kind exception must survive an exact restore (irreversible otherwise): %v err=%v", fi, err)
	}
	if b, err := os.ReadFile(filepath.Join(src, "secret.txt")); err != nil || string(b) != "cannot read\n" {
		t.Fatalf("a named unreadable-file exception must survive untouched: %q err=%v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(src, "sub/also-secret.txt")); err != nil || string(b) != "cannot read\n" {
		t.Fatalf("an exception nested under a directory must survive: %q err=%v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(src, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("an ordinary untracked file must still be pruned, err=%v", err)
	}

	want := []string{"important.pipe", "secret.txt", filepath.Join("sub", "also-secret.txt")}
	if strings.Join(kept, "|") != strings.Join(want, "|") {
		t.Fatalf("preserved exceptions must be reported so the CLI can say it could not restore them: got %v want %v", kept, want)
	}
	for _, rel := range removed {
		for _, ex := range want {
			if rel == ex {
				t.Fatalf("exception %s must never be reported as removed (removed=%v)", ex, removed)
			}
		}
	}
}

// TestSnapshotRefusesSymlinkRoot: filepath.WalkDir on a symlink visits only the
// link itself, so a symlinked project root would produce an EMPTY checkpoint
// that history then badges "Fully recoverable". Capturing nothing while
// claiming full recovery is the worst possible outcome, so this layer refuses
// and names the resolved path instead.
func TestSnapshotRefusesSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "project-real")
	mkdir(t, real, 0o755)
	writeFile(t, filepath.Join(real, "file"), "content\n", 0o644)
	link := filepath.Join(base, "project-link")
	symlink(t, real, link)

	oc := mustObj(t, t.TempDir())

	m, err := Snapshot(link, oc, nil, 0, 1, DURABLE, 0)
	if err == nil {
		t.Fatalf("a symlink root must be refused, got a manifest with %d entries", len(m.Entries))
	}
	if !errors.Is(err, ErrSymlinkRoot) || !strings.Contains(err.Error(), real) {
		t.Fatalf("refusal must be named and point at the resolved path: %v", err)
	}
	// The resolved path still works.
	if m, err := Snapshot(real, oc, nil, 0, 1, DURABLE, 0); err != nil || len(m.Entries) != 1 {
		t.Fatalf("the resolved root must snapshot normally: %v err=%v", m, err)
	}
	// Extra protected folders and the fold path get the same guard.
	if _, err := Snapshot(real, oc, nil, 0, 1, DURABLE, 0, link); err == nil || !errors.Is(err, ErrSymlinkRoot) {
		t.Fatalf("a symlinked extra protected folder must be refused: %v", err)
	}
	if _, err := SnapshotFold(link, oc, nil, 0, 1, DURABLE, 0, nil); err == nil || !errors.Is(err, ErrSymlinkRoot) {
		t.Fatalf("SnapshotFold must refuse a symlink root too: %v", err)
	}
	// And a symlinked RESTORE target, where every write (and every prune
	// removal) would land outside the path the user named.
	good, err := Snapshot(real, oc, nil, 0, 1, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := Restore(good, oc, link); err == nil || !errors.Is(err, ErrSymlinkRoot) {
		t.Fatalf("a symlinked restore target must be refused: %v", err)
	}
}

// TestCheckStoreLocationResolvesSymlinks: a purely lexical in-project store
// guard is trivially bypassed: `--store /tmp/apparent-store` pointing at
// <project>/inside-store passes it, and `rm -rf project` then takes the
// recovery store with it. Only the resolved location decides.
func TestCheckStoreLocationResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	inside := filepath.Join(project, "inside-store")
	mkdir(t, inside, 0o755)
	apparent := filepath.Join(base, "apparent-store")
	symlink(t, inside, apparent)

	err := CheckStoreLocation(apparent, project)
	if err == nil {
		t.Fatal("a store whose REAL location is inside the project must be refused")
	}
	if !strings.Contains(err.Error(), "inside the protected folder") || !strings.Contains(err.Error(), "resolves to") {
		t.Fatalf("refusal must name the containment and the symlink resolution: %v", err)
	}

	// A store dir that does not exist yet (created on demand) is resolved
	// through its nearest existing ancestor.
	linkedParent := filepath.Join(base, "linked-parent")
	symlink(t, project, linkedParent)
	if err := CheckStoreLocation(filepath.Join(linkedParent, "store-to-be"), project); err == nil {
		t.Fatal("a not-yet-created store under a symlinked in-project parent must be refused")
	}

	// A genuinely out-of-tree store, reached through a symlink, still passes.
	outside := filepath.Join(base, "outside-store")
	mkdir(t, outside, 0o755)
	linkedOutside := filepath.Join(base, "linked-outside")
	symlink(t, outside, linkedOutside)
	if err := CheckStoreLocation(linkedOutside, project); err != nil {
		t.Fatalf("an out-of-tree store reached via symlink must be accepted: %v", err)
	}
	if err := CheckStoreLocation(filepath.Join(base, "nowhere", "store"), project); err != nil {
		t.Fatalf("a fully nonexistent out-of-tree store must be accepted: %v", err)
	}
}

// TestCheckWorkspaceIdentityRefusesForeignStore: stamping store identity is
// useless unless it is verified; otherwise a daemon or a create can be pointed
// at another project's store and interleave two projects' checkpoints in one
// history.
func TestCheckWorkspaceIdentityRefusesForeignStore(t *testing.T) {
	m := Meta{Format: metaFormat, MachineID: "m1", Workspace: "/srv/project-a"}
	if err := CheckWorkspaceIdentity(m, "/srv/project-a"); err != nil {
		t.Fatalf("the store's own workspace must be accepted: %v", err)
	}
	err := CheckWorkspaceIdentity(m, "/srv/project-b")
	if err == nil {
		t.Fatal("a store stamped for another workspace must be refused")
	}
	if !strings.Contains(err.Error(), "/srv/project-a") || !strings.Contains(err.Error(), "/srv/project-b") {
		t.Fatalf("refusal must name BOTH workspaces: %v", err)
	}
	// No auto-migration: the stamp is not rewritten to match.
	if m.Workspace != "/srv/project-a" {
		t.Fatalf("identity must never be silently re-labelled, got %q", m.Workspace)
	}
}

// TestValidIDsExcludesTornManifests: a torn manifest file must never be counted
// as a checkpoint a user can act on, or `status` claims checkpoints that cannot
// be restored. IDs must still report the torn id (NextID must never reuse it);
// ValidIDs is the honest count.
func TestValidIDsExcludesTornManifests(t *testing.T) {
	storeDir := t.TempDir()
	for _, id := range []int{0, 1} {
		if err := Write(storeDir, &Manifest{ID: id, Coverage: DURABLE, Entries: map[string]Entry{}}); err != nil {
			t.Fatal(err)
		}
	}
	torn := filepath.Join(storeDir, "manifests", "2.json")
	if err := os.WriteFile(torn, []byte(`{"id":2,"entries":{"a":`), 0o600); err != nil {
		t.Fatal(err)
	}

	all, err := IDs(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("IDs must still see the torn id so numbering never reuses it: %v", all)
	}
	valid, err := ValidIDs(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 2 || valid[0] != 0 || valid[1] != 1 {
		t.Fatalf("a torn manifest is not a checkpoint anyone can restore: %v", valid)
	}
	if next, err := NextID(storeDir); err != nil || next != 3 {
		t.Fatalf("NextID must still advance past the torn id: %d err=%v", next, err)
	}
	// An empty store is not an error.
	if v, err := ValidIDs(t.TempDir()); err != nil || len(v) != 0 {
		t.Fatalf("empty store: %v err=%v", v, err)
	}
}
