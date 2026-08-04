// Command bench drives the real checkpoint binary (daemon, run, undo, restore,
// recover) through scripted destructive scenarios, scores each one from real
// disk state, and writes a JSON report.
//
// Three things are measured:
//
//   - Recovery. Four scenarios, each scored from what is on disk afterwards.
//   - A git-shadow baseline. The same scenarios against the strongest simple
//     git strategy, a force-add commit per turn, so checkpoint's numbers have
//     something to be read against. The shadow git dir lives outside the
//     workspace; an in-workspace .git would make the rm-rf column an artifact
//     of where .git happens to live.
//   - Overhead. Wrapped versus unwrapped, on a write-churn workload and again
//     on a realistic compile workload.
//
// Three rules keep the harness from flattering the product:
//
//   - the expected post-state is COMPUTED from the pre-run bytes, never
//     authored, so the harness cannot assert what it hopes to see;
//   - a scenario scores recovered=true only if the destructive mutation was
//     first OBSERVED on disk, so a wrapper that quietly swallows the workload
//     scores zero rather than full marks;
//   - an errored round scores recovered=false, and the JSON report is written
//     either way.
//
// Thresholds are deliberately not enforced here. This harness reports;
// bench/accept.sh scores the report against thresholds a human ratified.
//
// Usage:
//
//	go run ./bench --bin bin/checkpoint --base DIR --rounds 5 --out results/bench.json
//
// --base should sit on a filesystem that carries the dirent change feed (ext4,
// xfs, btrfs) for full capability. On a filesystem without one the harness
// records that fact rather than scoring the delete scenario zero.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// --- report schema -----------------------------------------------------------

type scenarioResult struct {
	Name      string `json:"name"`
	Round     int    `json:"round"`
	Recovered bool   `json:"recovered"`
	Detail    string `json:"detail,omitempty"`
	Error     string `json:"error,omitempty"`
}

type overheadResult struct {
	Files int `json:"files"`
	// Writer-visible cost: the churn script times ITSELF, wrapped vs unwrapped.
	// Capture is off the writer path, so this is what the working agent feels.
	UnwrappedMS int64   `json:"unwrapped_ms"`
	WrappedMS   int64   `json:"wrapped_ms"`
	OverheadPct float64 `json:"overhead_pct"`
	UsPerWrite  float64 `json:"us_per_write"`
	// Boundary latency: command exit until `run` returns, covering the backlog
	// drain, the settle window and the checkpoint cut. It is not writer-visible;
	// it is the turn's trailing cost.
	BoundaryMS    int64 `json:"boundary_ms"`
	MedianOfRuns  int   `json:"median_of_runs"`
	FeedActiveEnv bool  `json:"feed_active_env"`
	// Routine checkpoint cost: a handful of changed files on a warm daemon,
	// measured as the save round-trip minus the fixed 250 ms settle-quiet
	// window. Recovery point objective splits into the settle window plus
	// feed/scan time, and this is the feed/scan half.
	RoutineCutMS int64 `json:"routine_cut_ms"`
}

// undoResult is rollback latency: `checkpoint undo` timed end to end, from exec
// to exit, against a stated workload. The workload dimensions travel with the
// number because rollback cost scales with the turn being reverted, and a
// median quoted without them says nothing.
type undoResult struct {
	TreeFiles    int `json:"tree_files"`    // files in the workspace
	ChangedFiles int `json:"changed_files"` // files the agent turn rewrote
	// Samples is the ACHIEVED count: samples where undo exited 0 and every
	// changed file was verified back to its seeded bytes.
	Samples  int     `json:"samples"`
	MedianMS float64 `json:"median_ms"`
	MinMS    float64 `json:"min_ms"`
	MaxMS    float64 `json:"max_ms"`
	// StartupFloorMS is the same binary invoked as `checkpoint version`, which
	// touches no store. It is the exec cost every undo also pays, and it is what
	// a suspiciously small median is usually made of.
	StartupFloorMS float64 `json:"startup_floor_ms"`
	Note           string  `json:"note,omitempty"`
}

// realisticResult is overhead against a realistic task. The churn number has no
// resolving power against a percentage bar (both sides are noise at millisecond
// scale), so this measures a compile workload instead.
type realisticResult struct {
	Skipped bool   `json:"skipped"`
	Note    string `json:"note,omitempty"` // e.g. "gcc unavailable"
	// TranslationUnits is the achieved TU count after runtime scaling, which
	// pushes the unwrapped workload past 3 s so a percentage means something.
	TranslationUnits int   `json:"translation_units,omitempty"`
	UnwrappedMS      int64 `json:"unwrapped_ms,omitempty"`
	WrappedMS        int64 `json:"wrapped_ms,omitempty"`
	// UnwrappedSecs records the achieved unwrapped duration (target: 3 s or more).
	UnwrappedSecs float64 `json:"unwrapped_secs,omitempty"`
	// OverheadPct is null when the measurement was skipped.
	OverheadPct  *float64 `json:"overhead_pct"`
	MedianOfRuns int      `json:"median_of_runs,omitempty"`
}

type report struct {
	Bin        string           `json:"bin"`
	Base       string           `json:"base"`
	Rounds     int              `json:"rounds"`
	FeedActive bool             `json:"feed_active"`
	Scenarios  []scenarioResult `json:"scenarios"`
	GitShadow  []scenarioResult `json:"git_shadow"`
	Overhead   overheadResult   `json:"overhead"`
	Realistic  realisticResult  `json:"overhead_realistic"`
	Undo       undoResult       `json:"undo_latency"`
	Aggregate  map[string]any   `json:"aggregate"`
}

// --- entry point -------------------------------------------------------------

// recoveryScenarios names every recovery scenario this harness defines, in run
// order, and gitShadowScenarios the baseline ones. A run scores
// len(recoveryScenarios) x rounds rounds and no more: at the default five
// rounds that is 20 scored recovery rounds plus 15 baseline rounds. The counts
// live here so they can be asserted rather than remembered.
var (
	recoveryScenarios  = []string{"rm_rf_restore", "transient_salvage", "human_preservation", "agent_delete_undo"}
	gitShadowScenarios = []string{"git_rm_rf_restore", "git_transient", "git_human_preservation"}
)

var (
	bin    string
	baseFl = flag.String("base", "", "base dir for scenario sandboxes (ideally on ext4)")
	binFl  = flag.String("bin", "bin/checkpoint", "checkpoint binary to drive")
	rounds = flag.Int("rounds", 5, "rounds per scenario")
	outFl  = flag.String("out", "results/bench.json", "output JSON")
)

func main() {
	flag.Parse()
	var err error
	bin, err = filepath.Abs(*binFl)
	if err != nil || !executable(bin) {
		fatal("bin %s not executable (run make build first): %v", *binFl, err)
	}
	base := *baseFl
	if base == "" {
		base, _ = os.MkdirTemp("", "checkpoint-bench-")
	}
	base, _ = filepath.Abs(base)

	r := &report{Bin: bin, Base: base, Rounds: *rounds}

	// Feed availability on this filesystem, recorded rather than assumed.
	r.FeedActive = probeFeed(base)

	for round := 0; round < *rounds; round++ {
		r.Scenarios = append(r.Scenarios,
			scnRmRfRestore(base, round),
			scnTransientSalvage(base, round),
			scnHumanPreservation(base, round),
			scnAgentDeleteUndo(base, round, r.FeedActive),
		)
		r.GitShadow = append(r.GitShadow,
			gitRmRfRestore(base, round),
			gitTransient(base, round),
			gitHumanPreservation(base, round),
		)
	}
	// The run loop and the scenario list must not drift apart: the denominator
	// the report is read with comes from the list, so a scenario added to one
	// and not the other would quietly change what a percentage is out of.
	if len(r.Scenarios) != len(recoveryScenarios)*(*rounds) ||
		len(r.GitShadow) != len(gitShadowScenarios)*(*rounds) {
		fatal("scenario list and run loop disagree: %d/%d rounds recorded for %d/%d scenarios",
			len(r.Scenarios), len(r.GitShadow), len(recoveryScenarios), len(gitShadowScenarios))
	}
	r.Overhead = measureOverhead(base, r.FeedActive)
	r.Realistic = measureRealistic(base)
	// Rollback latency, on a tree big enough that the walk is not free: 500
	// files, of which one agent turn rewrote 20.
	r.Undo = measureUndo(base, 9, 500, 20)

	agg := aggregate(r)
	r.Aggregate = agg

	os.MkdirAll(filepath.Dir(*outFl), 0o755)
	b, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(*outFl, b, 0o644); err != nil {
		fatal("write %s: %v", *outFl, err)
	}
	fmt.Printf("bench: %d rounds -> %s\n", *rounds, *outFl)
	var keys []string
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-28s %v\n", k, agg[k])
	}
}

// aggregate folds a finished report into the flat summary accept.sh reads.
func aggregate(r *report) map[string]any {
	agg := map[string]any{}
	for _, name := range recoveryScenarios {
		agg[name+"_pct"] = pct(r.Scenarios, name)
	}
	for _, name := range gitShadowScenarios {
		agg[name+"_pct"] = pct(r.GitShadow, name)
	}
	agg["overhead_pct"] = r.Overhead.OverheadPct
	if r.Realistic.OverheadPct != nil {
		agg["overhead_realistic_pct"] = *r.Realistic.OverheadPct
	} else {
		agg["overhead_realistic_pct"] = nil // skipped (no gcc, say) becomes JSON null
	}
	agg["realistic_unwrapped_ms"] = r.Realistic.UnwrappedMS
	agg["boundary_ms"] = r.Overhead.BoundaryMS
	agg["routine_cut_ms"] = r.Overhead.RoutineCutMS
	// Rollback: the median travels with its denominator and its workload, so
	// the aggregate alone cannot be quoted as a bare headline number.
	agg["undo_ms"] = r.Undo.MedianMS
	agg["undo_samples"] = r.Undo.Samples
	agg["undo_tree_files"] = r.Undo.TreeFiles
	agg["undo_changed_files"] = r.Undo.ChangedFiles
	agg["us_per_write"] = r.Overhead.UsPerWrite
	agg["feed_active"] = r.FeedActive
	return agg
}

// pct is the share of rounds of one scenario that scored recovered.
func pct(rs []scenarioResult, name string) float64 {
	n, ok := 0, 0
	for _, r := range rs {
		if r.Name != name {
			continue
		}
		n++
		if r.Recovered {
			ok++
		}
	}
	if n == 0 {
		return 0
	}
	return 100 * float64(ok) / float64(n)
}

func executable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode()&0o111 != 0
}

func fatal(f string, args ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	os.Exit(1)
}
