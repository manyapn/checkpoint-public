package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

func TestSnapshotCapturesKindsAndModes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plain.txt"), "hi\n", 0o644)
	writeFile(t, filepath.Join(root, "script.sh"), "#!/bin/sh\n", 0o755)
	mkdir(t, filepath.Join(root, "empty"), 0o755)
	mkdir(t, filepath.Join(root, "nested/deep"), 0o755)
	writeFile(t, filepath.Join(root, "nested/deep/f"), "x\n", 0o600)
	symlink(t, "plain.txt", filepath.Join(root, "link"))

	m := snap(t, root, nil)

	if e, ok := m.Entries["plain.txt"]; !ok || e.Kind != KindFile || e.Mode != 0o644 {
		t.Fatalf("plain.txt: %+v (ok=%v)", e, ok)
	}
	if e := m.Entries["script.sh"]; e.Mode != 0o755 {
		t.Fatalf("script.sh should keep exec bit, got mode %o", e.Mode)
	}
	if e, ok := m.Entries["empty"]; !ok || e.Kind != KindDir {
		t.Fatalf("empty dir must be captured (git can't): %+v ok=%v", e, ok)
	}
	if e, ok := m.Entries["link"]; !ok || e.Kind != KindSymlink || e.Link != "plain.txt" {
		t.Fatalf("symlink: %+v ok=%v", e, ok)
	}
	if _, ok := m.Entries["nested/deep/f"]; !ok {
		t.Fatal("nested file must be captured")
	}
}

func TestSnapshotSkipsBulkExcludes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "keep.txt"), "keep\n", 0o644)
	mkdir(t, filepath.Join(root, "node_modules/pkg"), 0o755)
	writeFile(t, filepath.Join(root, "node_modules/pkg/index.js"), "junk\n", 0o644)
	mkdir(t, filepath.Join(root, ".git"), 0o755)
	writeFile(t, filepath.Join(root, ".git/HEAD"), "ref\n", 0o644)

	m := snap(t, root, nil)

	if _, ok := m.Entries["keep.txt"]; !ok {
		t.Fatal("keep.txt should be captured")
	}
	for rel := range m.Entries {
		if rel == "node_modules" || rel == ".git" ||
			strings.HasPrefix(rel, "node_modules/") || strings.HasPrefix(rel, ".git/") {
			t.Fatalf("excluded path leaked into manifest: %s", rel)
		}
	}
}

func TestWriteLoadLatestDurable(t *testing.T) {
	root := t.TempDir()
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "a\n", 0o644)

	oc := mustObj(t, storeDir)
	m0, err := Snapshot(root, oc, nil, 0, 100, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(storeDir, m0); err != nil {
		t.Fatal(err)
	}
	m1, err := Snapshot(root, oc, m0, 1, 200, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(storeDir, m1); err != nil {
		t.Fatal(err)
	}

	got, err := Load(storeDir, 0)
	if err != nil || got.ID != 0 {
		t.Fatalf("Load(0): %+v err=%v", got, err)
	}
	latest, ok, err := Latest(storeDir)
	if err != nil || !ok || latest.ID != 1 {
		t.Fatalf("Latest should be id=1: %+v ok=%v err=%v", latest, ok, err)
	}
}

func TestLatestSkipsPartialAndTornManifests(t *testing.T) {
	root := t.TempDir()
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "a\n", 0o644)
	oc := mustObj(t, storeDir)

	durable, _ := Snapshot(root, oc, nil, 0, 100, DURABLE, 0)
	Write(storeDir, durable)
	partial, _ := Snapshot(root, oc, nil, 1, 200, PARTIAL, 3)
	Write(storeDir, partial)
	// a torn/garbage manifest with the highest id must be skipped, not crash.
	torn := filepath.Join(storeDir, "manifests", "2.json")
	if err := os.WriteFile(torn, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	latest, ok, err := Latest(storeDir)
	if err != nil || !ok || latest.ID != 0 {
		t.Fatalf("Latest must fall back to the durable id=0, got %+v ok=%v err=%v", latest, ok, err)
	}
}

func TestLatestEmptyStore(t *testing.T) {
	_, ok, err := Latest(t.TempDir())
	if err != nil {
		t.Fatalf("empty store should not error: %v", err)
	}
	if ok {
		t.Fatal("empty store should report ok=false")
	}
}

func TestIncrementalReusesUnchangedRefs(t *testing.T) {
	root := t.TempDir()
	storeDir := t.TempDir()
	p := filepath.Join(root, "stable.txt")
	writeFile(t, p, "stable\n", 0o644)
	oc := mustObj(t, storeDir)

	m0, _ := Snapshot(root, oc, nil, 0, 100, DURABLE, 0)
	ref0 := m0.Entries["stable.txt"].Ref

	// unchanged file (same size+mtime): incremental snapshot must reuse the ref.
	m1, _ := Snapshot(root, oc, m0, 1, 200, DURABLE, 0)
	if m1.Entries["stable.txt"].Ref != ref0 {
		t.Fatal("unchanged file should reuse prior ref")
	}
}

// --- helpers ---

func mustObj(t *testing.T, storeDir string) *objstore.Store {
	t.Helper()
	oc, err := objstore.Open(filepath.Join(storeDir, "objects-root"))
	if err != nil {
		t.Fatal(err)
	}
	return oc
}

// TestSnapshotNamesUnsupportedKind: a kind of file the manifest cannot represent
// (a fifo here) becomes a NAMED exception, never a silent omission. The rest of
// the tree is still captured.
func TestSnapshotNamesUnsupportedKind(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "ok.txt"), "fine\n", 0o644)
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	m := snap(t, root, nil)

	if _, ok := m.Entries["ok.txt"]; !ok {
		t.Fatal("regular file must still be captured")
	}
	if _, ok := m.Entries["pipe"]; ok {
		t.Fatal("a fifo must not appear as a restorable entry")
	}
	if len(m.Exceptions) != 1 || m.Exceptions[0].Path != "pipe" ||
		!strings.Contains(m.Exceptions[0].Reason, "unsupported") {
		t.Fatalf("fifo must be a named unsupported-kind exception, got %+v", m.Exceptions)
	}
}

// TestSnapshotNamesUnreadableFile: a file the scan cannot read lands as a NAMED
// exception on the manifest (the seam stands in for permission denial, since
// the tests run as root and root ignores permission bits).
func TestSnapshotNamesUnreadableFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "ok.txt"), "fine\n", 0o644)
	writeFile(t, filepath.Join(root, "secret.txt"), "cannot read\n", 0o644)

	orig := readFile
	readFile = func(p string) ([]byte, error) {
		if filepath.Base(p) == "secret.txt" {
			return nil, os.ErrPermission
		}
		return orig(p)
	}
	defer func() { readFile = orig }()

	m := snap(t, root, nil)

	if _, ok := m.Entries["ok.txt"]; !ok {
		t.Fatal("readable file must still be captured")
	}
	if _, ok := m.Entries["secret.txt"]; ok {
		t.Fatal("an unreadable file must not appear as a restorable entry")
	}
	if len(m.Exceptions) != 1 || m.Exceptions[0].Path != "secret.txt" ||
		!strings.Contains(m.Exceptions[0].Reason, "unreadable") {
		t.Fatalf("unreadable file must be a named exception, got %+v", m.Exceptions)
	}
}

// TestExceptionsRoundTripManifest: named exceptions survive Write/Load.
func TestExceptionsRoundTripManifest(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{ID: 3, Coverage: DURABLE, Entries: map[string]Entry{},
		Exceptions: []Exception{{Path: "a/b", Reason: "unreadable: x"}}}
	if err := Write(dir, m); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Exceptions) != 1 || got.Exceptions[0] != m.Exceptions[0] {
		t.Fatalf("exceptions must round-trip, got %+v", got.Exceptions)
	}
}

// TestSnapshotCoversExtraRoots: extra protected folders land under
// Manifest.Extra keyed by absolute root, restore in place via RestoreExtras,
// and reuse refs incrementally like the primary tree.
func TestSnapshotCoversExtraRoots(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n", 0o644)
	writeFile(t, filepath.Join(extra, "settings.json"), "{\"a\":1}\n", 0o600)
	mkdir(t, filepath.Join(extra, "sub"), 0o755)

	oc := mustObj(t, t.TempDir())
	m, err := Snapshot(root, oc, nil, 0, 1, DURABLE, 0, extra)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Entries["main.go"]; !ok {
		t.Fatal("primary tree must be captured")
	}
	ex := m.Extra[extra]
	if ex == nil {
		t.Fatalf("extra root must be recorded under its absolute path, got %+v", m.Extra)
	}
	if e, ok := ex["settings.json"]; !ok || e.Kind != KindFile || e.Mode != 0o600 {
		t.Fatalf("extra-root file: %+v ok=%v", e, ok)
	}
	if _, ok := ex["sub"]; !ok {
		t.Fatal("extra-root dir must be captured")
	}

	// Incremental: an unchanged extra-root file reuses its ref.
	m2, err := Snapshot(root, oc, m, 1, 2, DURABLE, 0, extra)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Extra[extra]["settings.json"].Ref != ex["settings.json"].Ref {
		t.Fatal("unchanged extra-root file must reuse its ref")
	}

	// RestoreExtras puts the extra tree back in place after deletion.
	if err := os.RemoveAll(extra); err != nil {
		t.Fatal(err)
	}
	if err := RestoreExtras(m, oc); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(extra, "settings.json"))
	if err != nil || string(b) != "{\"a\":1}\n" {
		t.Fatalf("extra-root file must restore in place byte-exact: %q err=%v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(extra, "sub")); err != nil || !fi.IsDir() {
		t.Fatal("extra-root dir must restore in place")
	}
}

// TestSalvageSkipsExtraRootManifestPaths: a version whose path an extra-root
// tree already lists is not offered as a salvage (it is not a transient).
func TestSalvageSkipsExtraRootManifestPaths(t *testing.T) {
	oc := mustObj(t, t.TempDir())
	ref, _, err := oc.Put([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{Root: "/ws", Extra: map[string]map[string]Entry{
		"/home/u/.config/app": {"settings.json": {Kind: KindFile, Ref: ref}},
	}}
	got, _ := Salvage([]versionlog.Version{
		{Op: versionlog.OpModify, Path: "/home/u/.config/app/settings.json", Ref: ref},
	}, []*Manifest{m}, oc)
	if len(got) != 0 {
		t.Fatalf("extra-root manifest path must not be salvageable, got %v", got)
	}
}

func snap(t *testing.T, root string, prev *Manifest) *Manifest {
	t.Helper()
	oc := mustObj(t, t.TempDir())
	m, err := Snapshot(root, oc, prev, 0, 1, DURABLE, 0)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return m
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // defeat umask
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// TestScanProgressReportsRunningCount: a non-nil ScanProgress hook receives a
// growing entries-scanned count during Snapshot, ending with the exact total,
// so the "Setting up" files-scanned surface is honest, not decorative.
func TestScanProgressReportsRunningCount(t *testing.T) {
	root := t.TempDir()
	const files = 300 // > one progress stride
	for i := 0; i < files; i++ {
		writeFile(t, filepath.Join(root, fmt.Sprintf("f%03d.txt", i)), "x\n", 0o644)
	}
	var got []int
	ScanProgress = func(n int) { got = append(got, n) }
	defer func() { ScanProgress = nil }()

	m := snap(t, root, nil)

	if len(got) < 2 {
		t.Fatalf("progress must report during the scan, got %v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("progress must be non-decreasing, got %v", got)
		}
	}
	if got[len(got)-1] != len(m.Entries) {
		t.Fatalf("final progress %d must equal total entries scanned %d", got[len(got)-1], len(m.Entries))
	}
}
