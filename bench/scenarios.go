// The four recovery scenarios, each driven through the real binary and scored
// only from disk state afterwards.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// scnRmRfRestore: an agent turn writes, the whole workspace is deleted, and the
// latest checkpoint has to rebuild it byte-exact.
func scnRmRfRestore(base string, round int) scenarioResult {
	res := scenarioResult{Name: "rm_rf_restore", Round: round}
	e, err := newEnv(base, res.Name, round)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	seedTree(e.ws, round)
	if err := e.start(); err != nil {
		res.Error = err.Error()
		return res
	}
	defer e.stop()
	if _, err := e.cli("run", "--root", e.ws, "--store", e.store, "--",
		"bash", "-c", fmt.Sprintf("echo turn-%d > %s/turn.txt", round, e.ws)); err != nil {
		res.Error = "run: " + err.Error()
		return res
	}
	want, err := fingerprint(e.ws)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if err := os.RemoveAll(e.ws); err != nil {
		res.Error = err.Error()
		return res
	}
	if _, err := os.Stat(e.ws); err == nil {
		res.Error = "mutation not observed"
		return res
	}
	id := latestID(e.store)
	if out, err := e.cli("restore", "--store", e.store, fmt.Sprint(id), e.ws); err != nil {
		res.Error = "restore: " + err.Error() + ": " + out
		return res
	}
	got, err := fingerprint(e.ws)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Recovered, res.Detail = same(want, got)
	return res
}

// scnTransientSalvage: a file created and deleted between checkpoints, which no
// checkpoint ever held, has to come back byte-exact through `recover`.
func scnTransientSalvage(base string, round int) scenarioResult {
	res := scenarioResult{Name: "transient_salvage", Round: round}
	e, err := newEnv(base, res.Name, round)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	seedTree(e.ws, round)
	if err := e.start(); err != nil {
		res.Error = err.Error()
		return res
	}
	defer e.stop()
	content := fmt.Sprintf("transient r%d %d\n", round, time.Now().UnixNano())
	tr := filepath.Join(e.ws, "transient.txt")
	if err := os.WriteFile(tr, []byte(content), 0o644); err != nil {
		res.Error = err.Error()
		return res
	}
	time.Sleep(300 * time.Millisecond) // let capture drain the close-write
	if err := os.Remove(tr); err != nil {
		res.Error = err.Error()
		return res
	}
	outDir := filepath.Join(filepath.Dir(e.ws), "recovered")
	if out, err := e.cli("recover", "--store", e.store, "--to", outDir, e.ws); err != nil {
		res.Error = "recover: " + err.Error() + ": " + out
		return res
	}
	b, err := os.ReadFile(filepath.Join(outDir, "transient.txt"))
	if err != nil {
		res.Error = "transient not recovered"
		return res
	}
	res.Recovered = string(b) == content
	if !res.Recovered {
		res.Detail = "content mismatch"
	}
	return res
}

// scnHumanPreservation: the agent edits one file and the human edits another,
// then `undo` has to revert the agent's and leave the human's alone. This is
// the scenario the whole design exists for.
func scnHumanPreservation(base string, round int) scenarioResult {
	res := scenarioResult{Name: "human_preservation", Round: round}
	e, err := newEnv(base, res.Name, round)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	seedTree(e.ws, round)
	if err := e.start(); err != nil {
		res.Error = err.Error()
		return res
	}
	defer e.stop()
	agentTarget := filepath.Join(e.ws, "d0/f0.txt")
	humanTarget := filepath.Join(e.ws, "d1/f1.txt")
	origAgent, _ := os.ReadFile(agentTarget)
	humanContent := fmt.Sprintf("HUMAN r%d\n", round)
	if _, err := e.cli("run", "--root", e.ws, "--store", e.store, "--",
		"bash", "-c", fmt.Sprintf("echo AGENT-%d > %s; sleep 0.3", round, agentTarget)); err != nil {
		res.Error = "run: " + err.Error()
		return res
	}
	// The human edit comes from this process, which is never a descendant of the
	// wrapped command. It lands after the turn, so it sits in the next window as
	// a human write.
	if err := os.WriteFile(humanTarget, []byte(humanContent), 0o644); err != nil {
		res.Error = err.Error()
		return res
	}
	time.Sleep(300 * time.Millisecond)
	agentNow, _ := os.ReadFile(agentTarget)
	if string(agentNow) == string(origAgent) {
		res.Error = "agent mutation not observed"
		return res
	}
	if out, err := e.cli("undo", "--root", e.ws, "--store", e.store); err != nil {
		res.Error = "undo: " + err.Error() + ": " + out
		return res
	}
	agentAfter, _ := os.ReadFile(agentTarget)
	humanAfter, _ := os.ReadFile(humanTarget)
	switch {
	case string(agentAfter) != string(origAgent):
		res.Detail = "agent edit not reverted"
	case string(humanAfter) != humanContent:
		res.Detail = "HUMAN EDIT LOST"
	default:
		res.Recovered = true
	}
	return res
}

// scnAgentDeleteUndo: the agent deletes a pre-existing file and `undo` has to
// bring it back. This one needs delete provenance, which comes from the dirent
// change feed. Where there is no feed the scenario cannot run at all: it scores
// zero and says why in its detail, so read feed_active before reading this
// column as a result.
func scnAgentDeleteUndo(base string, round int, feedActive bool) scenarioResult {
	res := scenarioResult{Name: "agent_delete_undo", Round: round}
	if !feedActive {
		res.Detail = "change feed unavailable on this filesystem (delete provenance off)"
		return res
	}
	e, err := newEnv(base, res.Name, round)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	seedTree(e.ws, round)
	victim := filepath.Join(e.ws, "d0/f0.txt")
	orig, _ := os.ReadFile(victim)
	if err := e.start(); err != nil {
		res.Error = err.Error()
		return res
	}
	defer e.stop()
	if _, err := e.cli("run", "--root", e.ws, "--store", e.store, "--",
		"bash", "-c", fmt.Sprintf("rm %s", victim)); err != nil {
		res.Error = "run: " + err.Error()
		return res
	}
	if _, err := os.Stat(victim); err == nil {
		res.Error = "mutation not observed"
		return res
	}
	if out, err := e.cli("undo", "--root", e.ws, "--store", e.store); err != nil {
		res.Error = "undo: " + err.Error() + ": " + out
		return res
	}
	b, err := os.ReadFile(victim)
	res.Recovered = err == nil && string(b) == string(orig)
	if !res.Recovered {
		res.Detail = "agent-deleted file not restored"
	}
	return res
}
