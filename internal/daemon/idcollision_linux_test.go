//go:build linux

// Checkpoint-id collisions between the daemon's cuts and another writer (a CLI
// pre-op snapshot). The rule: checkpoint ids are never silently overwritten.
// Concurrent writers collide into an explicit refusal, and the loser retries
// with a fresh id rather than destroying the winner's manifest.
package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/manyapn/checkpoint-public/internal/store"
)

// TestBoundaryRetriesAfterIdCollision: another writer takes the id the daemon
// was about to use. The daemon must NOT overwrite it: it re-syncs numbering
// and writes a fresh id, and the foreign manifest survives byte-for-byte.
func TestBoundaryRetriesAfterIdCollision(t *testing.T) {
	setSettle(t, 100*time.Millisecond, 2*time.Second)
	root := t.TempDir()
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha\n")

	stop := startDaemonOrSkip(t, Config{Workspace: root, StoreDir: storeDir})
	defer stop()
	awaitSetupDone(t, SocketPath(storeDir))

	// Setup took id 0, so the daemon's next cut wants id 1. Another writer (a
	// CLI pre-op snapshot) claims 1 first.
	foreign := &store.Manifest{
		ID: 1, TimeNS: time.Now().UnixNano(), Root: root,
		Coverage: store.DURABLE, Entries: map[string]store.Entry{},
		Source: "pre-op snapshot (other writer)",
	}
	if err := store.Write(storeDir, foreign); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(root, "b.txt"), "beta\n")
	resp, err := RequestCheckpoint(SocketPath(storeDir), "run: after collision")
	if err != nil {
		t.Fatalf("boundary after id collision must succeed with a fresh id: %v", err)
	}
	if resp.ID == 1 {
		t.Fatal("the daemon reused the colliding id")
	}
	if resp.ID != 2 {
		t.Fatalf("expected the next free id 2, got %d", resp.ID)
	}

	// The other writer's manifest is intact, never silently replaced.
	kept, err := store.Load(storeDir, 1)
	if err != nil {
		t.Fatalf("the foreign manifest must survive: %v", err)
	}
	if kept.Source != foreign.Source || len(kept.Entries) != 0 {
		t.Fatalf("foreign manifest was overwritten: %+v", kept)
	}
	// The daemon's own checkpoint holds this window's content.
	m, err := store.Load(storeDir, resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Entries["b.txt"]; !ok {
		t.Fatalf("the retried checkpoint must contain the window's write; entries: %v", keysOf(m.Entries))
	}
	if m.Source != "run: after collision" {
		t.Fatalf("source lost across the retry: %q", m.Source)
	}
}
