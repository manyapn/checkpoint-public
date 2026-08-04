package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestCreateRmRfRestoreByteExact is the headline guarantee end to end: snapshot
// a fixture tree into an OUT-OF-TREE store, `rm -rf` the project, then restore
// by manifest id and verify the tree is byte-exact: content, unix mode (incl.
// the exec bit), symlink targets, and empty dirs (which git alone cannot
// represent).
func TestCreateRmRfRestoreByteExact(t *testing.T) {
	// Store lives entirely outside the project, so deleting the project cannot
	// touch it: the history has to survive `rm -rf` of the project it describes.
	storeDir := t.TempDir()
	oc := mustObj(t, storeDir)

	project := filepath.Join(t.TempDir(), "proj")

	// A tree that exercises every capturable kind + the git-can't cases.
	writeFile(t, filepath.Join(project, "README.md"), "# hi\n", 0o644)
	writeFile(t, filepath.Join(project, "bin/run.sh"), "#!/bin/sh\necho go\n", 0o755) // exec bit
	writeFile(t, filepath.Join(project, "src/main.go"), "package main\n", 0o644)
	writeFile(t, filepath.Join(project, "src/util.go"), "package main\n", 0o600) // odd perms
	writeFile(t, filepath.Join(project, "data/blob.bin"), string(bytes.Repeat([]byte{0, 1, 2, 3}, 4096)), 0o644)
	writeFile(t, filepath.Join(project, "dup_a.txt"), "identical\n", 0o644) // dedups with dup_b
	writeFile(t, filepath.Join(project, "dup_b.txt"), "identical\n", 0o644)
	mkdir(t, filepath.Join(project, "logs"), 0o755)              // empty dir (git can't)
	mkdir(t, filepath.Join(project, "deep/a/b/c"), 0o750)        // nested empty dir, odd perms
	symlink(t, "src/main.go", filepath.Join(project, "current")) // symlink

	before := fingerprint(t, project)

	m, err := Snapshot(project, oc, nil, 0, 1, DURABLE, 0)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := Write(storeDir, m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// The destructive step.
	if err := os.RemoveAll(project); err != nil {
		t.Fatalf("rm -rf: %v", err)
	}
	if _, err := os.Stat(project); !os.IsNotExist(err) {
		t.Fatalf("project should be gone after rm -rf, stat err=%v", err)
	}

	// Restore strictly by id, never by folding an event stream.
	loaded, err := Load(storeDir, 0)
	if err != nil {
		t.Fatalf("Load(0): %v", err)
	}
	if loaded.Coverage != DURABLE {
		t.Fatalf("manifest not DURABLE: %s", loaded.Coverage)
	}
	if err := Restore(loaded, oc, project); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	after := fingerprint(t, project)
	if diff := diffTrees(before, after); diff != "" {
		t.Fatalf("restored tree is not byte-exact:\n%s", diff)
	}
}

// node is one path's observable state for byte-exact comparison.
type node struct {
	kind string // "file" | "dir" | "symlink"
	mode uint32 // perm+special bits (files/dirs)
	sha  string // sha256 of content (files)
	link string // symlink target
}

// fingerprint walks root and records every path's kind, mode, content hash, and
// symlink target: the ground truth the restore must reproduce exactly.
func fingerprint(t *testing.T, root string) map[string]node {
	t.Helper()
	out := map[string]node{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			tgt, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = node{kind: "symlink", link: tgt}
		case fi.IsDir():
			out[rel] = node{kind: "dir", mode: unixMode(fi.Mode())}
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			out[rel] = node{kind: "file", mode: unixMode(fi.Mode()), sha: hex.EncodeToString(sum[:])}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fingerprint %s: %v", root, err)
	}
	return out
}

func diffTrees(before, after map[string]node) string {
	var b []string
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		bn, inB := before[k]
		an, inA := after[k]
		switch {
		case !inA:
			b = append(b, "missing after restore: "+k)
		case !inB:
			b = append(b, "unexpectedly present after restore: "+k)
		case bn != an:
			b = append(b, "mismatch at "+k+":\n  before "+format(bn)+"\n  after  "+format(an))
		}
	}
	return strings.Join(b, "\n")
}

func format(n node) string {
	switch n.kind {
	case "symlink":
		return "symlink -> " + n.link
	case "dir":
		return "dir mode=0o" + strconv.FormatUint(uint64(n.mode), 8)
	default:
		return "file mode=0o" + strconv.FormatUint(uint64(n.mode), 8) + " sha=" + n.sha
	}
}

// TestRestoreRefusesToFollowDestinationSymlink: when a manifest DIRECTORY path
// is occupied in the destination by a symlink pointing outside the target, a
// naive restore writes THROUGH the link, silently modifying a file outside the
// restore target and never materializing the promised directory. The manifest
// is authoritative for the tree it describes: a symlink squatting on a
// directory path is replaced, never followed.
func TestRestoreRefusesToFollowDestinationSymlink(t *testing.T) {
	src := t.TempDir()
	storeDir := t.TempDir()
	mkdir(t, filepath.Join(src, "nested"), 0o755)
	writeFile(t, filepath.Join(src, "nested/payload"), "checkpoint content\n", 0o644)

	oc := mustObj(t, storeDir)
	m, err := Snapshot(src, oc, nil, 0, 1, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	const sentinel = "OUTSIDE MUST NOT CHANGE\n"
	writeFile(t, filepath.Join(outside, "payload"), sentinel, 0o644)

	target := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(target, "nested")); err != nil {
		t.Fatal(err)
	}

	if err := Restore(m, oc, target); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(outside, "payload"))
	if err != nil || string(got) != sentinel {
		t.Fatalf("restore escaped the target and modified %s: got %q", filepath.Join(outside, "payload"), got)
	}
	fi, err := os.Lstat(filepath.Join(target, "nested"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("target/nested must be a real directory after restore, got %v (err %v)", fi, err)
	}
	b, err := os.ReadFile(filepath.Join(target, "nested/payload"))
	if err != nil || string(b) != "checkpoint content\n" {
		t.Fatalf("payload must land inside the target: %q err=%v", b, err)
	}
}

// TestRestoreDoesNotWriteThroughFileSymlink: the same escape via a symlink
// squatting on a FILE path.
func TestRestoreDoesNotWriteThroughFileSymlink(t *testing.T) {
	src := t.TempDir()
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(src, "f.txt"), "checkpoint content\n", 0o644)
	oc := mustObj(t, storeDir)
	m, err := Snapshot(src, oc, nil, 0, 1, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	const sentinel = "OUTSIDE MUST NOT CHANGE\n"
	outFile := filepath.Join(outside, "victim")
	writeFile(t, outFile, sentinel, 0o644)

	target := t.TempDir()
	if err := os.Symlink(outFile, filepath.Join(target, "f.txt")); err != nil {
		t.Fatal(err)
	}
	if err := Restore(m, oc, target); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(outFile); string(got) != sentinel {
		t.Fatalf("restore wrote through a file symlink: %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(target, "f.txt")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("target/f.txt must be a real file after restore: %v err=%v", fi, err)
	}
}

// TestRestoreExactRemovesExtrasButNeverExcluded pins both halves of the
// byte-exact guarantee: a full restore that leaves untracked files behind is
// not byte-exact, AND the hazard the naive fix creates: default-skipped
// directories are absent from every manifest BY DESIGN, so pruning must never
// touch them.
func TestRestoreExactRemovesExtrasButNeverExcluded(t *testing.T) {
	src := t.TempDir()
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(src, "kept.txt"), "kept\n", 0o644)
	oc := mustObj(t, storeDir)
	m, err := Snapshot(src, oc, nil, 0, 1, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	writeFile(t, filepath.Join(target, "kept.txt"), "stale\n", 0o644)
	writeFile(t, filepath.Join(target, "extra.txt"), "untracked\n", 0o644)
	mkdir(t, filepath.Join(target, "extradir/sub"), 0o755)
	writeFile(t, filepath.Join(target, "extradir/sub/deep.txt"), "untracked\n", 0o644)
	// Excluded trees: absent from the manifest, must survive.
	mkdir(t, filepath.Join(target, "node_modules/pkg"), 0o755)
	writeFile(t, filepath.Join(target, "node_modules/pkg/index.js"), "dep\n", 0o644)
	mkdir(t, filepath.Join(target, ".git"), 0o755)
	writeFile(t, filepath.Join(target, ".git/HEAD"), "ref\n", 0o644)

	removed, keptExceptions, err := RestoreExact(m, oc, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(keptExceptions) != 0 {
		t.Fatalf("this manifest names no exceptions, got %v", keptExceptions)
	}

	if b, _ := os.ReadFile(filepath.Join(target, "kept.txt")); string(b) != "kept\n" {
		t.Fatalf("checkpoint content must win: %q", b)
	}
	for _, gone := range []string{"extra.txt", "extradir"} {
		if _, err := os.Lstat(filepath.Join(target, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s is not in the checkpoint and must be removed by an exact restore", gone)
		}
	}
	for _, survivor := range []string{"node_modules/pkg/index.js", ".git/HEAD"} {
		if _, err := os.Stat(filepath.Join(target, survivor)); err != nil {
			t.Fatalf("default-skipped %s must NEVER be removed (absent from manifests by design): %v", survivor, err)
		}
	}
	var sawExcluded bool
	for _, rel := range removed {
		if strings.HasPrefix(rel, "node_modules") || strings.HasPrefix(rel, ".git") {
			sawExcluded = true
		}
	}
	if sawExcluded {
		t.Fatalf("excluded trees must not even be reported as removed: %v", removed)
	}
}

// TestCheckStoreLocationRefusesInTree: a store inside the folder it protects
// would be destroyed along with the project, so it is refused outright.
func TestCheckStoreLocationRefusesInTree(t *testing.T) {
	if err := CheckStoreLocation("/srv/project/.store", "/srv/project"); err == nil ||
		!strings.Contains(err.Error(), "inside the protected folder") {
		t.Fatalf("a store inside the project must be refused, got %v", err)
	}
	if err := CheckStoreLocation("/srv/project", "/srv/project"); err == nil {
		t.Fatal("a store that IS the project must be refused")
	}
	if err := CheckStoreLocation("/srv", "/srv/project"); err == nil {
		t.Fatal("a project inside its own store must be refused")
	}
	if err := CheckStoreLocation("/var/lib/checkpoint/p", "/srv/project"); err != nil {
		t.Fatalf("a genuinely out-of-tree store must be accepted: %v", err)
	}
	// Sibling prefix strings must not be mistaken for containment.
	if err := CheckStoreLocation("/srv/project-store", "/srv/project"); err != nil {
		t.Fatalf("sibling path sharing a prefix is out-of-tree: %v", err)
	}
}
