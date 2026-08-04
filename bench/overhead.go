// Overhead measurement: what wrapping a command under `checkpoint run` costs,
// on a write-churn workload and again on a realistic compile workload.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// measureOverhead times a burst of 2000 small writes, wrapped and unwrapped,
// and then the cost of a routine checkpoint on a warm daemon.
//
// The churn script times ITSELF into a nanosecond file, which isolates the
// writer-visible cost from the trailing boundary work `run` does after the
// command exits (drain, settle, cut). Wall-clocking the wrapper on a burst
// workload measures the settle ceiling instead of the writer path, which is a
// mistake worth not repeating.
func measureOverhead(base string, feedActive bool) overheadResult {
	const files = 2000
	const runs = 3
	script := func(ws, nsFile string) string {
		return fmt.Sprintf("s=$(date +%%s%%N); for i in $(seq 1 %d); do echo data-$i > %s/f$i.txt; done; e=$(date +%%s%%N); echo $((e-s)) > %s", files, ws, nsFile)
	}

	var un []time.Duration
	for i := 0; i < runs; i++ {
		dir := filepath.Join(base, fmt.Sprintf("ovh-un-%d", i))
		os.RemoveAll(dir)
		os.MkdirAll(dir, 0o755)
		ns := filepath.Join(base, fmt.Sprintf("ovh-un-%d.ns", i))
		exec.Command("bash", "-c", script(dir, ns)).Run()
		un = append(un, readNSFile(ns))
	}

	var wr, boundary []time.Duration
	for i := 0; i < runs; i++ {
		e, err := newEnv(base, "ovh-wr", i)
		if err != nil {
			continue
		}
		if err := e.start(); err != nil {
			e.stop() // a half-started daemon must not bias later runs
			continue
		}
		ns := filepath.Join(filepath.Dir(e.ws), "churn.ns")
		start := time.Now()
		exec.Command(bin, "run", "--root", e.ws, "--store", e.store, "--",
			"bash", "-c", script(e.ws, ns)).Run()
		total := time.Since(start)
		churn := readNSFile(ns)
		wr = append(wr, churn)
		if total > churn {
			boundary = append(boundary, total-churn)
		}
		e.stop()
	}
	o := overheadResult{Files: files, MedianOfRuns: runs, FeedActiveEnv: feedActive}
	if len(un) > 0 && len(wr) > 0 {
		u, w := medianDur(un), medianDur(wr)
		o.UnwrappedMS, o.WrappedMS = u.Milliseconds(), w.Milliseconds()
		if u > 0 {
			o.OverheadPct = 100 * float64(w-u) / float64(u)
		}
		o.UsPerWrite = float64((w - u).Microseconds()) / float64(files)
	}
	if len(boundary) > 0 {
		o.BoundaryMS = medianDur(boundary).Milliseconds()
	}
	o.RoutineCutMS = measureRoutineCut(base)
	return o
}

// measureRoutineCut times a routine checkpoint: a warm daemon, five changed
// files, median of three saves. The fixed settle-quiet window is subtracted so
// the number reports the feed and scan work rather than a constant wait.
func measureRoutineCut(base string) int64 {
	e, err := newEnv(base, "routine", 0)
	if err != nil {
		return 0
	}
	if err := e.start(); err != nil {
		return 0
	}
	defer e.stop()
	seedTree(e.ws, 0)
	e.cli("save", "--root", e.ws, "--store", e.store, "--source", "warmup")
	var cuts []time.Duration
	for i := 0; i < 3; i++ {
		for j := 0; j < 5; j++ {
			os.WriteFile(filepath.Join(e.ws, fmt.Sprintf("routine-%d-%d.txt", i, j)),
				[]byte(fmt.Sprintf("edit %d.%d\n", i, j)), 0o644)
		}
		time.Sleep(80 * time.Millisecond) // let capture drain the burst
		start := time.Now()
		e.cli("save", "--root", e.ws, "--store", e.store, "--source", "routine")
		total := time.Since(start)
		const settleQuiet = 250 * time.Millisecond // daemon constant
		if total > settleQuiet {
			cuts = append(cuts, total-settleQuiet)
		}
	}
	if len(cuts) == 0 {
		return 0
	}
	return medianDur(cuts).Milliseconds()
}

// measureUndo times ROLLBACK: `checkpoint undo` end to end, from fork/exec of
// the binary until it exits, on a tree of treeFiles files where one agent turn
// rewrote changedFiles of them.
//
// What is timed is the whole user-visible command: process start, store open,
// plan build, the pre-undo checkpoint it cuts, and every file it writes back.
// Nothing is subtracted. Two things make that number readable rather than
// flattering, and both are reported next to it:
//
//   - the workload dimensions, because rollback latency scales with the turn.
//     A median measured against one reverted file is a measurement of exec
//     overhead wearing a rollback costume;
//   - startup_floor_ms, the same binary run as `checkpoint version`, which
//     does no store work at all. Any undo median near the floor is dominated
//     by process start.
//
// A sample counts only if undo exited 0 AND every changed file is back to its
// seeded bytes. Failed samples are dropped and the ACHIEVED count is reported,
// so a median is never labelled with a denominator it did not have.
func measureUndo(base string, samples, treeFiles, changedFiles int) undoResult {
	res := undoResult{TreeFiles: treeFiles, ChangedFiles: changedFiles}

	// The floor: exec, flag parse, exit. The first run pays cold page cache, so
	// it is discarded rather than averaged in.
	var floors []time.Duration
	for i := 0; i < 6; i++ {
		start := time.Now()
		exec.Command(bin, "version").Run()
		if i > 0 {
			floors = append(floors, time.Since(start))
		}
	}
	if len(floors) > 0 {
		res.StartupFloorMS = msOf(medianDur(floors))
	}

	var undos []time.Duration
	for i := 0; i < samples; i++ {
		e, err := newEnv(base, "undo-lat", i)
		if err != nil {
			continue
		}
		seedWideTree(e.ws, treeFiles)
		if err := e.start(); err != nil {
			e.stop() // a half-started daemon must not bias later samples
			continue
		}
		if _, err := e.cli("run", "--root", e.ws, "--store", e.store, "--",
			"bash", "-c", turnScript(e.ws, changedFiles)); err != nil {
			e.stop()
			continue
		}
		start := time.Now()
		out, err := e.cli("undo", "--root", e.ws, "--store", e.store)
		elapsed := time.Since(start)
		e.stop()
		if err != nil {
			res.Note = "undo failed: " + firstLine(out)
			continue
		}
		if !revertedAll(e.ws, treeFiles, changedFiles) {
			res.Note = "undo did not restore the seeded bytes"
			continue
		}
		undos = append(undos, elapsed)
	}
	res.Samples = len(undos)
	if len(undos) == 0 {
		if res.Note == "" {
			res.Note = "no sample completed"
		}
		return res
	}
	sort.Slice(undos, func(i, j int) bool { return undos[i] < undos[j] })
	res.MedianMS = msOf(medianDur(undos))
	res.MinMS = msOf(undos[0])
	res.MaxMS = msOf(undos[len(undos)-1])
	return res
}

// seedWideTree lays down n files spread over 20 directories: a tree big enough
// that walking it is not free, which is the point of measuring against it.
func seedWideTree(ws string, n int) {
	for i := 0; i < 20; i++ {
		os.MkdirAll(filepath.Join(ws, fmt.Sprintf("pkg%d", i)), 0o755)
	}
	for i := 0; i < n; i++ {
		os.WriteFile(wideFile(ws, i), []byte(seedBytes(i)), 0o644)
	}
}

func wideFile(ws string, i int) string {
	return filepath.Join(ws, fmt.Sprintf("pkg%d", i%20), fmt.Sprintf("f%d.txt", i))
}

func seedBytes(i int) string { return fmt.Sprintf("seed %d\n", i) }

// turnScript is the agent turn: rewrite the first n files of the tree.
func turnScript(ws string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "echo AGENT-EDIT > %s\n", wideFile(ws, i))
	}
	return b.String()
}

// revertedAll reports whether every file the turn rewrote is back to its
// seeded bytes. A sample that did not actually roll back is not a rollback
// timing.
func revertedAll(ws string, treeFiles, changedFiles int) bool {
	for i := 0; i < changedFiles && i < treeFiles; i++ {
		b, err := os.ReadFile(wideFile(ws, i))
		if err != nil || string(b) != seedBytes(i) {
			return false
		}
	}
	return true
}

// msOf renders a duration in milliseconds to microsecond resolution. Rollback
// lands in single-digit milliseconds on small turns, where whole milliseconds
// throw away most of the signal.
func msOf(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// measureRealistic times overhead against a realistic agent task. The churn
// measurement above resolves microseconds per write well, but its percentage
// has no resolving power at all: both sides are noise at millisecond scale.
//
// The workload is a small C project, one header plus n translation units of a
// few functions each, compiled with `gcc -c` per unit and then linked. As in
// measureOverhead the script times itself, so the number is the writer-visible
// cost. The unit count is scaled at runtime until one unwrapped build takes at
// least 3 s, which is what gives a percentage comparison any meaning, and the
// achieved duration is recorded alongside it. Both sides are a median of three.
// Without gcc the measurement is skipped cleanly, leaving overhead_pct null and
// a note saying why.
func measureRealistic(base string) realisticResult {
	if _, err := exec.LookPath("gcc"); err != nil {
		return realisticResult{Skipped: true, Note: "gcc unavailable"}
	}
	const runs = 3
	const minUnwrapped = 3 * time.Second
	script := func(ws, nsFile string) string {
		return fmt.Sprintf(`s=$(date +%%s%%N); cd %s; for f in *.c; do gcc -c "$f" -o "${f%%.c}.o"; done; gcc -o app *.o; e=$(date +%%s%%N); echo $((e-s)) > %s`,
			ws, nsFile)
	}
	runUnwrapped := func(name string, tus int) time.Duration {
		dir := filepath.Join(base, name)
		os.RemoveAll(dir)
		genCProject(dir, tus)
		ns := dir + ".ns"
		os.Remove(ns)
		exec.Command("bash", "-c", script(dir, ns)).Run()
		return readNSFile(ns)
	}

	// Calibrate: scale the unit count until one unwrapped build takes 3 s or more.
	tus := 40
	var calib time.Duration
	for attempt := 0; attempt < 5; attempt++ {
		calib = runUnwrapped("real-cal", tus)
		if calib == 0 || calib >= minUnwrapped || tus >= 4000 {
			break
		}
		tus = int(float64(tus)*float64(minUnwrapped)/float64(calib)*1.15) + 1
		if tus > 4000 {
			tus = 4000
		}
	}
	if calib == 0 {
		return realisticResult{Skipped: true, Note: "workload failed to produce a timing"}
	}

	var un []time.Duration
	for i := 0; i < runs; i++ {
		if d := runUnwrapped(fmt.Sprintf("real-un-%d", i), tus); d > 0 {
			un = append(un, d)
		}
	}
	var wr []time.Duration
	for i := 0; i < runs; i++ {
		e, err := newEnv(base, "real-wr", i)
		if err != nil {
			continue
		}
		genCProject(e.ws, tus)
		if err := e.start(); err != nil {
			e.stop() // a half-started daemon must not bias later runs
			continue
		}
		ns := filepath.Join(filepath.Dir(e.ws), "build.ns")
		exec.Command(bin, "run", "--root", e.ws, "--store", e.store, "--",
			"bash", "-c", script(e.ws, ns)).Run()
		if d := readNSFile(ns); d > 0 {
			wr = append(wr, d)
		}
		e.stop()
	}

	// Report the ACHIEVED sample count, not the intended one. A single surviving
	// sample must not be labelled a median of three.
	res := realisticResult{TranslationUnits: tus, MedianOfRuns: min(len(un), len(wr))}
	if len(un) == 0 || len(wr) == 0 {
		res.Skipped = true
		res.Note = "measurement runs failed"
		return res
	}
	u, w := medianDur(un), medianDur(wr)
	res.UnwrappedMS, res.WrappedMS = u.Milliseconds(), w.Milliseconds()
	res.UnwrappedSecs = u.Seconds()
	p := 100 * float64(w-u) / float64(u)
	res.OverheadPct = &p
	return res
}

// genCProject writes a shared header, n small translation units of a few
// functions each, and a main.c that links them all.
func genCProject(dir string, n int) {
	os.MkdirAll(dir, 0o755)
	var h strings.Builder
	h.WriteString("#pragma once\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&h, "int fn_%d(int);\n", i)
	}
	os.WriteFile(filepath.Join(dir, "shared.h"), []byte(h.String()), 0o644)
	for i := 0; i < n; i++ {
		src := fmt.Sprintf(`#include "shared.h"
static int helper_a_%d(int x) { return x * %d + 1; }
static int helper_b_%d(int x) {
	int s = 0;
	for (int i = 0; i < (x %% 17) + 3; i++) s += helper_a_%d(i + %d);
	return s;
}
int fn_%d(int x) { return helper_a_%d(x) ^ helper_b_%d(x + 1); }
`, i, i+3, i, i, i, i, i, i)
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("tu_%04d.c", i)), []byte(src), 0o644)
	}
	var m strings.Builder
	m.WriteString("#include \"shared.h\"\n#include <stdio.h>\nint main(void) {\n\tint s = 0;\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&m, "\ts += fn_%d(%d);\n", i, i)
	}
	m.WriteString("\tprintf(\"%d\\n\", s);\n\treturn 0;\n}\n")
	os.WriteFile(filepath.Join(dir, "main.c"), []byte(m.String()), 0o644)
}

// readNSFile reads a duration a timed script wrote out in nanoseconds. A
// missing or unparseable file reads as zero, which the callers treat as a
// failed sample rather than an instant one.
func readNSFile(p string) time.Duration {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	var ns int64
	fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &ns)
	return time.Duration(ns)
}

func medianDur(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}
