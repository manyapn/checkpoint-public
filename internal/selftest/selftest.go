// Package selftest answers the one question a stranger who just installed
// checkpoint actually has: "does this work on MY machine?"
//
// A passing test suite on someone else's machine proves nothing about your
// kernel, filesystem, container runtime, or distro. `checkpoint doctor`
// predicts readiness by probing capabilities; selftest goes further and
// DEMONSTRATES the product's real guarantees end to end against a throwaway
// workspace, then prints (or emits as JSON) a per-guarantee PASS / FAIL / SKIP
// verdict that can be attached to a bug report as evidence.
//
// Design rules, all of them load-bearing:
//
//   - DRIVE THE REAL BINARY. Every scenario shells out to the installed
//     `checkpoint` binary rather than calling library functions, because the
//     thing a user runs is the artifact, not the package. A library-level test
//     passing while the shipped binary is stale or mis-linked is exactly the
//     failure mode this exists to catch.
//   - PROVE, DON'T ASSERT. Verdicts come from bytes on disk (content, mode,
//     symlink target) and the CLI's own documented output, never from an
//     internal success signal.
//   - NEVER CLAIM WHAT WAS NOT TESTED. A guarantee scoped to a capability this
//     machine lacks (delete attribution needs the dirent change feed) is
//     SKIPPED with the reason named, never quietly passed.
//   - CHECKS MUST HAVE TEETH. The secrets check carries a control marker that
//     MUST be found in the store; if the control is missing, the check reports
//     failure instead of a vacuous "no secret found".
//   - LEAVE NOTHING RUNNING. Every daemon selftest starts is stopped, including
//     on failure and on panic. Everything it writes lives under the caller's
//     scratch directory; the user's real projects are never touched.
//
// selftest never reads or writes anything outside workDir (plus, when the
// caller's path is too long for a Unix socket, one short scratch store under
// the system temp dir, which is named in Env and removed on exit).
package selftest

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Status values for Result.Status. They are part of the JSON contract a bug
// report attaches, so they are lowercase and stable.
const (
	StatusPass = "pass"
	StatusFail = "fail"
	StatusSkip = "skip"
)

// Result is one guarantee's verdict.
//
// Detail is written for a human reading a bug report: for a FAIL it states what
// was expected and what was actually observed, and for a SKIP it states why the
// guarantee could not be exercised here. Err carries the raw underlying error
// (command exit status, stderr tail) when there was one.
type Result struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Err    string `json:"err,omitempty"`
}

// Report is the whole run: one Result per guarantee plus the machine facts a
// bug report needs. Results and Env are always present (never null) so a client
// can iterate them without a nil check.
type Report struct {
	Results   []Result          `json:"results"`
	Env       map[string]string `json:"env"`
	StartedNS int64             `json:"started_ns"`
	EndedNS   int64             `json:"ended_ns"`
}

// OK reports whether every guarantee that could be exercised held. Skips do not
// fail the run, but they are printed, and a report full of skips is evidence of
// an untested machine, not a healthy one.
func (r Report) OK() bool {
	for _, res := range r.Results {
		if res.Status == StatusFail {
			return false
		}
	}
	return true
}

// Budgets. selftest is something a user runs while watching, so it is bounded
// at every level: per command, and for the run as a whole. Exceeding the run
// budget skips the remaining scenarios with that reason rather than hanging.
const (
	runBudget      = 120 * time.Second
	cmdTimeout     = 45 * time.Second
	settleWait     = 400 * time.Millisecond
	checkpointWait = 25 * time.Second
)

// Run exercises checkpoint's real guarantees against a throwaway workspace.
//
// binPath is the checkpoint binary to test (the installed artifact, not this
// process). workDir is a scratch directory the caller owns and may delete
// afterwards; selftest creates its workspaces and stores inside it.
func Run(binPath string, workDir string) (rep Report) {
	s := &session{
		bin:     binPath,
		results: []Result{},
		env:     map[string]string{},
	}
	s.deadline = time.Now().Add(runBudget)
	started := time.Now()

	// A panic in here must still stop the daemons: an orphaned daemon holding a
	// temp directory is a worse outcome than any failed check.
	defer func() {
		if p := recover(); p != nil {
			s.fail("selftest-internal", fmt.Sprintf("selftest itself panicked: %v (this is a checkpoint bug; please report it with the stack above)", p), nil)
		}
		s.cleanup()
		rep = Report{
			Results:   s.results,
			Env:       s.env,
			StartedNS: started.UnixNano(),
			EndedNS:   time.Now().UnixNano(),
		}
		if rep.Results == nil {
			rep.Results = []Result{}
		}
		if rep.Env == nil {
			rep.Env = map[string]string{}
		}
	}()

	if err := s.setup(binPath, workDir); err != nil {
		s.fail("selftest-setup", err.Error(), err)
		return
	}
	s.scenarios()
	return
}

// ---------------------------------------------------------------- session

type session struct {
	bin      string
	work     string // resolved scratch root, owned by the caller
	proj     string // primary workspace
	store    string // primary store
	dis      string // disaster workspace (rm -rf scenario)
	disStore string
	tmpStore string // short-path fallback store root, removed on cleanup

	deadline time.Time
	results  []Result
	env      map[string]string

	// stores whose daemons must be stopped on exit, in start order.
	started []stopTarget

	secretMarker string // must never appear in the store
	canaryMarker string // must appear in the store (proves the scan has teeth)
}

type stopTarget struct{ store, root string }

func (s *session) add(r Result) { s.results = append(s.results, r) }

func (s *session) pass(name, detail string) {
	s.add(Result{Name: name, Status: StatusPass, Detail: detail})
}
func (s *session) skip(name, detail string) {
	s.add(Result{Name: name, Status: StatusSkip, Detail: detail})
}
func (s *session) fail(name, detail string, err error) {
	r := Result{Name: name, Status: StatusFail, Detail: detail}
	if err != nil {
		r.Err = trimErr(err.Error())
	}
	s.add(r)
}

func (s *session) outOfTime() bool { return time.Now().After(s.deadline) }

// setup resolves paths, builds the fixture tree, and records the environment.
func (s *session) setup(binPath, workDir string) error {
	if strings.TrimSpace(binPath) == "" {
		return fmt.Errorf("no checkpoint binary given to selftest")
	}
	abs, err := filepath.Abs(binPath)
	if err != nil {
		return fmt.Errorf("resolving the checkpoint binary %q: %w", binPath, err)
	}
	s.bin = abs
	if fi, err := os.Stat(abs); err != nil {
		return fmt.Errorf("checkpoint binary %s cannot be executed: %v", abs, err)
	} else if fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file (mode %v)", abs, fi.Mode())
	}

	if strings.TrimSpace(workDir) == "" {
		return fmt.Errorf("no scratch directory given to selftest")
	}
	wd, err := filepath.Abs(workDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return fmt.Errorf("creating the scratch directory %s: %w", wd, err)
	}
	// The daemon resolves symlinks in its root; comparing against an
	// unresolved path would produce phantom mismatches.
	if resolved, err := filepath.EvalSymlinks(wd); err == nil {
		wd = resolved
	}
	s.work = wd

	s.proj = filepath.Join(wd, "project")
	s.dis = filepath.Join(wd, "disaster")
	storeRoot := filepath.Join(wd, "stores")

	// The daemon's socket path is derived from the store directory and must fit
	// sockaddr_un (108 bytes). A long TMPDIR would otherwise fail deep inside
	// bind(); fall back to a short scratch store and say so in Env.
	if !socketPathFits(filepath.Join(storeRoot, "primary")) {
		tmp, err := os.MkdirTemp("", "ckst")
		if err != nil {
			return fmt.Errorf("creating a short-path store directory: %w", err)
		}
		s.tmpStore = tmp
		storeRoot = tmp
		s.env["store_path_note"] = "the scratch path was too long for a Unix socket; stores were placed under " + tmp
	}
	s.store = filepath.Join(storeRoot, "primary")
	s.disStore = filepath.Join(storeRoot, "disaster")
	for _, d := range []string{s.proj, s.dis, storeRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if !socketPathFits(s.store) {
		return fmt.Errorf("store path %s is too long for a Unix socket (%d bytes); run selftest with a shorter scratch directory", s.store, len(s.store))
	}

	s.secretMarker = "CHECKPOINT-SELFTEST-SECRET-" + randomHex()
	s.canaryMarker = "CHECKPOINT-SELFTEST-CANARY-" + randomHex()
	if err := s.buildFixture(); err != nil {
		return fmt.Errorf("building the selftest fixture tree: %w", err)
	}
	s.recordEnv()
	return nil
}

// buildFixture writes the tree the scenarios operate on. It deliberately
// includes the things a naive backup loses: an exec bit, a restrictive mode, an
// empty directory, a symlink, binary content, and a .env holding credential
// material that must never be captured.
func (s *session) buildFixture() error {
	write := func(root, rel, content string, mode os.FileMode) error {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			return err
		}
		return os.Chmod(p, mode) // the umask must not decide the fixture
	}
	for _, root := range []string{s.proj, s.dis} {
		if err := write(root, "README.md", "# checkpoint selftest\ncontrol marker: "+s.canaryMarker+"\n", 0o644); err != nil {
			return err
		}
		if err := write(root, "bin/run.sh", "#!/bin/sh\necho selftest\n", 0o755); err != nil {
			return err
		}
		if err := write(root, "src/main.go", "package main // v1\n", 0o644); err != nil {
			return err
		}
		if err := write(root, "src/private.go", "package main // mode 0600\n", 0o600); err != nil {
			return err
		}
		if err := write(root, "data/blob.bin", strings.Repeat("\x00\x01\x02\x03", 2048), 0o644); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(root, "logs"), 0o755); err != nil {
			return err
		}
		if err := os.Symlink("src/main.go", filepath.Join(root, "current")); err != nil {
			return err
		}
	}
	// Credential material lives only in the primary workspace, written BEFORE
	// protection so the first-time scan sees it too.
	return write(s.proj, ".env", "API_KEY="+s.secretMarker+"\n", 0o600)
}

// cleanup stops every daemon selftest started and removes the fallback store.
// It runs even when a scenario failed or panicked.
func (s *session) cleanup() {
	for i := len(s.started) - 1; i >= 0; i-- {
		t := s.started[i]
		// A scenario may have deleted the root (that is the point of the rm -rf
		// scenario); `protect --stop` resolves it, so put a directory back.
		_ = os.MkdirAll(t.root, 0o755)
		_, _ = s.cli(20*time.Second, "protect", "--stop", "--store", t.store, t.root)
	}
	if s.tmpStore != "" {
		_ = os.RemoveAll(s.tmpStore)
	}
}

// ------------------------------------------------------------- CLI driving

// cli runs the checkpoint binary under test and returns its combined output.
// Stdin is empty so a confirmation prompt can never block the run.
func (s *session) cli(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.bin, args...)
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("`%s %s` did not finish within %s", filepath.Base(s.bin), strings.Join(args, " "), timeout)
	}
	if err != nil {
		return string(out), fmt.Errorf("`%s %s` failed: %v\n%s", filepath.Base(s.bin), strings.Join(args, " "), err, trimErr(string(out)))
	}
	return string(out), nil
}

// shell runs a command as a SEPARATE process tree from the checkpoint binary.
// Provenance is decided by process lineage, so a "human" edit must genuinely be
// made by a process the agent did not spawn; writing the file from inside this
// binary would be classified as checkpoint's own write and prove nothing.
func (s *session) shell(timeout time.Duration, script string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("`sh -c %q` failed: %v\n%s", script, err, trimErr(string(out)))
	}
	return string(out), nil
}

type checkpointRec struct {
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

func (s *session) history(root, storeDir string) ([]checkpointRec, error) {
	out, err := s.cli(cmdTimeout, "history", "--json", "--root", root, "--store", storeDir)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Checkpoints []checkpointRec `json:"checkpoints"`
	}
	line := lastJSONLine(out)
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		return nil, fmt.Errorf("history --json did not emit valid JSON: %v\noutput: %s", err, trimErr(out))
	}
	return doc.Checkpoints, nil
}

// latestID returns the highest checkpoint id on record.
func (s *session) latestID(root, storeDir string) (int, error) {
	h, err := s.history(root, storeDir)
	if err != nil {
		return 0, err
	}
	if len(h) == 0 {
		return 0, fmt.Errorf("no checkpoints on record in %s", storeDir)
	}
	id := h[0].ID
	for _, c := range h {
		if c.ID > id {
			id = c.ID
		}
	}
	return id, nil
}

func (s *session) waitCheckpoints(root, storeDir string, n int, within time.Duration) ([]checkpointRec, error) {
	deadline := time.Now().Add(within)
	var last []checkpointRec
	for {
		h, err := s.history(root, storeDir)
		if err == nil {
			last = h
			if len(h) >= n {
				return h, nil
			}
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("waited %s for %d checkpoint(s); the store holds %d", within, n, len(last))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type statusDoc struct {
	Protected        bool     `json:"protected"`
	Root             string   `json:"root"`
	Checkpoints      int      `json:"checkpoints"`
	Limited          bool     `json:"limited"`
	SettingUp        bool     `json:"setting_up"`
	FeedActive       bool     `json:"feed_active"`
	BaselineComplete bool     `json:"baseline_complete"`
	Outside          []string `json:"outside"`
	AgentSessions    int      `json:"agent_sessions"`
}

func (s *session) status(root, storeDir string) (statusDoc, error) {
	var st statusDoc
	out, err := s.cli(cmdTimeout, "status", "--json", "--root", root, "--store", storeDir)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal([]byte(lastJSONLine(out)), &st); err != nil {
		return st, fmt.Errorf("status --json did not emit valid JSON: %v\noutput: %s", err, trimErr(out))
	}
	return st, nil
}

// ------------------------------------------------------------- environment

func (s *session) recordEnv() {
	s.env["os"] = runtime.GOOS
	s.env["arch"] = runtime.GOARCH
	s.env["kernel"] = kernelRelease()
	s.env["distro"] = distroName()
	s.env["cpus"] = strconv.Itoa(runtime.NumCPU())
	s.env["root"] = strconv.FormatBool(os.Geteuid() == 0)
	s.env["uid"] = strconv.Itoa(os.Geteuid())
	s.env["binary"] = s.bin
	s.env["workspace_path"] = s.proj
	s.env["workspace_fs"] = filesystemName(s.proj)
	s.env["store_path"] = s.store
	s.env["store_fs"] = filesystemName(filepath.Dir(s.store))
	s.env["container_hint"] = containerHint()
	s.env["feed_active"] = "unknown (protection never started)"

	out, err := s.cli(15*time.Second, "version")
	if err != nil {
		s.env["version"] = "unavailable: " + trimErr(err.Error())
	} else {
		s.env["version"] = flatten(out)
	}
}

// ------------------------------------------------------------- scenarios

func (s *session) scenarios() {
	protectedOK, reason := s.scenarioProtect()

	s.guard("write-captured-and-restorable", protectedOK, reason, s.scenarioCaptureRestore)
	s.scenarioDisaster() // its own workspace, store and daemon
	s.guard("transient-salvage", protectedOK, reason, s.scenarioTransient)
	s.guard("agent-undo-preserves-human", protectedOK, reason, s.scenarioProvenance)
	s.guard("agent-delete-undone", protectedOK, reason, s.scenarioAgentDelete)
	s.guard("secrets-never-captured", protectedOK, reason, s.scenarioSecrets)
}

// guard runs a scenario unless protection never started or the run budget is
// spent. In both cases the guarantee was not tested, so it is skipped with the
// reason rather than reported either way.
func (s *session) guard(name string, protectedOK bool, reason string, fn func()) {
	if !protectedOK {
		s.skip(name, "not tested (protection never started on this machine): "+reason)
		return
	}
	if s.outOfTime() {
		s.skip(name, fmt.Sprintf("not tested: selftest exceeded its %s time budget before reaching this check", runBudget))
		return
	}
	fn()
}

// 1. protect + setup checkpoint appears.
func (s *session) scenarioProtect() (bool, string) {
	const name = "protection-starts"
	out, err := s.cli(cmdTimeout, "protect", "--store", s.store, s.proj)
	if err != nil {
		if why, blocked := environmentBlocked(out + " " + err.Error()); blocked {
			s.fail(name, fmt.Sprintf("checkpoint could not start protecting %s on this machine: %s. "+
				"Nothing below could be tested. Run `%s doctor --root %s` for the full readiness report.",
				s.proj, why, filepath.Base(s.bin), s.proj), err)
			return false, why
		}
		s.fail(name, fmt.Sprintf("`protect` failed on %s. Expected: standing protection confirmed. Observed: the command exited nonzero (output below).", s.proj), err)
		return false, "`protect` exited nonzero"
	}
	// Registered for teardown the moment the daemon may exist.
	s.started = append(s.started, stopTarget{store: s.store, root: s.proj})

	h, err := s.waitCheckpoints(s.proj, s.store, 1, checkpointWait)
	if err != nil {
		s.fail(name, fmt.Sprintf("protection started but no setup checkpoint ever appeared. Expected: one automatic \"setup\" checkpoint covering the pre-existing tree. Observed: %v.\nprotect said:\n%s", err, trimErr(out)), err)
		return false, "no setup checkpoint appeared"
	}
	st, serr := s.status(s.proj, s.store)
	if serr != nil {
		s.fail(name, "protection started but `status --json` could not be read; a client cannot tell whether this project is protected", serr)
		return false, "status --json unreadable"
	}
	s.env["feed_active"] = strconv.FormatBool(st.FeedActive)
	if !st.Protected {
		s.fail(name, fmt.Sprintf("`status --json` reports protected=false while a daemon is running for %s. Expected: protected=true after `protect` returned.", s.proj), nil)
		return false, "status reports not protected"
	}
	detail := fmt.Sprintf("`protect` confirmed standing protection and the daemon cut setup checkpoint #%d (%s) covering the pre-existing tree; change feed %s, complete baseline %v",
		h[0].ID, orUnknown(h[0].Badge), feedWords(st.FeedActive, s.env["workspace_fs"]), st.BaselineComplete)
	// selftest's own fixture contains a .env, which checkpoint deliberately
	// never captures and names as an exception. That downgrades the badge by
	// design, so say so; otherwise the badge reads as a defect.
	if ex := exceptionSummary(h[0]); ex != "" {
		detail += "; named exceptions on that checkpoint: " + ex
	}
	if st.Limited {
		detail += "; status reports LIMITED protection (see `status` for the named reason)"
	}
	s.pass(name, detail)
	return true, ""
}

// 2. a write is captured and restorable: write, save, delete, restore, compare.
func (s *session) scenarioCaptureRestore() {
	const name = "write-captured-and-restorable"
	const rel = "notes/keep.md"
	want := "captured content " + randomHex() + "\n"
	p := filepath.Join(s.proj, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.fail(name, "could not create the scratch file's directory", err)
		return
	}
	if err := os.WriteFile(p, []byte(want), 0o644); err != nil {
		s.fail(name, "could not write the scratch file", err)
		return
	}
	time.Sleep(settleWait)

	if _, err := s.cli(cmdTimeout, "save", "--root", s.proj, "--store", s.store, "--name", "selftest-capture"); err != nil {
		s.fail(name, "`save` could not cut a checkpoint holding the new file", err)
		return
	}
	id, err := s.latestID(s.proj, s.store)
	if err != nil {
		s.fail(name, "`save` returned success but no checkpoint id could be read back from `history --json`", err)
		return
	}
	if err := os.Remove(p); err != nil {
		s.fail(name, "could not delete the file to simulate loss", err)
		return
	}
	time.Sleep(settleWait)

	out, err := s.cli(cmdTimeout, "restore", "--store", s.store, "--only", rel, strconv.Itoa(id), s.proj)
	if err != nil {
		s.fail(name, fmt.Sprintf("`restore --only %s %d` failed. Expected: the deleted file restored from checkpoint %d.", rel, id, id), err)
		return
	}
	got, rerr := os.ReadFile(p)
	if rerr != nil {
		s.fail(name, fmt.Sprintf("after `restore --only %s %d` the file is still missing at %s. Expected: %d bytes restored. Observed: %v.\nrestore said:\n%s",
			rel, id, p, len(want), rerr, trimErr(out)), rerr)
		return
	}
	if string(got) != want {
		s.fail(name, fmt.Sprintf("restored bytes differ. Expected %d bytes (sha %s), observed %d bytes (sha %s) at %s.",
			len(want), shortSum([]byte(want)), len(got), shortSum(got), p), nil)
		return
	}
	s.pass(name, fmt.Sprintf("a %d-byte write was captured into checkpoint #%d and restored byte-exact after the file was deleted (%s)", len(want), id, rel))
}

// 3. whole-project disaster: rm -rf the workspace, restore latest, compare the
// whole tree (content, mode, symlink targets, empty dirs). Runs on its own
// workspace, store and daemon so the destruction cannot disturb anything else.
func (s *session) scenarioDisaster() {
	const name = "rm-rf-disaster-restore"
	if s.outOfTime() {
		s.skip(name, fmt.Sprintf("not tested: selftest exceeded its %s time budget before reaching this check", runBudget))
		return
	}
	out, err := s.cli(cmdTimeout, "protect", "--store", s.disStore, s.dis)
	if err != nil {
		if why, blocked := environmentBlocked(out + " " + err.Error()); blocked {
			s.skip(name, "not tested (protection could not start on this machine): "+why)
			return
		}
		s.fail(name, fmt.Sprintf("`protect` failed on the disaster workspace %s", s.dis), err)
		return
	}
	s.started = append(s.started, stopTarget{store: s.disStore, root: s.dis})

	if _, err := s.waitCheckpoints(s.dis, s.disStore, 1, checkpointWait); err != nil {
		s.fail(name, "no setup checkpoint appeared for the disaster workspace, so there was nothing to restore from", err)
		return
	}
	id, err := s.latestID(s.dis, s.disStore)
	if err != nil {
		s.fail(name, "could not read the latest checkpoint id for the disaster workspace", err)
		return
	}
	before, err := fingerprint(s.dis)
	if err != nil {
		s.fail(name, "could not fingerprint the disaster workspace before deleting it", err)
		return
	}
	if len(before) < 6 {
		s.fail(name, fmt.Sprintf("the fixture tree looks wrong before the test (%d entries); refusing to claim a vacuous pass", len(before)), nil)
		return
	}

	if err := os.RemoveAll(s.dis); err != nil {
		s.fail(name, "could not `rm -rf` the disaster workspace", err)
		return
	}
	if _, err := os.Stat(s.dis); !os.IsNotExist(err) {
		s.fail(name, fmt.Sprintf("the workspace should be gone after rm -rf; stat says %v", err), nil)
		return
	}
	time.Sleep(settleWait)

	rout, err := s.cli(cmdTimeout, "restore", "--store", s.disStore, strconv.Itoa(id), s.dis)
	if err != nil {
		s.fail(name, fmt.Sprintf("after `rm -rf %s`, `restore %d` failed. Expected: the whole tree rebuilt from the out-of-tree store.", s.dis, id), err)
		return
	}
	after, ferr := fingerprint(s.dis)
	if ferr != nil {
		s.fail(name, fmt.Sprintf("restore %d reported success but the workspace could not be read back: %v\nrestore said:\n%s", id, ferr, trimErr(rout)), ferr)
		return
	}
	if d := diffFingerprints(before, after); d != "" {
		s.fail(name, fmt.Sprintf("restoring checkpoint %d after `rm -rf` is NOT byte-exact. Expected the %d recorded entries back with identical content, modes and symlink targets. Differences:\n%s", id, len(before), d), nil)
		return
	}
	s.pass(name, fmt.Sprintf("`rm -rf` of the whole workspace, then `restore %d`, reproduced all %d entries byte-exact (content, permission bits, exec bits, symlink targets and empty directories)", id, len(before)))
}

// 4. transient salvage: a file created, closed and deleted before any
// checkpoint ever held it is still recoverable from the version log.
func (s *session) scenarioTransient() {
	const name = "transient-salvage"
	const rel = "tmp/transient.txt"
	want := "never survived to any checkpoint " + randomHex() + "\n"
	p := filepath.Join(s.proj, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.fail(name, "could not create the transient file's directory", err)
		return
	}
	if err := os.WriteFile(p, []byte(want), 0o644); err != nil {
		s.fail(name, "could not write the transient file", err)
		return
	}
	time.Sleep(settleWait)
	if err := os.Remove(p); err != nil {
		s.fail(name, "could not delete the transient file", err)
		return
	}
	time.Sleep(settleWait)

	to := filepath.Join(s.work, "recovered")
	if err := os.MkdirAll(to, 0o755); err != nil {
		s.fail(name, "could not create the recovery output directory", err)
		return
	}
	dst := filepath.Join(to, rel)

	// Capture is asynchronous; poll the real command rather than guessing a
	// sleep, so a slow machine reports slow, not broken.
	deadline := time.Now().Add(15 * time.Second)
	var out string
	var err error
	for {
		out, err = s.cli(cmdTimeout, "recover", "--to", to, "--store", s.store, s.proj)
		if err == nil {
			if b, rerr := os.ReadFile(dst); rerr == nil && string(b) == want {
				s.pass(name, fmt.Sprintf("%s was created, closed and deleted without ever appearing in a checkpoint; `recover --to` returned its %d bytes byte-exact", rel, len(want)))
				return
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	got, rerr := os.ReadFile(dst)
	switch {
	case err != nil:
		s.fail(name, fmt.Sprintf("`recover --to %s` failed. Expected: %s offered as a recoverable file (no checkpoint holds it).", to, rel), err)
	case rerr != nil:
		s.fail(name, fmt.Sprintf("`recover --to %s` did not produce %s. Expected: the deleted transient's %d bytes. Observed: %v.\nrecover said:\n%s",
			to, dst, len(want), rerr, trimErr(out)), rerr)
	default:
		s.fail(name, fmt.Sprintf("recovered bytes differ for %s. Expected %d bytes (sha %s), observed %d bytes (sha %s).",
			rel, len(want), shortSum([]byte(want)), len(got), shortSum(got)), nil)
	}
}

// 5. provenance: an agent edit is reverted by `undo` while a CONCURRENT human
// edit to another file survives untouched. This is checkpoint's differentiator,
// so the human edit is made by a real separate process while the wrapped agent
// command is running; it is not simulated.
func (s *session) scenarioProvenance() {
	const name = "agent-undo-preserves-human"
	agentRel, humanRel := "agent.txt", "human.txt"
	agentPath := filepath.Join(s.proj, agentRel)
	humanPath := filepath.Join(s.proj, humanRel)
	const agentV1, humanV1 = "agent v1\n", "human v1\n"
	const agentV2, humanV2 = "agent v2\n", "human v2\n"

	if err := os.WriteFile(agentPath, []byte(agentV1), 0o644); err != nil {
		s.fail(name, "could not write the pre-turn agent file", err)
		return
	}
	if err := os.WriteFile(humanPath, []byte(humanV1), 0o644); err != nil {
		s.fail(name, "could not write the pre-turn human file", err)
		return
	}
	time.Sleep(settleWait)
	if _, err := s.cli(cmdTimeout, "save", "--root", s.proj, "--store", s.store, "--name", "selftest-baseline"); err != nil {
		s.fail(name, "`save` could not cut the pre-turn baseline checkpoint", err)
		return
	}
	baseline, err := s.latestID(s.proj, s.store)
	if err != nil {
		s.fail(name, "could not read the baseline checkpoint id", err)
		return
	}

	// The agent's turn: wrapped, so its whole process tree attributes to it.
	agentScript := fmt.Sprintf("sleep 0.8; printf 'agent v2\\n' > %s; sleep 0.3", shellQuote(agentPath))
	done := make(chan struct{})
	var runOut string
	var runErr error
	go func() {
		defer close(done)
		runOut, runErr = s.cli(cmdTimeout, "run", "--root", s.proj, "--store", s.store, "--", "sh", "-c", agentScript)
	}()
	// The human, editing a different file at the same time, from a process the
	// agent never spawned. The trailing sleep keeps the writer alive past the
	// close-write event so lineage is resolvable (a reaped one-shot writer
	// classifies Unknown, which is a conflict, not a wrong revert).
	time.Sleep(400 * time.Millisecond)
	_, herr := s.shell(20*time.Second, fmt.Sprintf("printf 'human v2\\n' > %s; sleep 0.5", shellQuote(humanPath)))
	<-done
	if runErr != nil {
		s.fail(name, "the wrapped agent command (`run -- sh -c ...`) failed, so there was no agent turn to undo", runErr)
		return
	}
	if herr != nil {
		s.fail(name, "the concurrent human edit could not be made", herr)
		return
	}
	if got, _ := os.ReadFile(agentPath); string(got) != agentV2 {
		s.fail(name, fmt.Sprintf("the agent's write never landed: %s = %q, expected %q. Nothing was proven, so this is not a pass.", agentRel, string(got), agentV2), nil)
		return
	}
	if got, _ := os.ReadFile(humanPath); string(got) != humanV2 {
		s.fail(name, fmt.Sprintf("the human's write never landed: %s = %q, expected %q. Nothing was proven, so this is not a pass.", humanRel, string(got), humanV2), nil)
		return
	}

	out, uerr := s.cli(cmdTimeout, "undo", "--root", s.proj, "--store", s.store)
	if uerr != nil {
		s.fail(name, fmt.Sprintf("`undo` failed after a turn in which the agent edited %s and a concurrent human process edited %s. "+
			"Expected: the agent's file reverted to the baseline (checkpoint %d) and the human's left alone. "+
			"A nonzero exit here usually means a writer could not be attributed and was reported as a conflict; see the output.", agentRel, humanRel, baseline), uerr)
		return
	}
	agentGot, _ := os.ReadFile(agentPath)
	humanGot, _ := os.ReadFile(humanPath)
	var problems []string
	if string(agentGot) != agentV1 {
		problems = append(problems, fmt.Sprintf("the agent-written %s was NOT reverted: expected %q (the baseline content), observed %q", agentRel, agentV1, string(agentGot)))
	}
	if string(humanGot) != humanV2 {
		problems = append(problems, fmt.Sprintf("the human-written %s was modified by undo: expected %q (untouched), observed %q. This is the failure checkpoint exists to prevent", humanRel, humanV2, string(humanGot)))
	}
	if len(problems) > 0 {
		s.fail(name, strings.Join(problems, "; ")+".\nundo said:\n"+trimErr(out), nil)
		return
	}
	s.pass(name, fmt.Sprintf("during one wrapped agent turn the agent edited %s and a separate human process concurrently edited %s; `undo` reverted %s to its baseline (checkpoint %d) and left %s untouched",
		agentRel, humanRel, agentRel, baseline, humanRel))
	_ = runOut
}

// 6. an agent-DELETED file is restored by undo. Delete provenance comes from
// the dirent change feed, so on a filesystem without one this guarantee does
// not apply here and is SKIPPED with that reason, never silently passed.
func (s *session) scenarioAgentDelete() {
	const name = "agent-delete-undone"
	st, err := s.status(s.proj, s.store)
	if err != nil {
		s.fail(name, "could not read `status --json` to determine whether the change feed is active", err)
		return
	}
	if !st.FeedActive {
		s.skip(name, fmt.Sprintf("not tested: the dirent change feed is unavailable on this workspace's filesystem (%s), and delete attribution is feed-scoped. "+
			"Deletions carry no provenance here, so `undo` cannot know an agent deleted a file. Restore-by-checkpoint still brings deleted files back "+
			"(covered by rm-rf-disaster-restore). To exercise this guarantee, put the project on ext4/xfs/btrfs; in a container, bind-mount it from the host "+
			"instead of working on the container's overlay layer.", s.env["workspace_fs"]))
		return
	}

	const rel = "doomed.txt"
	p := filepath.Join(s.proj, rel)
	want := "the agent will delete this " + randomHex() + "\n"
	if err := os.WriteFile(p, []byte(want), 0o644); err != nil {
		s.fail(name, "could not write the file the agent is meant to delete", err)
		return
	}
	time.Sleep(settleWait)
	if _, err := s.cli(cmdTimeout, "save", "--root", s.proj, "--store", s.store, "--name", "selftest-delete-baseline"); err != nil {
		s.fail(name, "`save` could not cut the pre-turn baseline checkpoint", err)
		return
	}
	baseline, err := s.latestID(s.proj, s.store)
	if err != nil {
		s.fail(name, "could not read the baseline checkpoint id", err)
		return
	}
	script := fmt.Sprintf("rm %s; sleep 0.3", shellQuote(p))
	if _, err := s.cli(cmdTimeout, "run", "--root", s.proj, "--store", s.store, "--", "sh", "-c", script); err != nil {
		s.fail(name, "the wrapped agent command that deletes the file failed", err)
		return
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		s.fail(name, fmt.Sprintf("the agent's `rm %s` did not actually delete the file (stat: %v); nothing was proven", rel, err), nil)
		return
	}
	out, uerr := s.cli(cmdTimeout, "undo", "--root", s.proj, "--store", s.store)
	if uerr != nil {
		s.fail(name, fmt.Sprintf("`undo` failed after a wrapped agent deleted %s. Expected: the file restored from baseline checkpoint %d.", rel, baseline), uerr)
		return
	}
	got, rerr := os.ReadFile(p)
	if rerr != nil {
		s.fail(name, fmt.Sprintf("`undo` did not bring back the agent-deleted %s. Expected: %d bytes restored from checkpoint %d. Observed: %v.\nundo said:\n%s",
			rel, len(want), baseline, rerr, trimErr(out)), rerr)
		return
	}
	if string(got) != want {
		s.fail(name, fmt.Sprintf("the agent-deleted %s came back with the wrong bytes: expected %d (sha %s), observed %d (sha %s)",
			rel, len(want), shortSum([]byte(want)), len(got), shortSum(got)), nil)
		return
	}
	s.pass(name, fmt.Sprintf("a wrapped agent deleted %s; `undo` recognised the deletion as the agent's (change feed active) and restored the file byte-exact from checkpoint %d", rel, baseline))
}

// 7. secrets are never captured. The .env marker must appear NOWHERE under the
// store, including inside compressed objects. A control marker from an
// ordinary file must be found, otherwise the search proves nothing and this
// reports failure rather than a vacuous pass.
func (s *session) scenarioSecrets() {
	const name = "secrets-never-captured"
	envPath := filepath.Join(s.proj, ".env")
	// Rewrite the credential file while protection is LIVE, so the close-write
	// capture path is exercised as well as the first-time scan.
	if err := os.WriteFile(envPath, []byte("API_KEY="+s.secretMarker+"\nROTATED=1\n"), 0o600); err != nil {
		s.fail(name, "could not rewrite the credential fixture file", err)
		return
	}
	time.Sleep(settleWait)
	if _, err := s.cli(cmdTimeout, "save", "--root", s.proj, "--store", s.store, "--name", "selftest-secrets"); err != nil {
		s.fail(name, "`save` could not cut a checkpoint after the credential file was written", err)
		return
	}

	secretHits, err := scanStore(s.store, []byte(s.secretMarker))
	if err != nil {
		s.fail(name, fmt.Sprintf("could not read the store at %s to check it for credential material", s.store), err)
		return
	}
	canaryHits, err := scanStore(s.store, []byte(s.canaryMarker))
	if err != nil {
		s.fail(name, fmt.Sprintf("could not read the store at %s to check the control marker", s.store), err)
		return
	}
	if len(canaryHits) == 0 {
		s.fail(name, fmt.Sprintf("INCONCLUSIVE, reported as a failure: the control marker written into README.md was not found anywhere under %s either, "+
			"so \"the secret is not in the store\" proves nothing (the search may be unable to see stored content, or nothing was captured at all). "+
			"Expected: the control marker inside a compressed object.", s.store), nil)
		return
	}
	if len(secretHits) > 0 {
		s.fail(name, fmt.Sprintf("CREDENTIAL MATERIAL WAS CAPTURED: the marker written only into %s appears in %d file(s) under the store: %s. "+
			"Expected: .env is on the never-captured denylist and its bytes must never enter the object store.",
			envPath, len(secretHits), strings.Join(shorten(secretHits, s.store, 5), ", ")), nil)
		return
	}
	if b, rerr := os.ReadFile(envPath); rerr != nil || !bytes.Contains(b, []byte(s.secretMarker)) {
		s.fail(name, fmt.Sprintf("the credential file %s was modified or removed while checkpoint was watching; capture must never write inside the protected folder", envPath), rerr)
		return
	}
	s.pass(name, fmt.Sprintf("a .env holding a unique marker was written before and during protection; the marker appears nowhere under %s (%d store files searched, compressed objects decompressed and searched too), while the control marker from README.md was found in %d, which proves the search really does see stored content",
		s.store, countFiles(s.store), len(canaryHits)))
}

// ------------------------------------------------------------- rendering

// Text renders the report the way a user reads it: the verdict per guarantee,
// every failure and skip explained in words, then the machine facts a bug
// report needs.
func (r Report) Text() string {
	var b strings.Builder
	pass, fail, skip := 0, 0, 0
	width := 0
	for _, res := range r.Results {
		switch res.Status {
		case StatusPass:
			pass++
		case StatusFail:
			fail++
		default:
			skip++
		}
		if len(res.Name) > width {
			width = len(res.Name)
		}
	}
	elapsed := time.Duration(r.EndedNS - r.StartedNS)
	fmt.Fprintf(&b, "checkpoint selftest: %d checks, %d passed, %d failed, %d skipped (%s)\n\n",
		len(r.Results), pass, fail, skip, elapsed.Round(100*time.Millisecond))

	for _, res := range r.Results {
		label := "SKIP"
		switch res.Status {
		case StatusPass:
			label = "PASS"
		case StatusFail:
			label = "FAIL"
		}
		fmt.Fprintf(&b, "%s  %-*s  %s\n", label, width, res.Name, indentContinuation(res.Detail, width+6))
		if res.Err != "" {
			fmt.Fprintf(&b, "      %-*s  error: %s\n", width, "", indentContinuation(res.Err, width+6))
		}
	}

	b.WriteString("\nEnvironment\n")
	keys := make([]string, 0, len(r.Env))
	for k := range r.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kw := 0
	for _, k := range keys {
		if len(k) > kw {
			kw = len(k)
		}
	}
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-*s  %s\n", kw, k, flatten(r.Env[k]))
	}

	b.WriteString("\n")
	switch {
	case fail > 0:
		fmt.Fprintf(&b, "VERDICT: %d guarantee(s) DID NOT HOLD on this machine. Please attach the full `selftest --json` output\n"+
			"to a bug report; the Environment block above is what makes it actionable.\n", fail)
	case pass == 0:
		b.WriteString("VERDICT: nothing could be tested on this machine. Run `checkpoint doctor`: it names what is missing and how to fix it.\n")
	case skip > 0:
		tail := "%d checks could not be tested here and are explained above as SKIP.\n"
		if skip == 1 {
			tail = "%d check could not be tested here and is explained above as SKIP.\n"
		}
		fmt.Fprintf(&b, "VERDICT: every guarantee that applies to this machine held (%d of %d).\n"+
			"That is a property of this environment, not a failure: "+tail, pass, len(r.Results), skip)
	default:
		b.WriteString("VERDICT: every guarantee held on this machine.\n")
	}
	return b.String()
}

// JSON is what a bug report attaches: stable field names, lists never null.
func (r Report) JSON() ([]byte, error) {
	if r.Results == nil {
		r.Results = []Result{}
	}
	if r.Env == nil {
		r.Env = map[string]string{}
	}
	return json.MarshalIndent(r, "", "  ")
}

// ------------------------------------------------------------- helpers

// fingerprint records every path under root the way a user would compare it:
// kind, permission bits, content hash, symlink target. Mtimes are excluded,
// because checkpoint does not promise them.
func fingerprint(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		fi, lerr := os.Lstat(p)
		if lerr != nil {
			return lerr
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, terr := os.Readlink(p)
			if terr != nil {
				return terr
			}
			out[rel] = "symlink -> " + target
		case fi.IsDir():
			out[rel] = fmt.Sprintf("dir mode=%04o", fi.Mode().Perm())
		default:
			b, berr := os.ReadFile(p)
			if berr != nil {
				return berr
			}
			out[rel] = fmt.Sprintf("file mode=%04o sha=%s", fi.Mode().Perm(), shortSum(b))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// diffFingerprints renders the difference between two trees, "" when identical.
// It is bounded: a wholly failed restore must not print thousands of lines.
func diffFingerprints(want, got map[string]string) string {
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	all := make([]string, 0, len(keys))
	for k := range keys {
		all = append(all, k)
	}
	sort.Strings(all)
	var b strings.Builder
	shown, total := 0, 0
	for _, k := range all {
		w, okW := want[k]
		g, okG := got[k]
		if okW && okG && w == g {
			continue
		}
		total++
		if shown >= 10 {
			continue
		}
		shown++
		switch {
		case !okG:
			fmt.Fprintf(&b, "  missing after restore: %s (%s)\n", k, w)
		case !okW:
			fmt.Fprintf(&b, "  unexpected after restore: %s (%s)\n", k, g)
		default:
			fmt.Fprintf(&b, "  changed: %s: %s -> %s\n", k, w, g)
		}
	}
	if total > shown {
		fmt.Fprintf(&b, "  … and %d more difference(s)\n", total-shown)
	}
	return b.String()
}

// maxScanFile bounds how much of one store file is searched for a marker. Store
// objects are single file versions; anything larger is not a fixture of ours.
const maxScanFile = 64 << 20

// scanStore searches every file under the store for marker, decompressing zlib
// (git loose objects) as well as reading raw bytes. A check that only looked at
// plaintext would pass on a store full of compressed secrets.
func scanStore(storeDir string, marker []byte) ([]string, error) {
	var hits []string
	err := filepath.WalkDir(storeDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable entry is not a hit; keep going
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if fi, ierr := d.Info(); ierr == nil && fi.Size() > maxScanFile {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if bytes.Contains(b, marker) {
			hits = append(hits, p)
			return nil
		}
		if zr, zerr := zlib.NewReader(bytes.NewReader(b)); zerr == nil {
			dec, _ := io.ReadAll(io.LimitReader(zr, maxScanFile))
			zr.Close()
			if bytes.Contains(dec, marker) {
				hits = append(hits, p+" (inside the compressed object)")
			}
		}
		return nil
	})
	if err != nil {
		return hits, err
	}
	return hits, nil
}

func countFiles(root string) int {
	n := 0
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func shorten(paths []string, root string, max int) []string {
	out := make([]string, 0, max)
	for i, p := range paths {
		if i >= max {
			out = append(out, fmt.Sprintf("… and %d more", len(paths)-max))
			break
		}
		if rel, err := filepath.Rel(root, p); err == nil {
			p = rel
		}
		out = append(out, p)
	}
	return out
}

// environmentBlocked recognises the failures that mean "this machine cannot run
// checkpoint" rather than "checkpoint is broken", and renders them as words a
// user can act on.
func environmentBlocked(out string) (string, bool) {
	l := strings.ToLower(out)
	switch {
	case strings.Contains(l, "cap_sys_admin"), strings.Contains(l, "operation not permitted"), strings.Contains(l, "permission denied"):
		return "fanotify could not be armed, because checkpoint needs CAP_SYS_ADMIN. Run as root, grant the binary the capability " +
			"(`sudo setcap cap_sys_admin+ep $(command -v checkpoint)`), or start the container with `--cap-add SYS_ADMIN`", true
	case strings.Contains(l, "function not implemented"), strings.Contains(l, "enosys"):
		return "this kernel has no fanotify support (CONFIG_FANOTIFY is off), so writes cannot be watched at all", true
	case strings.Contains(l, "too many open files"), strings.Contains(l, "max_user_groups"):
		return "the per-user fanotify group limit is exhausted; raise /proc/sys/fs/fanotify/max_user_groups or stop other fanotify users", true
	default:
		return "", false
	}
}

// exceptionSummary renders a checkpoint's named exceptions, marking the ones
// selftest's own fixture causes on purpose so a reader is not sent hunting.
func exceptionSummary(c checkpointRec) string {
	if len(c.Exceptions) == 0 {
		return ""
	}
	parts := make([]string, 0, len(c.Exceptions))
	for i, ex := range c.Exceptions {
		if i == 4 {
			parts = append(parts, fmt.Sprintf("… and %d more", len(c.Exceptions)-4))
			break
		}
		note := ""
		if filepath.Base(ex.Path) == ".env" {
			note = " [expected: selftest's fixture deliberately includes a .env]"
		}
		parts = append(parts, fmt.Sprintf("%s (%s)%s", ex.Path, ex.Reason, note))
	}
	return strings.Join(parts, ", ")
}

func feedWords(active bool, fsName string) string {
	if active {
		return fmt.Sprintf("ACTIVE on %s (deletions are attributed; checkpoints scale with changes)", fsName)
	}
	return fmt.Sprintf("unavailable on %s (deletions are not attributed; checkpoints use full scans)", fsName)
}

// lastJSONLine picks the JSON document out of output whose earlier lines may
// carry warnings.
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

func trimErr(s string) string {
	s = strings.TrimSpace(s)
	const max = 1200
	if len(s) > max {
		return "…" + s[len(s)-max:]
	}
	return s
}

func flatten(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")), " ")
}

// indentContinuation keeps multi-line details aligned under the first column.
func indentContinuation(s string, indent int) string {
	if !strings.Contains(s, "\n") {
		return s
	}
	pad := "\n" + strings.Repeat(" ", indent)
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", pad)
}

func shortSum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func randomHex() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// shellQuote makes a path safe inside the single-quoted shell scripts selftest
// hands to `sh -c`. Scratch paths are ours, but a caller's workDir is not.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// socketPathFits mirrors the CLI's own sockaddr_un guard, so selftest fails with
// a sentence instead of a bind() errno.
func socketPathFits(storeDir string) bool {
	const sunPathMax = 108
	return len(filepath.Join(storeDir, "daemon.sock"))+1 <= sunPathMax
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no badge reported"
	}
	return s
}

// distroName reports the distribution a bug report needs, from os-release.
func distroName() string {
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var id, ver string
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			v = strings.Trim(v, `"`)
			switch k {
			case "PRETTY_NAME":
				return v
			case "ID":
				id = v
			case "VERSION_ID":
				ver = v
			}
		}
		if id != "" {
			return strings.TrimSpace(id + " " + ver)
		}
	}
	return "unknown"
}

// containerHint reports whether this looks like a container, because "works on
// my machine" and "works in my container" are different claims.
func containerHint() string {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "docker (/.dockerenv present)"
	}
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		c := string(b)
		switch {
		case strings.Contains(c, "docker"):
			return "docker (cgroup)"
		case strings.Contains(c, "kubepods"):
			return "kubernetes (cgroup)"
		case strings.Contains(c, "lxc"):
			return "lxc (cgroup)"
		}
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return "podman (/run/.containerenv present)"
	}
	return "no container detected"
}
