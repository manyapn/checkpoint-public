//go:build linux

package feed

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// ext4Dir provisions a loopback-ext4 mount for the test: container root
// filesystems are usually overlayfs, which refuses filesystem marks outright.
// Skips when the environment cannot mount (no privilege, no mkfs).
func ext4Dir(t *testing.T) string {
	t.Helper()
	img := filepath.Join(t.TempDir(), "ext4.img")
	if out, err := exec.Command("dd", "if=/dev/zero", "of="+img, "bs=1M", "count=64").CombinedOutput(); err != nil {
		t.Skipf("dd: %v: %s", err, out)
	}
	if out, err := exec.Command("mkfs.ext4", "-q", img).CombinedOutput(); err != nil {
		t.Skipf("mkfs.ext4: %v: %s", err, out)
	}
	mnt := filepath.Join(t.TempDir(), "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mount", "-o", "loop", img, mnt).CombinedOutput(); err != nil {
		t.Skipf("cannot mount loopback ext4 (needs privilege): %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("umount", mnt).Run() })
	return mnt
}

// TestFeedDirentEventsOnExt4: the feed reports create/write/rename/delete with
// the acting pid and absolute paths, filtered to the protected roots.
func TestFeedDirentEventsOnExt4(t *testing.T) {
	mnt := ext4Dir(t)
	root := filepath.Join(mnt, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}

	f, err := New([]string{root})
	if err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skipf("fanotify unavailable: %v", err)
		}
		t.Fatalf("New on ext4: %v", err)
	}
	defer f.Close()

	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(sub, "a.txt")
	if err := os.WriteFile(a, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(sub, "b.txt")
	if err := os.Rename(a, b); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	// Outside every protected root: must not be reported.
	outside := filepath.Join(mnt, "outside.txt")
	if err := os.WriteFile(outside, []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := map[string]Event{}
	for _, e := range f.Drain() {
		got[string(e.Op)+" "+e.Path] = e
	}
	self := int32(os.Getpid())
	for _, want := range []string{
		"create " + sub,
		"create " + a,
		"write " + a,
		"rename-from " + a,
		"rename-to " + b,
		"delete " + b,
	} {
		e, ok := got[want]
		if !ok {
			t.Fatalf("missing event %q; got %v", want, keys(got))
		}
		if e.Pid != self {
			t.Fatalf("%q pid = %d, want self %d", want, e.Pid, self)
		}
	}
	if e, ok := got["create "+sub]; !ok || !e.Dir {
		t.Fatal("mkdir must be reported with Dir=true")
	}
	for k := range got {
		if filepath.Base(k) == "outside.txt" {
			t.Fatalf("unprotected path leaked into the feed: %s", k)
		}
	}
	if f.Overflowed() {
		t.Fatal("no overflow expected in this tiny window")
	}
}

// TestFeedUnsupportedDegrades: on a filesystem that refuses filesystem marks
// (overlayfs, the usual container root fs), New reports ErrUnsupported so the
// daemon can fall back to scan mode. Conditional: skips when the temp dir's
// filesystem actually supports marks (e.g. running on a real ext4 host).
func TestFeedUnsupportedDegrades(t *testing.T) {
	dir := t.TempDir()
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	const overlayfsMagic = 0x794c7630
	if st.Type != overlayfsMagic {
		t.Skipf("temp dir is not overlayfs (type %#x); degrade path not exercisable here", st.Type)
	}
	_, err := New([]string{dir})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("overlayfs must degrade with ErrUnsupported, got %v", err)
	}
}

func keys(m map[string]Event) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
