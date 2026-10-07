package watch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func needRoot(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, unix.EPERM) {
		t.Skip("needs CAP_SYS_ADMIN")
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestWritesReportCloseWriteWithReadableFd(t *testing.T) {
	root := t.TempDir()
	w, err := OpenWrites(root)
	needRoot(t, err)
	defer w.Close()
	path := filepath.Join(root, "a.txt")
	os.WriteFile(path, []byte("hello"), 0o644)
	os.Remove(path)
	var got []string
	var content []byte
	for i := 0; i < 20 && len(got) == 0; i++ {
		Poll(100, w.Fd())
		w.Drain(func(p string, fd int, pid int) {
			if !strings.HasPrefix(p, root+"/") {
				return // the mark covers the whole mount; other tests write too
			}
			got = append(got, p)
			buf := make([]byte, 16)
			n, _ := unix.Read(fd, buf)
			content = buf[:n]
			if pid != os.Getpid() {
				t.Errorf("pid = %d, want %d", pid, os.Getpid())
			}
		})
	}
	if len(got) != 1 || got[0] != path || string(content) != "hello" {
		t.Fatalf("got %v content %q", got, content)
	}
}

func TestChangesReportDeleteWithPid(t *testing.T) {
	root := t.TempDir()
	c, err := OpenChanges(root)
	if errors.Is(err, ErrUnsupported) {
		t.Skip("filesystem has no change feed:", err)
	}
	needRoot(t, err)
	defer c.Close()
	path := filepath.Join(root, "doomed.txt")
	os.WriteFile(path, []byte("x"), 0o644)
	os.Remove(path)
	os.Rename(filepath.Join(root), filepath.Join(root)) // no-op rename keeps the test honest about Drain
	seen := map[string]bool{}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !(seen[Created] && seen[Deleted] && seen[Written]) {
		Poll(100, c.Fd())
		for _, ch := range c.Drain() {
			if ch.Path == path && ch.Pid == os.Getpid() {
				seen[ch.Op] = true
			}
		}
	}
	if !seen[Created] || !seen[Deleted] || !seen[Written] {
		t.Fatalf("saw %v", seen)
	}
	if c.Overflowed {
		t.Fatal("unexpected overflow")
	}
}

func TestChangesReportRenameBothNames(t *testing.T) {
	root := t.TempDir()
	c, err := OpenChanges(root)
	if errors.Is(err, ErrUnsupported) {
		t.Skip("no change feed here")
	}
	needRoot(t, err)
	defer c.Close()
	from, to := filepath.Join(root, "old"), filepath.Join(root, "new")
	os.WriteFile(from, []byte("x"), 0o644)
	os.Rename(from, to)
	seen := map[string]string{}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(seen) < 2 {
		Poll(100, c.Fd())
		for _, ch := range c.Drain() {
			if ch.Op == RenamedFrom || ch.Op == RenamedTo {
				seen[ch.Op] = ch.Path
			}
		}
	}
	if seen[RenamedFrom] != from || seen[RenamedTo] != to {
		t.Fatalf("rename seen as %v", seen)
	}
}
