package undo

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/oplog"
	"github.com/manyapn/checkpoint-public/internal/store"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

const root = "/w"

func ver(path, writer string) versionlog.Version {
	return versionlog.Version{Op: versionlog.OpModify, Path: root + "/" + path, Writer: writer}
}

// baselineWith builds a baseline manifest whose named files hold the given
// content in oc, so restore targets are materializable.
func baselineWith(t *testing.T, oc *objstore.Store, files map[string]string) *store.Manifest {
	t.Helper()
	m := &store.Manifest{Root: root, Entries: map[string]store.Entry{}}
	for rel, content := range files {
		ref, _, err := oc.Put([]byte(content))
		if err != nil {
			t.Fatal(err)
		}
		m.Entries[rel] = store.Entry{Kind: store.KindFile, Ref: ref, Mode: 0o644}
	}
	return m
}

func actions(p *Plan) map[string]Action {
	m := map[string]Action{}
	for _, e := range p.Entries {
		m[e.Rel] = e.Action
	}
	return m
}

func TestPlanAgentOnlyModifyIsRestore(t *testing.T) {
	oc := mustObj(t)
	base := baselineWith(t, oc, map[string]string{"a.txt": "before\n"})
	window := []versionlog.Version{ver("a.txt", "agent")}
	p := BuildPlan(base, window, root, nil)
	if actions(p)["a.txt"] != Restore {
		t.Fatalf("agent-only modify of an existing file must be Restore, got %v", actions(p))
	}
}

func TestPlanAgentCreatedIsDelete(t *testing.T) {
	oc := mustObj(t)
	base := baselineWith(t, oc, nil) // file did not exist pre-turn
	window := []versionlog.Version{ver("new.txt", "agent")}
	p := BuildPlan(base, window, root, nil)
	if actions(p)["new.txt"] != Delete {
		t.Fatalf("agent-created file must be Delete on undo, got %v", actions(p))
	}
}

func TestPlanHumanTouchedIsConflict(t *testing.T) {
	oc := mustObj(t)
	base := baselineWith(t, oc, map[string]string{"a.txt": "before\n"})
	// agent AND human both wrote a.txt in the window -> NeedsReview.
	window := []versionlog.Version{ver("a.txt", "agent"), ver("a.txt", "human")}
	p := BuildPlan(base, window, root, nil)
	if actions(p)["a.txt"] != Conflict {
		t.Fatalf("a file the human also touched must be Conflict, got %v", actions(p))
	}
}

func TestPlanUnknownTouchedIsConflict(t *testing.T) {
	oc := mustObj(t)
	base := baselineWith(t, oc, map[string]string{"a.txt": "before\n"})
	window := []versionlog.Version{ver("a.txt", "agent"), ver("a.txt", "unknown")}
	p := BuildPlan(base, window, root, nil)
	if actions(p)["a.txt"] != Conflict {
		t.Fatalf("a file with an unknown writer must be Conflict, got %v", actions(p))
	}
}

func TestPlanPurelyHumanFileIgnored(t *testing.T) {
	oc := mustObj(t)
	base := baselineWith(t, oc, nil)
	// human-only edit: not the agent's change -> not a candidate at all.
	window := []versionlog.Version{ver("mine.txt", "human")}
	p := BuildPlan(base, window, root, nil)
	if _, present := actions(p)["mine.txt"]; present {
		t.Fatalf("a purely-human file must not appear in the undo plan, got %v", actions(p))
	}
}

func TestPlanOnlyFilter(t *testing.T) {
	oc := mustObj(t)
	base := baselineWith(t, oc, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	window := []versionlog.Version{ver("a.txt", "agent"), ver("b.txt", "agent")}
	p := BuildPlan(base, window, root, []string{"a.txt"})
	got := actions(p)
	if _, ok := got["a.txt"]; !ok {
		t.Fatal("--only a.txt should keep a.txt")
	}
	if _, ok := got["b.txt"]; ok {
		t.Fatal("--only a.txt should drop b.txt")
	}
}

// TestApplyRevertsAgentPreservesHuman is the core undo guarantee end-to-end
// (pure filesystem, no fanotify): agent-only files revert to baseline byte-exact,
// agent-created files are removed, conflicted + human files are left untouched.
// TestPlanRestoresAgentDeletedFile: with delete provenance (an OpDelete version
// from the change-feed), a file the agent removed becomes a Restore candidate
// and its baseline content comes back. A human's deletion stays untouched, and an
// agent delete of a file the human also wrote is a conflict.
func TestPlanRestoresAgentDeletedFile(t *testing.T) {
	oc := mustObj(t)
	baseline := baselineWith(t, oc, map[string]string{
		"agent-rm.txt": "bring me back\n",
		"human-rm.txt": "human chose this\n",
		"mixed.txt":    "contested\n",
	})
	del := func(path, writer string) versionlog.Version {
		return versionlog.Version{Op: versionlog.OpDelete, Path: root + "/" + path, Writer: writer}
	}
	window := []versionlog.Version{
		del("agent-rm.txt", "agent"),
		del("human-rm.txt", "human"),
		ver("mixed.txt", "human"), // human wrote it...
		del("mixed.txt", "agent"), // ...then the agent deleted it
	}
	p := BuildPlan(baseline, window, root, nil)
	a := actions(p)
	if a["agent-rm.txt"] != Restore {
		t.Fatalf("agent-deleted file must be restorable, got %v", a)
	}
	if _, ok := a["human-rm.txt"]; ok {
		t.Fatalf("a human deletion is not the agent's change to undo, got %v", a)
	}
	if a["mixed.txt"] != Conflict {
		t.Fatalf("agent-deleted + human-written must be a conflict, got %v", a)
	}
}

// TestSaveBothPreservesBothVersions pins the conflict floor's escape hatch:
// save-both writes the checkpoint version ALONGSIDE the live file
// (<path><suffix>) without touching the live content; a conflict with no
// baseline version (agent-created, human-edited) is reported as skipped, and
// the live file is still never modified.
func TestSaveBothPreservesBothVersions(t *testing.T) {
	oc := mustObj(t)
	dir := t.TempDir()
	baseline := baselineWith(t, oc, map[string]string{"app.conf": "checkpoint version\n"})
	window := []versionlog.Version{
		{Op: versionlog.OpModify, Path: dir + "/app.conf", Writer: "agent"},
		{Op: versionlog.OpModify, Path: dir + "/app.conf", Writer: "human"},
		{Op: versionlog.OpModify, Path: dir + "/fresh.txt", Writer: "agent"},
		{Op: versionlog.OpModify, Path: dir + "/fresh.txt", Writer: "human"},
	}
	live := "live mixed content\n"
	if err := os.WriteFile(filepath.Join(dir, "app.conf"), []byte(live), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fresh.txt"), []byte(live), 0o644); err != nil {
		t.Fatal(err)
	}

	p := BuildPlan(baseline, window, dir, nil)
	a := actions(p)
	if a["app.conf"] != Conflict || a["fresh.txt"] != Conflict {
		t.Fatalf("both files must be conflicts, got %v", a)
	}

	saved, skipped, kept, errs := MaterializeConflicts(p, oc, ".checkpoint-7")
	if len(errs) != 0 || len(kept) != 0 {
		t.Fatalf("errors: %v kept: %v", errs, kept)
	}
	if len(saved) != 1 || saved[0] != filepath.Join(dir, "app.conf")+".checkpoint-7" {
		t.Fatalf("checkpoint version must land alongside, got %v", saved)
	}
	if len(skipped) != 1 || skipped[0] != "fresh.txt" {
		t.Fatalf("agent-created conflict has no baseline to save, got %v", skipped)
	}
	b, err := os.ReadFile(filepath.Join(dir, "app.conf") + ".checkpoint-7")
	if err != nil || string(b) != "checkpoint version\n" {
		t.Fatalf("sibling must hold the checkpoint version: %q err=%v", b, err)
	}
	for _, f := range []string{"app.conf", "fresh.txt"} {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		if string(b) != live {
			t.Fatalf("live file %s must never be modified by save-both, got %q", f, b)
		}
	}

	// An existing sibling is NEVER overwritten: the user may have merged their
	// resolution into it, and a rerun of undo must not destroy that merge.
	merged := "my hand-merged resolution\n"
	if err := os.WriteFile(filepath.Join(dir, "app.conf")+".checkpoint-7", []byte(merged), 0o644); err != nil {
		t.Fatal(err)
	}
	saved2, _, kept2, errs2 := MaterializeConflicts(p, oc, ".checkpoint-7")
	if len(errs2) != 0 || len(saved2) != 0 {
		t.Fatalf("rerun must not rewrite existing siblings: saved=%v errs=%v", saved2, errs2)
	}
	if len(kept2) != 1 {
		t.Fatalf("existing sibling must be reported kept, got %v", kept2)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "app.conf") + ".checkpoint-7")
	if string(b) != merged {
		t.Fatalf("hand-merged sibling must survive a save-both rerun, got %q", b)
	}
}

// TestReplayJournalFinishesInterruptedUndo: an interrupted undo is completed
// from its journal payloads alone, with no plan recomputation (the interrupted
// run's writes already shifted the provenance window).
func TestReplayJournalFinishesInterruptedUndo(t *testing.T) {
	oc := mustObj(t)
	dir := t.TempDir()
	ref, _, err := oc.Put([]byte("baseline bytes\n"))
	if err != nil {
		t.Fatal(err)
	}
	// The journaled plan: restore one file, delete one, save-both one sibling.
	if err := os.WriteFile(filepath.Join(dir, "gen.txt"), []byte("agent made this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	acts := []oplog.Action{
		{Do: "restore-file", Path: filepath.Join(dir, "a.txt"), Kind: store.KindFile, Ref: ref, Mode: 0o644},
		{Do: "delete", Path: filepath.Join(dir, "gen.txt")},
		{Do: "delete", Path: filepath.Join(dir, "already-gone.txt")}, // idempotent
		{Do: "save-both", Path: filepath.Join(dir, "c.txt.checkpoint-0"), Kind: store.KindFile, Ref: ref, Mode: 0o644},
	}
	res := ReplayJournal(acts, oc)
	if len(res.Errors) != 0 {
		t.Fatalf("replay errors: %v", res.Errors)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(b) != "baseline bytes\n" {
		t.Fatalf("restore-file must materialize from the journal payload: %q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gen.txt")); !os.IsNotExist(err) {
		t.Fatal("journaled delete must remove the file")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "c.txt.checkpoint-0")); err != nil || string(b) != "baseline bytes\n" {
		t.Fatalf("save-both must materialize the sibling: %q err=%v", b, err)
	}
	// Replay is idempotent: run it again, same end state, no errors.
	res2 := ReplayJournal(acts, oc)
	if len(res2.Errors) != 0 {
		t.Fatalf("replay must be idempotent: %v", res2.Errors)
	}
}

func TestApplyRevertsAgentPreservesHuman(t *testing.T) {
	oc := mustObj(t)
	work := t.TempDir()

	// Baseline (pre-turn): agent_edit.txt existed with "ORIGINAL".
	base := &store.Manifest{Root: work, Entries: map[string]store.Entry{}}
	origRef, _, _ := oc.Put([]byte("ORIGINAL\n"))
	base.Entries["agent_edit.txt"] = store.Entry{Kind: store.KindFile, Ref: origRef, Mode: 0o644}

	// Current on-disk state after the turn:
	writeFile(t, filepath.Join(work, "agent_edit.txt"), "AGENT CHANGED\n")  // agent-only modify
	writeFile(t, filepath.Join(work, "agent_new.txt"), "AGENT MADE THIS\n") // agent-created
	writeFile(t, filepath.Join(work, "human_edit.txt"), "HUMAN WORK\n")     // human-only, must survive
	writeFile(t, filepath.Join(work, "both.txt"), "AGENT THEN HUMAN\n")     // conflict, must survive

	window := []versionlog.Version{
		{Op: versionlog.OpModify, Path: filepath.Join(work, "agent_edit.txt"), Writer: "agent"},
		{Op: versionlog.OpModify, Path: filepath.Join(work, "agent_new.txt"), Writer: "agent"},
		{Op: versionlog.OpModify, Path: filepath.Join(work, "human_edit.txt"), Writer: "human"},
		{Op: versionlog.OpModify, Path: filepath.Join(work, "both.txt"), Writer: "agent"},
		{Op: versionlog.OpModify, Path: filepath.Join(work, "both.txt"), Writer: "human"},
	}

	plan := BuildPlan(base, window, work, nil)
	res := Apply(plan, oc, work)
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}

	// agent_edit reverted to baseline
	if got := readFile(t, filepath.Join(work, "agent_edit.txt")); got != "ORIGINAL\n" {
		t.Fatalf("agent_edit.txt should revert to ORIGINAL, got %q", got)
	}
	// agent_new removed
	if _, err := os.Stat(filepath.Join(work, "agent_new.txt")); !os.IsNotExist(err) {
		t.Fatal("agent_new.txt should be removed on undo")
	}
	// human_edit untouched
	if got := readFile(t, filepath.Join(work, "human_edit.txt")); got != "HUMAN WORK\n" {
		t.Fatalf("human_edit.txt must be preserved, got %q", got)
	}
	// both.txt untouched (conflict, skipped whole-file)
	if got := readFile(t, filepath.Join(work, "both.txt")); got != "AGENT THEN HUMAN\n" {
		t.Fatalf("conflicted both.txt must be left untouched, got %q", got)
	}
	// reported honestly
	sort.Strings(res.Conflicts)
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "both.txt" {
		t.Fatalf("both.txt should be reported as a conflict, got %v", res.Conflicts)
	}
}

func mustObj(t *testing.T) *objstore.Store {
	t.Helper()
	oc, err := objstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return oc
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSelfWritesDoNotBlockLaterUndo pins resumable selective undo:
// checkpoint's own restore writes are classified
// "checkpoint", not "human": counting them as human would conflict the very
// file a previous `undo --only` just reverted, so a turn could be addressed
// only once. A REAL human write after the undo must still conflict; that is
// the safety property this must not trade away.
func TestSelfWritesDoNotBlockLaterUndo(t *testing.T) {
	oc := mustObj(t)
	base := baselineWith(t, oc, map[string]string{"a.txt": "orig-a\n", "b.txt": "orig-b\n"})

	// The agent changed both; a previous `undo --only a.txt` then reverted
	// a.txt, and that revert was captured as checkpoint's own write.
	window := []versionlog.Version{
		ver("a.txt", "agent"),
		ver("b.txt", "agent"),
		ver("a.txt", "checkpoint"), // the earlier undo's restore
	}
	a := actions(BuildPlan(base, window, root, nil))
	if a["b.txt"] != Restore {
		t.Fatalf("the untouched agent change must still be restorable, got %v", a)
	}
	if a["a.txt"] != Restore {
		t.Fatalf("checkpoint's own revert must not conflict the file it reverted, got %v", a)
	}

	// Safety: a genuine human write after the undo DOES conflict.
	window = append(window, ver("a.txt", "human"))
	if got := actions(BuildPlan(base, window, root, nil))["a.txt"]; got != Conflict {
		t.Fatalf("a human edit after an undo must conflict (never be silently reverted), got %v", got)
	}

	// And a checkpoint write with no agent write at all is not a candidate:
	// restoring something checkpoint wrote is not undoing the agent.
	onlySelf := []versionlog.Version{ver("c.txt", "checkpoint")}
	if _, present := actions(BuildPlan(base, onlySelf, root, nil))["c.txt"]; present {
		t.Fatalf("a checkpoint-only write is nobody's change to undo")
	}
}

// TestConflictNamesWhoElseWrote pins that a conflict reports WHO else wrote the
// path. Both a human write and an unattributable one make the path untouchable,
// but only one of them justifies telling the user they changed it, and a
// recovery tool that overstates what it knows is the failure this project
// exists to avoid.
func TestConflictNamesWhoElseWrote(t *testing.T) {
	root := t.TempDir()
	base := &store.Manifest{Entries: map[string]store.Entry{
		"human.txt":   {Kind: store.KindFile, Ref: "r1"},
		"unknown.txt": {Kind: store.KindFile, Ref: "r2"},
		"both.txt":    {Kind: store.KindFile, Ref: "r3"},
	}}
	win := []versionlog.Version{
		{Path: filepath.Join(root, "human.txt"), Writer: "agent"},
		{Path: filepath.Join(root, "human.txt"), Writer: "human"},
		{Path: filepath.Join(root, "unknown.txt"), Writer: "agent"},
		{Path: filepath.Join(root, "unknown.txt"), Writer: "unknown"},
		{Path: filepath.Join(root, "both.txt"), Writer: "agent"},
		{Path: filepath.Join(root, "both.txt"), Writer: "human"},
		{Path: filepath.Join(root, "both.txt"), Writer: "unknown"},
	}
	want := map[string]Other{
		"human.txt": OtherHuman, "unknown.txt": OtherUnknown, "both.txt": OtherBoth,
	}
	plan := BuildPlan(base, win, root, nil)
	if len(plan.Entries) != 3 {
		t.Fatalf("want 3 planned paths, got %d", len(plan.Entries))
	}
	for _, e := range plan.Entries {
		if e.Action != Conflict {
			t.Fatalf("%s: want Conflict (an unattributed write must be as untouchable as a human one), got %s", e.Rel, e.Action)
		}
		if e.Other != want[e.Rel] {
			t.Fatalf("%s: want Other=%q, got %q", e.Rel, want[e.Rel], e.Other)
		}
	}
}
