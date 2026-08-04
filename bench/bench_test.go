// Tests that pin what the harness MEASURES, as opposed to what a summary of it
// might claim. Two things get overstated when a benchmark is quoted from
// memory: how many scenarios ran, and which numbers were actually timed. Both
// are asserted here so the report cannot drift away from its own description.

package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestScenarioAndRoundCounts pins the size of a default run. The harness
// defines four recovery scenarios and runs five rounds of each, which is 20
// scored rounds, not 50 scenarios.
func TestScenarioAndRoundCounts(t *testing.T) {
	if len(recoveryScenarios) != 4 {
		t.Errorf("recovery scenarios: %d (%v), want 4", len(recoveryScenarios), recoveryScenarios)
	}
	if len(gitShadowScenarios) != 3 {
		t.Errorf("git-shadow scenarios: %d (%v), want 3", len(gitShadowScenarios), gitShadowScenarios)
	}
	f := flag.Lookup("rounds")
	if f == nil {
		t.Fatal("no --rounds flag")
	}
	if f.DefValue != "5" {
		t.Errorf("default rounds is %q, want \"5\"", f.DefValue)
	}
	// The number a reader of the report is entitled to: distinct scenarios
	// times rounds. Anything larger has to come from raising one of the two.
	if got := len(recoveryScenarios) * 5; got != 20 {
		t.Errorf("a default run scores %d recovery rounds, want 20", got)
	}
}

// TestAggregateCarriesRollbackLatency is the regression pin for the metric the
// harness used to be missing. Recovery percentages, writer overhead, boundary
// latency and routine cut cost were all reported; rollback latency, the cost of
// the command a user actually waits on, was not measured anywhere. If it
// disappears from the aggregate again, this fails.
func TestAggregateCarriesRollbackLatency(t *testing.T) {
	r := &report{
		Rounds: 1,
		Undo: undoResult{
			TreeFiles: 500, ChangedFiles: 20, Samples: 9,
			MedianMS: 12.335, MinMS: 9.147, MaxMS: 21.964, StartupFloorMS: 2.217,
		},
	}
	agg := aggregate(r)
	for _, k := range []string{"undo_ms", "undo_samples", "undo_tree_files", "undo_changed_files"} {
		if _, ok := agg[k]; !ok {
			t.Fatalf("aggregate is missing %q; rollback latency is not being reported", k)
		}
	}
	if agg["undo_ms"] != 12.335 {
		t.Errorf("undo_ms = %v, want the measured median 12.335", agg["undo_ms"])
	}
	// The median must never travel without its denominator and its workload:
	// 12 ms means one thing for 20 reverted files and another for one.
	if agg["undo_samples"] != 9 || agg["undo_tree_files"] != 500 || agg["undo_changed_files"] != 20 {
		t.Errorf("workload context lost: samples=%v tree=%v changed=%v",
			agg["undo_samples"], agg["undo_tree_files"], agg["undo_changed_files"])
	}
}

// TestUndoSampleIsVerifiedNotAssumed covers the check that keeps a rollback
// timing honest: a run where the files did not actually come back is not a
// rollback measurement, however fast it was.
func TestUndoSampleIsVerifiedNotAssumed(t *testing.T) {
	ws := t.TempDir()
	seedWideTree(ws, 40)
	if !revertedAll(ws, 40, 20) {
		t.Fatal("a freshly seeded tree must read as reverted")
	}
	if err := os.WriteFile(wideFile(ws, 7), []byte("AGENT-EDIT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if revertedAll(ws, 40, 20) {
		t.Error("a file left at the agent's bytes must not count as reverted")
	}
	if err := os.Remove(wideFile(ws, 7)); err != nil {
		t.Fatal(err)
	}
	if revertedAll(ws, 40, 20) {
		t.Error("a missing file must not count as reverted")
	}
	// Files outside the changed prefix are not part of the check.
	if err := os.WriteFile(wideFile(ws, 30), []byte("unrelated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wideFile(ws, 7), []byte(seedBytes(7)), 0o644); err != nil {
		t.Fatal(err)
	}
	if !revertedAll(ws, 40, 20) {
		t.Error("restoring the seeded bytes must read as reverted")
	}
	if _, err := os.Stat(filepath.Join(ws, "pkg0")); err != nil {
		t.Errorf("seedWideTree must lay down its directories: %v", err)
	}
}

// TestMsOfKeepsSubMillisecondResolution guards the unit. Rollback lands in
// single-digit milliseconds on small turns, where truncating to whole
// milliseconds would throw away most of the signal and round in whichever
// direction happens to flatter.
func TestMsOfKeepsSubMillisecondResolution(t *testing.T) {
	if got := msOf(2217 * time.Microsecond); got != 2.217 {
		t.Errorf("msOf(2.217ms) = %v, want 2.217", got)
	}
	if got := msOf(1500 * time.Microsecond); got != 1.5 {
		t.Errorf("msOf(1.5ms) = %v, want 1.5", got)
	}
}
