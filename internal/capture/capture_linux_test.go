//go:build linux

package capture

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/provenance"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

// newOrSkip builds a Watcher, skipping the test when fanotify is unavailable
// (no CAP_SYS_ADMIN, or a filesystem that rejects the mount mark), so the suite
// degrades gracefully instead of failing on a host that cannot arm it.
func newOrSkip(t *testing.T, workspace, storeDir string) *Watcher {
	t.Helper()
	w, err := New(Config{Workspace: workspace, StoreDir: storeDir})
	if err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
			t.Skipf("fanotify unavailable in this environment: %v", err)
		}
		t.Fatalf("New: %v", err)
	}
	return w
}

// drainUntil polls Drain until pred is satisfied or the deadline passes,
// absorbing fanotify delivery latency deterministically.
func drainUntil(t *testing.T, w *Watcher, pred func([]versionlog.Version) bool) []versionlog.Version {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w.Drain()
		vs := w.Versions()
		if pred(vs) {
			return vs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for expected versions; have %+v", w.Versions())
	return nil
}

func versionFor(vs []versionlog.Version, suffix string) (versionlog.Version, bool) {
	for i := len(vs) - 1; i >= 0; i-- { // latest wins
		if strings.HasSuffix(vs[i].Path, suffix) {
			return vs[i], true
		}
	}
	return versionlog.Version{}, false
}

func content(t *testing.T, storeDir, ref string) []byte {
	t.Helper()
	oc, err := objstore.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := oc.Get(ref)
	if err != nil {
		t.Fatalf("Get(%s): %v", ref, err)
	}
	return b
}

// TestTransientRecoveredByteExact is the core capture invariant: a file
// created→written→closed→UNLINKED before any checkpoint is still captured
// byte-exact, because content is read through the kernel-retained fd.
func TestTransientRecoveredByteExact(t *testing.T) {
	ws := t.TempDir()
	storeDir := t.TempDir()
	w := newOrSkip(t, ws, storeDir)
	defer w.Close()

	want := []byte("transient content that outlives the file\n")
	p := filepath.Join(ws, "transient.txt")
	if err := os.WriteFile(p, want, 0o644); err != nil { // create+write+close
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil { // gone before any boundary
		t.Fatal(err)
	}

	vs := drainUntil(t, w, func(vs []versionlog.Version) bool {
		_, ok := versionFor(vs, "/transient.txt")
		return ok
	})
	v, _ := versionFor(vs, "/transient.txt")
	if got := content(t, storeDir, v.Ref); !bytes.Equal(got, want) {
		t.Fatalf("recovered transient bytes mismatch:\n got %q\nwant %q", got, want)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("file should still be gone from disk; recovery is from the store, not disk")
	}
}

// TestUnclosedWriteNotCapturedUntilClose is the honesty boundary: bytes written
// but not yet closed are NOT a version; the version appears only when the
// writable handle closes.
func TestUnclosedWriteNotCapturedUntilClose(t *testing.T) {
	ws := t.TempDir()
	storeDir := t.TempDir()
	w := newOrSkip(t, ws, storeDir)
	defer w.Close()

	p := filepath.Join(ws, "open.txt")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("half-written, still open\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil { // flushed to the fs, but handle still open
		t.Fatal(err)
	}

	// Give any (incorrect) event time to arrive; assert none did.
	for i := 0; i < 5; i++ {
		w.Drain()
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := versionFor(w.Versions(), "/open.txt"); ok {
		f.Close()
		t.Fatal("an unclosed write must not be captured (would be a false 'recoverable')")
	}

	// Now close: the version must appear.
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	drainUntil(t, w, func(vs []versionlog.Version) bool {
		_, ok := versionFor(vs, "/open.txt")
		return ok
	})
}

// TestMmapWithCloseCaptured: a memory-mapped write is captured
// when the file is closed afterward. mmap is not inherently invisible; only a
// mapping mutated with no flush/close before death is Unsupported.
func TestMmapWithCloseCaptured(t *testing.T) {
	ws := t.TempDir()
	storeDir := t.TempDir()
	w := newOrSkip(t, ws, storeDir)
	defer w.Close()

	p := filepath.Join(ws, "mapped.bin")
	want := bytes.Repeat([]byte("MMAP"), 1024) // 4 KiB
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(len(want))); err != nil {
		t.Fatal(err)
	}
	mm, err := unix.Mmap(int(f.Fd()), 0, len(want), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	copy(mm, want)
	if err := unix.Msync(mm, unix.MS_SYNC); err != nil {
		t.Fatal(err)
	}
	if err := unix.Munmap(mm); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil { // close triggers FAN_CLOSE_WRITE
		t.Fatal(err)
	}

	vs := drainUntil(t, w, func(vs []versionlog.Version) bool {
		_, ok := versionFor(vs, "/mapped.bin")
		return ok
	})
	v, _ := versionFor(vs, "/mapped.bin")
	if got := content(t, storeDir, v.Ref); !bytes.Equal(got, want) {
		t.Fatalf("mmap-with-close content mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

// TestQueueOverflowDetected forces a real fanotify queue overflow
// deterministically: storm far more close-writes than the
// default queue (16384) holds WITHOUT draining, then drain once and assert the
// overflow was detected. A checkpoint in this window would read as Incomplete.
func TestQueueOverflowDetected(t *testing.T) {
	ws := t.TempDir()
	storeDir := t.TempDir()
	w := newOrSkip(t, ws, storeDir)
	defer w.Close()

	// Fill the queue past its bound before reading a single event.
	const storm = 20000 // > FAN default queue depth (16384)
	for i := 0; i < storm; i++ {
		p := filepath.Join(ws, "f"+itoa(i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Now drain: the queue held more than it could, so a FAN_Q_OVERFLOW marker
	// is present. Drain repeatedly to consume the whole (truncated) queue.
	deadline := time.Now().Add(5 * time.Second)
	for !w.Overflowed() && time.Now().Before(deadline) {
		if w.Drain() == 0 {
			break
		}
	}
	if !w.Overflowed() {
		t.Fatal("a storm past the queue bound must be detected as overflow (Incomplete)")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

// TestExcludedAndOutsidePathsNotCaptured: bulk-excluded dirs and paths outside
// the workspace are never versioned.
func TestExcludedAndOutsidePathsNotCaptured(t *testing.T) {
	ws := t.TempDir()
	storeDir := t.TempDir()
	w := newOrSkip(t, ws, storeDir)
	defer w.Close()

	nm := filepath.Join(ws, "node_modules")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nm, "junk.js"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A real file to prove the watcher is live and draining.
	if err := os.WriteFile(filepath.Join(ws, "real.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	drainUntil(t, w, func(vs []versionlog.Version) bool {
		_, ok := versionFor(vs, "/real.txt")
		return ok
	})
	if _, ok := versionFor(w.Versions(), "/junk.js"); ok {
		t.Fatal("bulk-excluded node_modules path must not be captured")
	}
}

// TestOutsideBoundaryAgentWriteSurfaced: an AGENT-tree write
// landing outside every protected root is uncaptured but SURFACED as an
// unprotected change (distinct paths listed, every write counted); writes by
// non-agent processes and the store's own writes are not, because ambient mount
// noise must not drown the signal.
func TestOutsideBoundaryAgentWriteSurfaced(t *testing.T) {
	ws := t.TempDir()
	outsideDir := t.TempDir() // same mount, not protected
	storeDir := t.TempDir()
	w := newOrSkip(t, ws, storeDir)
	defer w.Close()

	self := provenance.Identity{Pid: os.Getpid()}
	if s, ok := provenance.StartTime(os.Getpid()); ok {
		self.Start = s
	}
	w.RegisterAgentRoot(self)

	// The agent's write must come from a DESCENDANT process, not from this one:
	// the test binary is the same executable as the capture code, so a direct
	// write here is checkpoint's OWN write (class "checkpoint"), exactly what
	// self-attribution is for. A real agent write comes from a child, which is
	// what this fixture models. The child lingers so its /proc entry is
	// still readable when the event is classified.
	writeViaAgentChild := func(path, content string) {
		t.Helper()
		cmd := exec.Command("bash", "-c", "printf %s \"$1\" > \"$2\"; sleep 0.4", "bash", content, path)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	}
	outPath := filepath.Join(outsideDir, "escaped.txt")
	waitOutside := func(wantCount int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			w.Drain()
			if paths, n := w.OutsideWrites(); n >= wantCount {
				if len(paths) != 1 || paths[0] != outPath {
					t.Fatalf("outside list must hold the one distinct path, got %v (count %d)", paths, n)
				}
				return
			}
			if time.Now().After(deadline) {
				paths, n := w.OutsideWrites()
				t.Fatalf("outside agent write never surfaced: %v count=%d want>=%d", paths, n, wantCount)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	writeViaAgentChild(outPath, "agent wrote here\n")
	waitOutside(1)
	// Second write to the same path (after the first event was drained, so
	// fanotify cannot merge them): counted again, listed once.
	writeViaAgentChild(outPath, "again\n")
	// A protected write must NOT land in the outside list.
	writeViaAgentChild(filepath.Join(ws, "in.txt"), "in\n")
	waitOutside(2)
	for _, v := range w.Versions() {
		if v.Path == outPath {
			t.Fatal("an outside-boundary write must not be captured as a version")
		}
	}

	// After the agent session ends, further outside writes are ambient noise:
	// not attributed, not surfaced.
	w.UnregisterAgentRoot(self)
	_, before := w.OutsideWrites()
	if err := os.WriteFile(filepath.Join(outsideDir, "ambient.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	w.Drain()
	if _, after := w.OutsideWrites(); after != before {
		t.Fatalf("non-session outside writes must not be surfaced, count %d -> %d", before, after)
	}
}

// TestCaptureNeverStoresSecrets pins the LIVE path, which is separate code
// from the boundary scan: a credential written while the watcher is running
// must never have its bytes read into the object store, and must not appear in
// the version log at all. (The scan path is pinned separately by store's tests.)
func TestCaptureNeverStoresSecrets(t *testing.T) {
	ws := t.TempDir()
	storeDir := t.TempDir()
	w := newOrSkip(t, ws, storeDir)
	defer w.Close()

	const secret = "API_KEY=sk-live-DO-NOT-CAPTURE\n"
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "certs/tls.key"), []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An ordinary file proves the watcher is live and draining.
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	drainUntil(t, w, func(vs []versionlog.Version) bool {
		_, ok := versionFor(vs, "/main.go")
		return ok
	})

	for _, v := range w.Versions() {
		if strings.HasSuffix(v.Path, "/.env") || strings.HasSuffix(v.Path, "/tls.key") {
			t.Fatalf("credential %s was versioned; it must never be captured", v.Path)
		}
	}
	oc, err := objstore.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := oc.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		b, err := oc.Get(ref)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), "DO-NOT-CAPTURE") || strings.Contains(string(b), "PRIVATE KEY") {
			t.Fatalf("credential content reached the object store as %s", ref)
		}
	}
}

// TestReadAllPartialThenErrorIsReported pins the content read against silent
// truncation: when read() hands back bytes and THEN fails with a non-EOF error,
// the bytes so far are a truncated prefix, not the file. A non-blocking pipe
// reproduces exactly that shape with a real kernel errno (data, then EAGAIN)
// with no mocking. Returning those bytes as a successful capture would store a
// truncated version and report it as fully recoverable.
func TestReadAllPartialThenErrorIsReported(t *testing.T) {
	var fds [2]int
	if err := unix.Pipe2(fds[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	const prefix = "first chunk of the file"
	if _, err := unix.Write(fds[1], []byte(prefix)); err != nil {
		t.Fatal(err)
	}
	// The writer is still open, so the reader is not at EOF: the second read
	// fails with EAGAIN, mid-"file".
	got, err := readAll(fds[0], int64(len(prefix)))
	if err == nil {
		t.Fatalf("a read that failed mid-file must be reported, got %d bytes (%q) and nil error", len(got), got)
	}
	if got != nil {
		t.Fatalf("a failed read must not hand back a truncated prefix, got %q", got)
	}
}

// TestPartialReadIsMissedNotTruncated drives the same failure through the real
// capture path: a close-write whose content read fails partway must be a NAMED
// miss (surfaced, downgrading the checkpoint), never a silently truncated
// version that reads as fully recoverable.
func TestPartialReadIsMissedNotTruncated(t *testing.T) {
	ws := t.TempDir()
	storeDir := t.TempDir()
	w := newOrSkip(t, ws, storeDir)
	defer w.Close()

	const prefix = "TRUNCATED-PREFIX"
	var calls int
	orig := readFD
	readFD = func(fd int, p []byte) (int, error) {
		calls++
		if calls == 1 {
			return copy(p, prefix), nil
		}
		return 0, unix.EIO // fails mid-file, after bytes were already handed back
	}
	defer func() { readFD = orig }()

	p := filepath.Join(ws, "half.txt")
	if err := os.WriteFile(p, bytes.Repeat([]byte("A"), 128*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for w.Missed() == 0 && time.Now().Before(deadline) {
		w.Drain()
		time.Sleep(20 * time.Millisecond)
	}
	readFD = orig

	if _, ok := versionFor(w.Versions(), "/half.txt"); ok {
		t.Fatal("a partially-read capture must not be versioned: it would report truncated content as recoverable")
	}
	if w.Missed() == 0 {
		t.Fatal("a failed content read must count as a missed capture")
	}
	found := false
	for _, mp := range w.MissedPaths() {
		if mp == p {
			found = true
		}
	}
	if !found {
		t.Fatalf("the missed path must be NAMED, got %v", w.MissedPaths())
	}
	oc, err := objstore.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := oc.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		b, err := oc.Get(ref)
		if err != nil {
			continue
		}
		if bytes.Contains(b, []byte(prefix)) {
			t.Fatalf("truncated content reached the object store as %s", ref)
		}
	}
}

// TestWorkspaceUnderExcludedParentIsCaptured: the bulk-exclude list names
// rebuildable directories INSIDE the protected root. A workspace that merely
// SITS under a directory with one of those names (/home/me/build/proj) is an
// ordinary project and must be captured in full; excluding it would leave the
// user unprotected while the tool reports the workspace protected.
func TestWorkspaceUnderExcludedParentIsCaptured(t *testing.T) {
	for _, parent := range []string{"build", "dist", "target", ".git", "node_modules", "__pycache__", ".venv"} {
		t.Run(parent, func(t *testing.T) {
			ws := filepath.Join(t.TempDir(), parent, "proj")
			if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
				t.Fatal(err)
			}
			storeDir := t.TempDir()
			w := newOrSkip(t, ws, storeDir)
			defer w.Close()

			want := []byte("package main\n")
			if err := os.WriteFile(filepath.Join(ws, "src/main.go"), want, 0o644); err != nil {
				t.Fatal(err)
			}
			vs := drainUntil(t, w, func(vs []versionlog.Version) bool {
				_, ok := versionFor(vs, "/src/main.go")
				return ok
			})
			v, _ := versionFor(vs, "/src/main.go")
			if got := content(t, storeDir, v.Ref); !bytes.Equal(got, want) {
				t.Fatalf("content mismatch: got %q want %q", got, want)
			}

			// Deletes take the same exclusion decision and must agree.
			del := filepath.Join(ws, "src/gone.go")
			if err := w.RecordDelete(del, int32(os.Getpid())); err != nil {
				t.Fatal(err)
			}
			if _, ok := versionFor(w.Versions(), "/src/gone.go"); !ok {
				t.Fatal("a delete inside the workspace must be journaled, whatever the workspace's parent is named")
			}

			// Exclusion WITHIN the workspace is unchanged.
			inner := filepath.Join(ws, "node_modules", "deep")
			if err := os.MkdirAll(inner, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(inner, "junk.js"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws, "after.txt"), []byte("y\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			drainUntil(t, w, func(vs []versionlog.Version) bool {
				_, ok := versionFor(vs, "/after.txt")
				return ok
			})
			if _, ok := versionFor(w.Versions(), "/junk.js"); ok {
				t.Fatal("a rebuildable dir inside the workspace must still be excluded")
			}
		})
	}
}
