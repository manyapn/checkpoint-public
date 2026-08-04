package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// rewritePreservingStat rewrites path with new content of the SAME length and
// puts the original mtime back, the exact shape a build tool / formatter /
// timestamp-preserving editor produces. Returns the content that is now on disk.
func rewritePreservingStat(t *testing.T, path, content string) string {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != len(content) {
		t.Fatalf("test bug: replacement must be the same length (%d vs %d)", len(old), len(content))
	}
	if err := os.WriteFile(path, []byte(content), fi.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != fi.Size() || after.ModTime().UnixNano() != fi.ModTime().UnixNano() {
		t.Fatalf("test bug: size/mtime not preserved (%d/%d vs %d/%d)",
			after.Size(), after.ModTime().UnixNano(), fi.Size(), fi.ModTime().UnixNano())
	}
	return content
}

// TestFoldRehashesDirtyPathWithPreservedStat is the sharp case: the change feed
// EXPLICITLY named this path dirty, so the fold has positive evidence the bytes
// moved. Reusing the previous ref because size+mtime happen to match makes the
// new checkpoint claim content that is not on disk, and says nothing about it:
// silent wrong content, the one failure mode worse than a named loss.
func TestFoldRehashesDirtyPathWithPreservedStat(t *testing.T) {
	root := t.TempDir()
	oc := mustObj(t, t.TempDir())
	p := filepath.Join(root, "main.go")
	writeFile(t, p, "package aaa\n", 0o644)

	base, err := Snapshot(root, oc, nil, 0, 100, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}

	want := rewritePreservingStat(t, p, "package bbb\n")

	m, err := SnapshotFold(root, oc, base, 1, 200, DURABLE, 0, map[string][]string{root: {p}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := oc.Get(m.Entries["main.go"].Ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("fold checkpointed stale content for a path the feed marked dirty:\n got: %q\nwant: %q", got, want)
	}
}

// TestSnapshotRehashesSameStatRewrite: the full scan has no dirty feed, only
// size+mtime, and a same-length rewrite with a restored mtime is invisible to
// that pair. Restoring this checkpoint must not hand back the previous bytes.
func TestSnapshotRehashesSameStatRewrite(t *testing.T) {
	root := t.TempDir()
	oc := mustObj(t, t.TempDir())
	p := filepath.Join(root, "main.go")
	writeFile(t, p, "package aaa\n", 0o644)

	base, err := Snapshot(root, oc, nil, 0, 100, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}

	want := rewritePreservingStat(t, p, "package bbb\n")

	m, err := Snapshot(root, oc, base, 1, 200, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := oc.Get(m.Entries["main.go"].Ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("full scan checkpointed stale content:\n got: %q\nwant: %q", got, want)
	}
}

// TestSnapshotRehashesSettledFileWithRestoredMtime is the realistic version of
// the same bug: an old, quiet file (well past the settle window, so its stat
// key IS trusted for reuse) rewritten by a tool that puts the timestamps back,
// which every "preserve mtime" flag does. Only the ctime half of the key can
// catch this one, because ctime is the single stat field userspace cannot set.
func TestSnapshotRehashesSettledFileWithRestoredMtime(t *testing.T) {
	root := t.TempDir()
	oc := mustObj(t, t.TempDir())
	p := filepath.Join(root, "vendored.go")
	writeFile(t, p, "package aaa\n", 0o644)

	// Two scans past the settle window: the second records a stat key that is
	// eligible for reuse (pinned by the read count below).
	time.Sleep(time.Duration(statSettleNS) + 100*time.Millisecond)
	m0, err := Snapshot(root, oc, nil, 0, 100, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := Snapshot(root, oc, m0, 1, 200, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}

	orig := readFile
	var reads int
	readFile = func(path string) ([]byte, error) {
		reads++
		return orig(path)
	}
	defer func() { readFile = orig }()

	if _, err := Snapshot(root, oc, trusted, 2, 300, DURABLE, 0); err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatalf("precondition: the file must be reuse-eligible here, got %d reads", reads)
	}

	want := rewritePreservingStat(t, p, "package bbb\n")

	m, err := Snapshot(root, oc, trusted, 3, 400, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := oc.Get(m.Entries["vendored.go"].Ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("timestamp-preserving rewrite went unnoticed:\n got: %q\nwant: %q", got, want)
	}
}

// TestIncrementalStillSkipsReadsForUnchangedFiles guards the other direction:
// tightening the reuse key must not degrade the scan into rehashing the whole
// tree at every boundary, which is the property that keeps checkpoints cheap.
// TestIncrementalReusesUnchangedRefs cannot see this (refs are
// content-addressed, so they match whether or not the bytes were re-read), so
// this counts actual reads through the readFile seam.
func TestIncrementalStillSkipsReadsForUnchangedFiles(t *testing.T) {
	root := t.TempDir()
	oc := mustObj(t, t.TempDir())
	for _, rel := range []string{"a.txt", "b.txt", "sub/c.txt"} {
		writeFile(t, filepath.Join(root, rel), "content "+rel+"\n", 0o644)
	}

	orig := readFile
	var reads []string
	readFile = func(p string) ([]byte, error) {
		reads = append(reads, filepath.Base(p))
		return orig(p)
	}
	defer func() { readFile = orig }()

	base, err := Snapshot(root, oc, nil, 0, 100, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 3 {
		t.Fatalf("baseline should read every file, read %v", reads)
	}

	// Inside the settle window the stat key is not yet trustworthy (a same-tick
	// write is invisible to it), so these files are read again: the bounded
	// price of the fix.
	reads = nil
	mid, err := Snapshot(root, oc, base, 1, 200, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 3 {
		t.Fatalf("files touched within the settle window must be re-read, read %v", reads)
	}

	// Trust is established by the scan that RECORDS the stat (only that scan can
	// know the file's timestamp tick was already over), so the first scan taken
	// more than a settle window after the last write reads the files once more.
	time.Sleep(time.Duration(statSettleNS) + 100*time.Millisecond)
	reads = nil
	settled, err := Snapshot(root, oc, mid, 2, 300, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 3 {
		t.Fatalf("the scan that settles the stat key still reads, read %v", reads)
	}

	// From there on an untouched tree costs stat calls only. This is the
	// property that keeps checkpoints cheap on a large tree, so it is pinned.
	reads = nil
	if _, err := Snapshot(root, oc, settled, 3, 400, DURABLE, 0); err != nil {
		t.Fatal(err)
	}
	if len(reads) != 0 {
		t.Fatalf("settled untouched tree must be rescanned without reading any file, read %v", reads)
	}
}
