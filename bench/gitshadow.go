// The git-shadow baseline: the same recovery scenarios run against the
// strongest simple git strategy, a force-add commit at every turn boundary.
// It exists so checkpoint's recovery numbers are read against something rather
// than admired on their own.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// shadowDir is the external git dir for a scenario env. It sits beside the
// workspace rather than inside it, so destroying the workspace can never take
// the shadow history with it. A .git inside the workspace would make the rm-rf
// column partly an artifact of where .git lives.
func shadowDir(ws string) string {
	return filepath.Join(filepath.Dir(ws), "gitshadow")
}

// gitShadow runs git against the external git dir with the workspace as the
// work tree, so no .git (directory or file) ever exists inside the workspace.
// Global and system git config are masked: the invoking user's settings for
// autocrlf, hooks path or excludes must not be able to flip a baseline column.
func gitShadow(ws string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_DIR="+shadowDir(ws), "GIT_WORK_TREE="+ws,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// gitTurn commits the whole workspace, force-adding everything.
func gitTurn(ws string) error {
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A", "-f"},
		{"-c", "user.email=b@b", "-c", "user.name=b", "commit", "-q", "-m", "turn", "--allow-empty"},
	} {
		if out, err := gitShadow(ws, args...); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
	}
	return nil
}

func gitRmRfRestore(base string, round int) scenarioResult {
	res := scenarioResult{Name: "git_rm_rf_restore", Round: round}
	e, err := newEnv(base, res.Name, round)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	seedTree(e.ws, round)
	if err := gitTurn(e.ws); err != nil {
		res.Error = err.Error()
		return res
	}
	want, _ := fingerprint(e.ws)
	os.RemoveAll(e.ws) // the shadow git dir lives outside the workspace and survives
	if _, err := os.Stat(e.ws); err == nil {
		res.Error = "mutation not observed"
		return res
	}
	if err := os.MkdirAll(e.ws, 0o755); err != nil {
		res.Error = err.Error()
		return res
	}
	out, err := gitShadow(e.ws, "checkout", "-q", "--", ".")
	if err == nil {
		got, _ := fingerprint(e.ws)
		ok, d := same(want, got)
		res.Recovered, res.Detail = ok, d
	} else {
		res.Detail = "git checkout failed: " + strings.TrimSpace(out)
	}
	return res
}

func gitTransient(base string, round int) scenarioResult {
	res := scenarioResult{Name: "git_transient", Round: round}
	e, err := newEnv(base, res.Name, round)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	seedTree(e.ws, round)
	if err := gitTurn(e.ws); err != nil {
		res.Error = err.Error()
		return res
	}
	tr := filepath.Join(e.ws, "transient.txt")
	os.WriteFile(tr, []byte("gone before any commit\n"), 0o644)
	os.Remove(tr)
	if err := gitTurn(e.ws); err != nil { // the next turn's commit
		res.Error = err.Error()
		return res
	}
	out, _ := gitShadow(e.ws, "log", "--all", "--diff-filter=A", "--name-only")
	res.Recovered = strings.Contains(out, "transient.txt")
	if !res.Recovered {
		res.Detail = "no commit ever saw the transient"
	}
	return res
}

func gitHumanPreservation(base string, round int) scenarioResult {
	res := scenarioResult{Name: "git_human_preservation", Round: round}
	e, err := newEnv(base, res.Name, round)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	seedTree(e.ws, round)
	if err := gitTurn(e.ws); err != nil {
		res.Error = err.Error()
		return res
	}
	agentTarget := filepath.Join(e.ws, "d0/f0.txt")
	humanTarget := filepath.Join(e.ws, "d1/f1.txt")
	origAgent, _ := os.ReadFile(agentTarget)
	humanContent := fmt.Sprintf("HUMAN r%d\n", round)
	os.WriteFile(agentTarget, []byte("AGENT\n"), 0o644) // the agent's edit
	os.WriteFile(humanTarget, []byte(humanContent), 0o644)
	// Undoing "the agent's changes" without provenance means resetting the tree.
	if out, err := gitShadow(e.ws, "checkout", "-q", "--", "."); err != nil {
		res.Error = out
		return res
	}
	agentAfter, _ := os.ReadFile(agentTarget)
	humanAfter, _ := os.ReadFile(humanTarget)
	res.Recovered = string(agentAfter) == string(origAgent) && string(humanAfter) == humanContent
	if !res.Recovered {
		res.Detail = "git has no provenance: reverting the agent reverts the human too"
	}
	return res
}
