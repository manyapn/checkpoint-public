package undo

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/manyapn/checkpoint-public/internal/objects"
	"github.com/manyapn/checkpoint-public/internal/snapshot"
	"github.com/manyapn/checkpoint-public/internal/writelog"
)

func setup(t *testing.T) (root string, objs *objects.Store, baseline *snapshot.Checkpoint) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "proj")
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(root, "agent.txt"), []byte("before"), 0o644)
	os.WriteFile(filepath.Join(root, "human.txt"), []byte("mine"), 0o644)
	os.WriteFile(filepath.Join(root, "shared.txt"), []byte("v0"), 0o644)
	objs, _ = objects.Open(filepath.Join(base, "store"))
	baseline, _ = snapshot.Scan(root, objs, nil)
	return root, objs, baseline
}

func entry(root, rel, writer, content string, objs *objects.Store) writelog.Entry {
	ref, _ := objs.Put([]byte(content))
	os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644)
	return writelog.Entry{Op: writelog.Write, Path: root + "/" + rel, Ref: ref, Writer: writer}
}

func TestPlanAndApply(t *testing.T) {
	root, objs, baseline := setup(t)
	os.MkdirAll(filepath.Join(root, "gen"), 0o755)
	window := []writelog.Entry{
		entry(root, "agent.txt", "agent", "after", objs),
		entry(root, "human.txt", "human", "still mine", objs),
		entry(root, "shared.txt", "agent", "v1", objs),
		entry(root, "shared.txt", "human", "v2", objs),
		entry(root, "gen/new.txt", "agent", "generated", objs),
		entry(root, "mystery.txt", "unknown", "?", objs),
	}
	steps := Plan(baseline, window, root, nil)
	got := map[string]string{}
	for _, s := range steps {
		got[s.Rel] = s.Action
	}
	want := map[string]string{"agent.txt": Revert, "shared.txt": Conflict, "gen/new.txt": Remove}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
	res := Apply(steps, objs, root)
	if len(res.Errors) > 0 {
		t.Fatal(res.Errors)
	}
	read := func(rel string) string {
		b, _ := os.ReadFile(filepath.Join(root, rel))
		return string(b)
	}
	if read("agent.txt") != "before" || read("human.txt") != "still mine" || read("shared.txt") != "v2" {
		t.Fatalf("after undo: agent=%q human=%q shared=%q", read("agent.txt"), read("human.txt"), read("shared.txt"))
	}
	if _, err := os.Stat(filepath.Join(root, "gen/new.txt")); !os.IsNotExist(err) {
		t.Fatal("agent-created file should be removed")
	}
	if !reflect.DeepEqual(res.Conflicts, []string{"shared.txt"}) {
		t.Fatal(res.Conflicts)
	}
}

func TestSelfWritesNeverConflict(t *testing.T) {
	root, objs, baseline := setup(t)
	window := []writelog.Entry{
		entry(root, "agent.txt", "agent", "after", objs),
		entry(root, "agent.txt", "self", "before", objs),
	}
	steps := Plan(baseline, window, root, nil)
	if len(steps) != 1 || steps[0].Action != Revert {
		t.Fatalf("a restore by checkpoint itself must not block a later undo: %+v", steps)
	}
}

func TestOnlyLimitsThePlan(t *testing.T) {
	root, objs, baseline := setup(t)
	window := []writelog.Entry{
		entry(root, "agent.txt", "agent", "after", objs),
		entry(root, "shared.txt", "agent", "v1", objs),
	}
	steps := Plan(baseline, window, root, []string{"shared.txt"})
	if len(steps) != 1 || steps[0].Rel != "shared.txt" {
		t.Fatalf("got %+v", steps)
	}
}

func TestSaveBothKeepsLiveFile(t *testing.T) {
	root, objs, baseline := setup(t)
	window := []writelog.Entry{
		entry(root, "shared.txt", "agent", "v1", objs),
		entry(root, "shared.txt", "human", "v2", objs),
	}
	steps := Plan(baseline, window, root, nil)
	saved, errs := SaveBoth(steps, objs, root, ".checkpoint-0")
	if len(errs) > 0 || !reflect.DeepEqual(saved, []string{"shared.txt.checkpoint-0"}) {
		t.Fatalf("saved %v errs %v", saved, errs)
	}
	live, _ := os.ReadFile(filepath.Join(root, "shared.txt"))
	side, _ := os.ReadFile(filepath.Join(root, "shared.txt.checkpoint-0"))
	if string(live) != "v2" || string(side) != "v0" {
		t.Fatalf("live=%q side=%q", live, side)
	}
}

func TestTurnSkipsBookkeeping(t *testing.T) {
	root, objs, _ := setup(t)
	storeDir := filepath.Dir(root) + "/store"
	for _, src := range []string{"setup", "run: agent", PreUndo} {
		c, _ := snapshot.Scan(root, objs, nil)
		c.Source = src
		snapshot.Commit(storeDir, c)
	}
	turn, baseline, err := Turn(storeDir)
	if err != nil || turn.Source != "run: agent" || baseline.Source != "setup" {
		t.Fatalf("turn=%+v baseline=%+v err=%v", turn, baseline, err)
	}
}
