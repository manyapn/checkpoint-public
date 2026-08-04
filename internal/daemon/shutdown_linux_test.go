//go:build linux

// Clean-shutdown durability and store-identity refusal. Both are driven through
// the daemon's external surface (Serve + the socket protocol) and verified
// against the store on disk.
package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manyapn/checkpoint-public/internal/store"
)

// TestShutdownCutsFinalCheckpoint: a write that closed after the last
// boundary must not survive only as salvage. A clean shutdown (the stop
// channel: SIGTERM/SIGINT, `protect --stop`) drains and cuts one final DURABLE
// checkpoint with source "shutdown" that contains the pending write.
func TestShutdownCutsFinalCheckpoint(t *testing.T) {
	root := t.TempDir()
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(root, "pre.txt"), "existed before protection\n")

	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})
	awaitSetupDone(t, SocketPath(storeDir))
	before, err := store.IDs(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("expected the setup checkpoint only, got %v", before)
	}

	// Written and closed with no boundary requested.
	writeFile(t, filepath.Join(root, "pending.txt"), "closed but never checkpointed\n")

	stop()

	after, err := store.IDs(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("clean shutdown must cut one final checkpoint, ids before=%v after=%v", before, after)
	}
	m, err := store.Load(storeDir, after[len(after)-1])
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != "shutdown" {
		t.Fatalf("final checkpoint source = %q, want %q", m.Source, "shutdown")
	}
	if m.Coverage != store.DURABLE {
		t.Fatalf("shutdown checkpoint coverage = %q, want DURABLE", m.Coverage)
	}
	if _, ok := m.Entries["pending.txt"]; !ok {
		t.Fatalf("the write pending at shutdown must be in the shutdown checkpoint; entries: %v", keysOf(m.Entries))
	}
	if _, ok := m.Entries["pre.txt"]; !ok {
		t.Fatal("the shutdown checkpoint must still cover the rest of the tree")
	}
}

// TestShutdownSkipsEmptyWindow is the other half: the shutdown cut obeys the
// existing skip-empty rule, so a restart-then-stop with nothing written adds no
// manifest. Emptiness is only provable with the change-feed live (ext4); in
// scan mode the daemon must keep cutting, because deletes are invisible there.
func TestShutdownSkipsEmptyWindow(t *testing.T) {
	mnt := ext4Dir(t)
	root := filepath.Join(mnt, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(mnt, "store")
	writeFile(t, filepath.Join(root, "f.txt"), "content\n")

	cfg := Config{Workspace: root, StoreDir: storeDir}
	stop := startDaemonOrSkip(t, cfg)
	st := awaitSetupDone(t, SocketPath(storeDir))
	if !st.FeedActive {
		stop()
		t.Skip("change-feed required: in scan mode emptiness cannot be proven, so shutdown always cuts")
	}
	writeFile(t, filepath.Join(root, "pending.txt"), "closed but never checkpointed\n")
	stop()

	ids, err := store.IDs(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("setup + shutdown expected, got %v", ids)
	}

	// Second run: nothing is written, so the shutdown window is provably empty
	// and must mint nothing.
	stop2 := startDaemonOrSkip(t, cfg)
	awaitSetupDone(t, SocketPath(storeDir))
	stop2()

	again, err := store.IDs(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(ids) {
		t.Fatalf("a shutdown over an empty window must cut nothing, ids %v -> %v", ids, again)
	}
}

// TestServeRefusesForeignWorkspaceStore: a store stamped for workspace
// A must not be served for workspace B, which would mix two projects into one
// history (and one incremental fold). Serve must refuse, naming the stamped
// workspace, before it becomes ready or binds a socket. The check runs before
// fanotify is armed, so it needs no privilege.
func TestServeRefusesForeignWorkspaceStore(t *testing.T) {
	wsA := t.TempDir()
	wsB := t.TempDir()
	storeDir := t.TempDir()
	if _, err := store.EnsureMeta(storeDir, wsA); err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{})
	stopCh := make(chan struct{})
	defer close(stopCh)
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(Config{Workspace: wsB, StoreDir: storeDir}, ready, stopCh) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Serve accepted a store stamped for a different workspace")
		}
		if !strings.Contains(err.Error(), wsA) {
			t.Fatalf("refusal must name the workspace the store belongs to (%s): %v", wsA, err)
		}
	case <-ready:
		t.Fatal("daemon reached READY on a store stamped for a different workspace")
	case <-time.After(10 * time.Second):
		t.Fatal("Serve neither refused nor became ready")
	}
	if _, err := os.Stat(SocketPath(storeDir)); !os.IsNotExist(err) {
		t.Fatalf("a refused start must not leave a socket behind (stat err: %v)", err)
	}
}

func keysOf(m map[string]store.Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
