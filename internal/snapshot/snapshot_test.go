package snapshot

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/manyapn/checkpoint-public/internal/objects"
	"github.com/manyapn/checkpoint-public/internal/writelog"
)

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, mode)
}

func fixture(t *testing.T) (root, storeDir string, objs *objects.Store) {
	t.Helper()
	base := t.TempDir()
	root, storeDir = filepath.Join(base, "proj"), filepath.Join(base, "store")
	write(t, filepath.Join(root, "main.go"), "package main\n", 0o644)
	write(t, filepath.Join(root, "bin/run.sh"), "#!/bin/sh\n", 0o755)
	write(t, filepath.Join(root, "node_modules/x/i.js"), "bulk", 0o644)
	write(t, filepath.Join(root, ".env"), "SECRET=1", 0o600)
	os.Mkdir(filepath.Join(root, "empty"), 0o755)
	os.Symlink("main.go", filepath.Join(root, "link"))
	objs, _ = objects.Open(storeDir)
	return root, storeDir, objs
}

func TestScanRecordsTreeAndNamesSkips(t *testing.T) {
	root, _, objs := fixture(t)
	c, err := Scan(root, objs, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bin", "bin/run.sh", "empty", "link", "main.go"}
	var got []string
	for rel := range c.Entries {
		got = append(got, rel)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	if c.Entries["bin/run.sh"].Mode != 0o755 || c.Entries["link"].Link != "main.go" || c.Entries["empty"].Kind != Dir {
		t.Fatalf("metadata wrong: %+v", c.Entries)
	}
	if len(c.Exceptions) != 1 || c.Exceptions[0].Path != ".env" {
		t.Fatalf("expected .env as the one named exception, got %+v", c.Exceptions)
	}
	if c.Badge() != "Recoverable with exceptions" {
		t.Fatal(c.Badge())
	}
}

func TestRestoreAfterRmRfIsByteExact(t *testing.T) {
	root, storeDir, objs := fixture(t)
	c, _ := Scan(root, objs, nil)
	if err := Commit(storeDir, c); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(root)
	loaded, err := Load(storeDir, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(loaded, objs, root, true); err != nil {
		t.Fatal(err)
	}
	again, _ := Scan(root, objs, nil)
	for rel, e := range c.Entries {
		g := again.Entries[rel]
		if g.Kind != e.Kind || g.Ref != e.Ref || g.Mode != e.Mode || g.Link != e.Link {
			t.Fatalf("%s differs after restore: %+v vs %+v", rel, g, e)
		}
	}
}

func TestExactRestoreRemovesStrangersButNotExcludedDirs(t *testing.T) {
	root, _, objs := fixture(t)
	c, _ := Scan(root, objs, nil)
	write(t, filepath.Join(root, "stray.txt"), "new", 0o644)
	removed, err := Restore(c, objs, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, []string{"stray.txt"}) {
		t.Fatalf("removed = %v", removed)
	}
	for _, keep := range []string{"node_modules/x/i.js", ".env"} {
		if _, err := os.Stat(filepath.Join(root, keep)); err != nil {
			t.Fatalf("%s must survive an exact restore", keep)
		}
	}
}

func TestRestoreNeverWritesThroughSymlink(t *testing.T) {
	root, _, objs := fixture(t)
	c, _ := Scan(root, objs, nil)
	outside := t.TempDir()
	os.RemoveAll(filepath.Join(root, "bin"))
	os.Symlink(outside, filepath.Join(root, "bin"))
	if _, err := Restore(c, objs, root, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "run.sh")); err == nil {
		t.Fatal("restore followed a symlink out of the target")
	}
	if fi, _ := os.Lstat(filepath.Join(root, "bin")); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("symlink should have been replaced by a real dir")
	}
}

func TestScanReusesUnchangedFilesAndSeesChanges(t *testing.T) {
	root, _, objs := fixture(t)
	first, _ := Scan(root, objs, nil)
	// backdate so the ctime settle window does not force a re-read
	first.ScanNS += int64(2 * time.Second)
	write(t, filepath.Join(root, "main.go"), "package changed\n", 0o644)
	second, _ := Scan(root, objs, first)
	if second.Entries["main.go"].Ref == first.Entries["main.go"].Ref {
		t.Fatal("changed file must get a new ref")
	}
	if second.Entries["bin/run.sh"].Ref != first.Entries["bin/run.sh"].Ref {
		t.Fatal("unchanged file must keep its ref")
	}
}

func TestFoldMatchesScan(t *testing.T) {
	root, _, objs := fixture(t)
	prev, _ := Scan(root, objs, nil)
	write(t, filepath.Join(root, "main.go"), "v2", 0o644)
	write(t, filepath.Join(root, "new/deep/f.txt"), "x", 0o644)
	os.Remove(filepath.Join(root, "bin/run.sh"))
	changed := []string{
		filepath.Join(root, "main.go"), filepath.Join(root, "new"), filepath.Join(root, "bin/run.sh"),
	}
	folded, err := Fold(root, objs, prev, changed)
	if err != nil {
		t.Fatal(err)
	}
	scanned, _ := Scan(root, objs, nil)
	for rel, e := range scanned.Entries {
		if f := folded.Entries[rel]; f.Kind != e.Kind || f.Ref != e.Ref || f.Link != e.Link {
			t.Fatalf("fold differs at %s: %+v vs %+v", rel, f, e)
		}
	}
	if len(folded.Entries) != len(scanned.Entries) {
		t.Fatalf("fold has %d entries, scan %d", len(folded.Entries), len(scanned.Entries))
	}
}

func TestCommitNeverOverwrites(t *testing.T) {
	root, storeDir, objs := fixture(t)
	a, _ := Scan(root, objs, nil)
	b, _ := Scan(root, objs, nil)
	Commit(storeDir, a)
	Commit(storeDir, b)
	if a.ID == b.ID {
		t.Fatal("two commits got the same id")
	}
	ids, _ := IDs(storeDir)
	if !reflect.DeepEqual(ids, []int{0, 1}) {
		t.Fatal(ids)
	}
}

func TestPruneKeepsNamedAndLatest(t *testing.T) {
	root, storeDir, objs := fixture(t)
	now := time.Now()
	mk := func(name string, age time.Duration, content string) *Checkpoint {
		write(t, filepath.Join(root, "main.go"), content, 0o644)
		c, _ := Scan(root, objs, nil)
		c.Name, c.TimeNS = name, now.Add(-age).UnixNano()
		Commit(storeDir, c)
		return c
	}
	old := mk("", 30*24*time.Hour, "old")
	named := mk("keep-me", 20*24*time.Hour, "named")
	latest := mk("", 10*24*time.Hour, "latest")
	rep, err := Prune(storeDir, objs, 7, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.RemovedCheckpoints, []int{old.ID}) {
		t.Fatalf("removed %v", rep.RemovedCheckpoints)
	}
	if objs.Has(old.Entries["main.go"].Ref) {
		t.Fatal("old content should be garbage collected")
	}
	for _, c := range []*Checkpoint{named, latest} {
		if _, err := Load(storeDir, c.ID); err != nil {
			t.Fatalf("checkpoint %d should survive", c.ID)
		}
		if !objs.Has(c.Entries["main.go"].Ref) {
			t.Fatal("kept checkpoint lost its content")
		}
	}
}

func TestSalvageOffersOnlyUnheldPaths(t *testing.T) {
	root, _, objs := fixture(t)
	c, _ := Scan(root, objs, nil)
	ref, _ := objs.Put([]byte("transient"))
	writes := []writelog.Entry{
		{Op: writelog.Write, Path: root + "/main.go", Ref: c.Entries["main.go"].Ref},
		{Op: writelog.Write, Path: root + "/tmp.txt", Ref: ref},
		{Op: writelog.Delete, Path: root + "/tmp.txt"},
	}
	got := Salvage(writes, []*Checkpoint{c}, objs)
	if !reflect.DeepEqual(got, map[string]string{root + "/tmp.txt": ref}) {
		t.Fatalf("salvage = %v", got)
	}
}

func TestMeasureUsageCountsObjectsAndCheckpoints(t *testing.T) {
	root, storeDir, objs := fixture(t)
	c, _ := Scan(root, objs, nil)
	Commit(storeDir, c)
	u := MeasureUsage(storeDir)
	if u.Objects != 2 || u.Checkpoints != 1 || u.Bytes == 0 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestSecretAndStoreLocationRules(t *testing.T) {
	for _, p := range []string{".env", ".env.local", "a/.ssh/id", "x/id_rsa", "c.pem", ".config/gh/hosts.yml"} {
		if !IsSecret(p) {
			t.Errorf("%s should be a secret", p)
		}
	}
	for _, p := range []string{".envrc", "gh/main.go", "src/key.go", "environment.txt"} {
		if IsSecret(p) {
			t.Errorf("%s should not be a secret", p)
		}
	}
	if CheckStoreLocation("/p/store", "/p") == nil || CheckStoreLocation("/p", "/p") == nil {
		t.Error("in-tree store must be refused")
	}
	if CheckStoreLocation("/s", "/p") != nil {
		t.Error("out-of-tree store must be allowed")
	}
}
