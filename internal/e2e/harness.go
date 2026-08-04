//go:build linux

// Package e2e drives the real checkpoint BINARY against a real daemon on a real
// filesystem. It exists because the package suites, thorough as they are, test
// libraries, while every CLI guarantee (what `status` says, that `undo` cuts a
// pre-undo checkpoint, that `--json` fields are never null) is a promise made to
// the user by the shipped command. Those promises are only verified by running
// that command, so they are verified here rather than by hand at a terminal.
//
// Tests here need CAP_SYS_ADMIN for fanotify and skip honestly without it.
package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// binary builds checkpoint once per test run and returns its path. Building
// (rather than trusting bin/) is deliberate: a test that exercises yesterday's
// binary is worse than no test, because it reports already-fixed defects as
// live and live defects as fixed.
func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "ckbin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "checkpoint")
		cmd := exec.Command("go", "build", "-o", binPath, "github.com/manyapn/checkpoint-public/cmd/checkpoint")
		cmd.Dir = repoRoot()
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = &buildFailure{out: string(out), err: err}
		}
	})
	if buildErr != nil {
		t.Fatalf("building checkpoint: %v", buildErr)
	}
	return binPath
}

type buildFailure struct {
	out string
	err error
}

func (b *buildFailure) Error() string { return b.err.Error() + "\n" + b.out }

func repoRoot() string {
	dir, _ := os.Getwd() // .../internal/e2e
	return filepath.Clean(filepath.Join(dir, "..", ".."))
}

// env is one disposable project: a workspace plus an out-of-tree store. Paths
// live directly under /tmp and stay SHORT, because the daemon's socket path is
// derived from the store dir, and a long t.TempDir() name overruns sun_path's
// 108-byte limit with a confusing "invalid argument".
type env struct {
	t     *testing.T
	dir   string
	WS    string
	Store string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ck")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, WS: filepath.Join(dir, "ws"), Store: filepath.Join(dir, "st")}
	if err := os.MkdirAll(e.WS, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.StopProtect() // never leave a daemon behind holding a temp dir
		os.RemoveAll(dir)
	})
	return e
}

// Run executes the CLI and returns combined output plus the exit error.
func (e *env) Run(args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command(binary(e.t), args...)
	cmd.Stdin = strings.NewReader("") // never block on a confirmation prompt
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// MustRun fails the test if the command does not succeed.
func (e *env) MustRun(args ...string) string {
	e.t.Helper()
	out, err := e.Run(args...)
	if err != nil {
		e.t.Fatalf("checkpoint %s\nexit: %v\noutput:\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// MustFail fails the test if the command SUCCEEDS, returning its output so the
// caller can assert on the reason (a refusal must explain itself).
func (e *env) MustFail(args ...string) string {
	e.t.Helper()
	out, err := e.Run(args...)
	if err == nil {
		e.t.Fatalf("expected `checkpoint %s` to fail; it succeeded:\n%s", strings.Join(args, " "), out)
	}
	return out
}

// Protect starts standing protection and waits for the setup checkpoint. It
// SKIPS the test when fanotify is unavailable rather than reporting a pass.
func (e *env) Protect(extraArgs ...string) {
	e.t.Helper()
	args := append([]string{"protect", "--store", e.Store}, extraArgs...)
	args = append(args, e.WS)
	out, err := e.Run(args...)
	if err != nil {
		if strings.Contains(out, "CAP_SYS_ADMIN") || strings.Contains(out, "operation not permitted") {
			e.t.Skipf("fanotify unavailable in this environment:\n%s", out)
		}
		e.t.Fatalf("protect failed:\n%s", out)
	}
	e.WaitCheckpoints(1, 10*time.Second) // the automatic setup checkpoint
}

func (e *env) StopProtect() {
	if _, err := os.Stat(filepath.Join(e.Store, "daemon.pid")); err != nil {
		return
	}
	e.Run("protect", "--stop", "--store", e.Store, e.WS)
}

// Write creates or replaces a workspace file and gives capture a moment to see
// the close-write (the daemon polls; a test that races it is testing nothing).
func (e *env) Write(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.WS, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
}

func (e *env) Read(rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.WS, rel))
	if err != nil {
		e.t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

func (e *env) Exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(e.WS, rel))
	return err == nil
}

// StatusJSON and HistoryJSON decode the documented client contracts.
type Status struct {
	Protected        bool     `json:"protected"`
	Root             string   `json:"root"`
	ProtectedDirs    []string `json:"protected_dirs"`
	Checkpoints      int      `json:"checkpoints"`
	Limited          bool     `json:"limited"`
	SettingUp        bool     `json:"setting_up"`
	LastCkptNS       int64    `json:"last_ckpt_ns"`
	FeedActive       bool     `json:"feed_active"`
	BaselineComplete bool     `json:"baseline_complete"`
	Outside          []string `json:"outside"`
	OutsideCount     int      `json:"outside_count"`
	AgentSessions    int      `json:"agent_sessions"`
}

type Checkpoint struct {
	ID         int    `json:"id"`
	TimeNS     int64  `json:"time_ns"`
	Badge      string `json:"badge"`
	Source     string `json:"source"`
	Name       string `json:"name"`
	Missed     int    `json:"missed"`
	Exceptions []struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	} `json:"exceptions"`
}

func (e *env) StatusJSON() Status {
	e.t.Helper()
	out := e.MustRun("status", "--json", "--root", e.WS, "--store", e.Store)
	var st Status
	if err := json.Unmarshal([]byte(lastJSONLine(out)), &st); err != nil {
		e.t.Fatalf("status --json is not valid JSON: %v\n%s", err, out)
	}
	return st
}

func (e *env) HistoryJSON() []Checkpoint {
	e.t.Helper()
	out := e.MustRun("history", "--json", "--root", e.WS, "--store", e.Store)
	var doc struct {
		Checkpoints []Checkpoint `json:"checkpoints"`
	}
	if err := json.Unmarshal([]byte(lastJSONLine(out)), &doc); err != nil {
		e.t.Fatalf("history --json is not valid JSON: %v\n%s", err, out)
	}
	return doc.Checkpoints
}

// RawJSON returns the JSON line itself, for asserting on the wire form (e.g.
// that a list field is [] and never null).
func (e *env) RawJSON(args ...string) string {
	e.t.Helper()
	return lastJSONLine(e.MustRun(args...))
}

// lastJSONLine picks the JSON document out of output that may carry warnings
// on preceding lines.
func lastJSONLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") {
			return l
		}
	}
	return ""
}

// WaitCheckpoints waits until the store holds at least n checkpoints.
func (e *env) WaitCheckpoints(n int, within time.Duration) []Checkpoint {
	e.t.Helper()
	deadline := time.Now().Add(within)
	var last []Checkpoint
	for time.Now().Before(deadline) {
		last = e.HistoryJSON()
		if len(last) >= n {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("waited %s for %d checkpoint(s); have %d", within, n, len(last))
	return nil
}

// Agent runs a command as a WRAPPED agent (so its writes attribute to the
// agent), through the real `run` path.
func (e *env) Agent(script string) string {
	e.t.Helper()
	return e.MustRun("run", "--root", e.WS, "--store", e.Store, "--", "bash", "-c", script+"; sleep 0.3")
}
