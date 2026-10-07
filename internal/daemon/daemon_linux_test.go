package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/manyapn/checkpoint-public/internal/lineage"
	"github.com/manyapn/checkpoint-public/internal/snapshot"
	"github.com/manyapn/checkpoint-public/internal/writelog"
)

type harness struct {
	root, storeDir, sock string
	stop                 chan struct{}
	done                 chan error
	stopped              bool
	err                  error
}

func (h *harness) shutdown() error {
	if !h.stopped {
		h.stopped = true
		close(h.stop)
		h.err = <-h.done
	}
	return h.err
}

func start(t *testing.T) *harness {
	t.Helper()
	settleQuiet, settleCeiling = 50*time.Millisecond, 500*time.Millisecond
	base := t.TempDir()
	h := &harness{root: filepath.Join(base, "proj"), storeDir: filepath.Join(base, "store"),
		stop: make(chan struct{}), done: make(chan error, 1)}
	os.MkdirAll(h.root, 0o755)
	os.WriteFile(filepath.Join(h.root, "a.txt"), []byte("a0"), 0o644)
	h.sock = SocketPath(h.storeDir)
	ready := make(chan struct{})
	go func() { h.done <- Serve(Config{Root: h.root, StoreDir: h.storeDir}, ready, h.stop) }()
	select {
	case <-ready:
	case err := <-h.done:
		if errors.Is(err, unix.EPERM) {
			t.Skip("needs CAP_SYS_ADMIN")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { h.shutdown() })
	return h
}

func (h *harness) write(t *testing.T, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.root, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) read(rel string) string {
	b, _ := os.ReadFile(filepath.Join(h.root, rel))
	return string(b)
}

func TestSetupCheckpointThenBoundaryCut(t *testing.T) {
	h := start(t)
	st, err := GetStatus(h.sock)
	if err != nil || !st.Protected || st.Checkpoints != 1 {
		t.Fatalf("status = %+v, %v", st, err)
	}
	h.write(t, "b.txt", "b0")
	res, err := Checkpoint(h.sock, "run: test", "")
	if err != nil || res.ID != 1 || res.Entries != 2 || res.Badge != "Fully recoverable" {
		t.Fatalf("checkpoint = %+v, %v", res, err)
	}
	c, _ := snapshot.Load(h.storeDir, 1)
	if c.Source != "run: test" || c.Entries["b.txt"].Ref == "" {
		t.Fatalf("manifest = %+v", c)
	}
}

func TestDeletedFileSurvivesInWriteLog(t *testing.T) {
	h := start(t)
	h.write(t, "transient.txt", "never checkpointed")
	os.Remove(filepath.Join(h.root, "transient.txt"))
	Checkpoint(h.sock, "manual", "")
	writes, _ := writelog.Read(filepath.Join(h.storeDir, snapshot.LogFile))
	var found bool
	for _, w := range writes {
		if w.Op == writelog.Write && filepath.Base(w.Path) == "transient.txt" && w.Ref != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("transient write not recorded: %+v", writes)
	}
}

func TestAgentWritesAreAttributedByLineage(t *testing.T) {
	h := start(t)
	agent := exec.Command("sh", "-c", "sleep 0.2; echo agent > agent.txt; rm a.txt; sleep 0.1")
	agent.Dir = h.root
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	startTime, _ := lineage.StartTime(agent.Process.Pid)
	id := lineage.Identity{Pid: agent.Process.Pid, Start: startTime}
	if err := Register(h.sock, id); err != nil {
		t.Fatal(err)
	}
	// the test binary is the checkpoint binary, so a human must be another program
	human := exec.Command("sh", "-c", "echo me > human.txt")
	human.Dir = h.root
	human.Run()
	agent.Wait()
	Checkpoint(h.sock, "run: agent", "")
	Unregister(h.sock, id)
	writes, _ := writelog.Read(filepath.Join(h.storeDir, snapshot.LogFile))
	by := map[string]string{}
	for _, w := range writes {
		by[w.Op+" "+filepath.Base(w.Path)] = w.Writer
	}
	if by["write agent.txt"] != lineage.Agent || by["write human.txt"] != lineage.Human {
		t.Fatalf("attribution = %v", by)
	}
	st, _ := GetStatus(h.sock)
	if st.FeedActive && by["delete a.txt"] != lineage.Agent {
		t.Fatalf("delete should be the agent's: %v", by)
	}
}

func TestUnchangedWindowIsSkipped(t *testing.T) {
	h := start(t)
	st, _ := GetStatus(h.sock)
	if !st.FeedActive {
		t.Skip("skip-empty needs the change feed")
	}
	// The feed marks the whole filesystem; another process deleting a
	// directory on it can leave an unresolvable event, which counts as a
	// hole and rightly prevents a skip. That is not this test's subject.
	if st.Overflowed {
		t.Skip("another process on this filesystem made the window unprovable")
	}
	res, _ := Checkpoint(h.sock, "manual", "")
	if !res.Skipped || res.ID != 0 {
		t.Fatalf("expected skip, got %+v", res)
	}
	res, _ = Checkpoint(h.sock, "manual", "named")
	if res.Skipped || res.ID != 1 {
		t.Fatalf("a named save must always cut: %+v", res)
	}
}

func TestShutdownCutsFinalCheckpoint(t *testing.T) {
	h := start(t)
	h.write(t, "late.txt", "written after the last checkpoint")
	if err := h.shutdown(); err != nil {
		t.Fatal(err)
	}
	c, _ := snapshot.Latest(h.storeDir)
	if c.Source != "shutdown" || c.Entries["late.txt"].Ref == "" {
		t.Fatalf("latest = %+v", c)
	}
}

func TestRestartKeepsNumbering(t *testing.T) {
	h := start(t)
	Checkpoint(h.sock, "manual", "one")
	h.shutdown()
	ready, stop, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { done <- Serve(Config{Root: h.root, StoreDir: h.storeDir}, ready, stop) }()
	<-ready
	res, _ := Checkpoint(h.sock, "manual", "two")
	close(stop)
	<-done
	// setup(0), one(1), an empty shutdown window is skipped, so two is 2
	if res.ID != 2 {
		t.Fatalf("after setup(0) and one(1) the next id should be 2, got %d", res.ID)
	}
}
