// Package selftest proves the guarantees on the machine it runs on by driving
// the real checkpoint binary against a throwaway project and checking bytes
// on disk. Every scenario passes, fails, or is skipped with the reason.
package selftest

import (
	"bytes"
	"compress/zlib"
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
	"strings"
	"time"
)

type Result struct {
	Name   string `json:"name"`
	Status string `json:"status"` // pass | fail | skip
	Detail string `json:"detail"`
}

type Report struct {
	Results []Result          `json:"results"`
	Env     map[string]string `json:"env"`
}

func (r Report) OK() bool {
	for _, res := range r.Results {
		if res.Status == "fail" {
			return false
		}
	}
	return true
}

func (r Report) Text() string {
	var b strings.Builder
	for _, res := range r.Results {
		fmt.Fprintf(&b, "%-4s  %-28s %s\n", strings.ToUpper(res.Status), res.Name, res.Detail)
	}
	if r.OK() {
		b.WriteString("\nVERDICT: every guarantee that could be tested held on this machine.\n")
	} else {
		b.WriteString("\nVERDICT: at least one guarantee FAILED here.\n")
	}
	return b.String()
}

type session struct {
	bin, root, store string
	verbose          bool
	feed             bool
	report           Report
	step             int
}

// Run exercises every scenario under work. bin is the binary under test.
func Run(bin, work string, verbose bool) Report {
	s := &session{bin: bin, root: filepath.Join(work, "project"), store: filepath.Join(work, "store"), verbose: verbose}
	s.report.Env = map[string]string{"os": runtime.GOOS, "arch": runtime.GOARCH, "work": work}
	if out, err := exec.Command("uname", "-r").Output(); err == nil {
		s.report.Env["kernel"] = strings.TrimSpace(string(out))
	}
	defer s.cli(10*time.Second, "protect", "--stop")
	for _, scenario := range []struct {
		name string
		fn   func() error
	}{
		{"protection-starts", s.protectionStarts},
		{"write-captured-and-restorable", s.writeCaptured},
		{"rm-rf-disaster-restore", s.disasterRestore},
		{"transient-salvage", s.transientSalvage},
		{"agent-undo-preserves-human", s.undoPreservesHuman},
		{"agent-delete-undone", s.agentDeleteUndone},
		{"secrets-never-captured", s.secretsNeverCaptured},
	} {
		s.step++
		err := scenario.fn()
		status, detail := "pass", "ok"
		if err != nil {
			status, detail = "fail", err.Error()
			if strings.HasPrefix(err.Error(), "skip: ") {
				status, detail = "skip", strings.TrimPrefix(err.Error(), "skip: ")
			}
		}
		s.report.Results = append(s.report.Results, Result{scenario.name, status, detail})
		if s.verbose {
			fmt.Printf("STEP %d %-28s %s  %s\n", s.step, scenario.name, strings.ToUpper(status), detail)
		}
		if status == "fail" && scenario.name == "protection-starts" {
			break
		}
	}
	return s.report
}

func (s *session) cli(timeout time.Duration, args ...string) (string, error) {
	cmd := exec.Command(s.bin, append([]string{args[0], "--root", s.root, "--store", s.store}, args[1:]...)...)
	cmd.Dir = s.root
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if s.verbose {
		fmt.Printf("    $ checkpoint %s\n", strings.Join(args, " "))
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" {
				fmt.Printf("      %s\n", line)
			}
		}
	}
	if err != nil {
		return string(out), fmt.Errorf("checkpoint %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// shell runs a command as a separate process so the daemon sees a writer
// that is not checkpoint itself.
func (s *session) shell(script string) error {
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = s.root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sh -c %q: %v: %s", script, err, out)
	}
	return nil
}

func (s *session) read(rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.root, rel))
	return string(b), err
}

func (s *session) protectionStarts() error {
	os.MkdirAll(filepath.Join(s.root, "src"), 0o755)
	os.MkdirAll(filepath.Join(s.root, "empty"), 0o755)
	os.WriteFile(filepath.Join(s.root, "README.md"), []byte("control-marker-"+marker()+"\n"), 0o644)
	os.WriteFile(filepath.Join(s.root, "src/main.go"), []byte("package main\n"), 0o644)
	os.WriteFile(filepath.Join(s.root, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(s.root, "agent.txt"), []byte("agent-v1\n"), 0o644)
	os.WriteFile(filepath.Join(s.root, "human.txt"), []byte("human-v1\n"), 0o644)
	os.WriteFile(filepath.Join(s.root, "doomed.txt"), []byte("do not delete me\n"), 0o644)
	os.Symlink("README.md", filepath.Join(s.root, "link"))
	if out, err := s.cli(30*time.Second, "doctor"); err != nil {
		return fmt.Errorf("doctor refused this machine:\n%s", out)
	}
	if _, err := s.cli(30*time.Second, "protect"); err != nil {
		return err
	}
	out, err := s.cli(10*time.Second, "status", "--json")
	if err != nil {
		return err
	}
	var st struct {
		Protected   bool `json:"protected"`
		FeedActive  bool `json:"feed_active"`
		Checkpoints int  `json:"checkpoints"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil || !st.Protected || st.Checkpoints != 1 {
		return fmt.Errorf("after protect, status = %s", strings.TrimSpace(out))
	}
	s.feed = st.FeedActive
	return nil
}

func (s *session) writeCaptured() error {
	if err := s.shell("mkdir -p notes && printf 'keep this\\n' > notes/keep.md"); err != nil {
		return err
	}
	if _, err := s.cli(30*time.Second, "save"); err != nil {
		return err
	}
	id, err := s.latestID()
	if err != nil {
		return err
	}
	os.Remove(filepath.Join(s.root, "notes/keep.md"))
	if _, err := s.cli(30*time.Second, "restore", "--only", "notes/keep.md", fmt.Sprint(id)); err != nil {
		return err
	}
	if got, _ := s.read("notes/keep.md"); got != "keep this\n" {
		return fmt.Errorf("restored content = %q", got)
	}
	return nil
}

func (s *session) disasterRestore() error {
	if _, err := s.cli(30*time.Second, "save", "--name", "before-disaster"); err != nil {
		return err
	}
	id, _ := s.latestID()
	before := fingerprint(s.root)
	if err := s.shell("rm -rf -- \"$PWD\"/* \"$PWD\"/.[!.]*"); err != nil {
		return err
	}
	if len(fingerprint(s.root)) != 0 {
		return fmt.Errorf("rm -rf left files behind")
	}
	if _, err := s.cli(60*time.Second, "restore", fmt.Sprint(id)); err != nil {
		return err
	}
	if diff := diffFingerprints(before, fingerprint(s.root)); diff != "" {
		return fmt.Errorf("tree differs after restore: %s", diff)
	}
	return nil
}

func (s *session) transientSalvage() error {
	content := "transient-" + marker() + "\n"
	if err := s.shell(fmt.Sprintf("mkdir -p tmp && printf '%s' > tmp/transient.txt && rm tmp/transient.txt", content)); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond) // let the daemon drain the close event
	out := filepath.Join(filepath.Dir(s.root), "recovered")
	if _, err := s.cli(30*time.Second, "recover", "--to", out); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(out, "tmp/transient.txt"))
	if err != nil || string(b) != content {
		return fmt.Errorf("recovered %q, %v", b, err)
	}
	return nil
}

func (s *session) undoPreservesHuman() error {
	s.cli(30*time.Second, "save")
	agent := exec.Command(s.bin, "run", "--root", s.root, "--store", s.store, "--",
		"sh", "-c", "sleep 0.3; printf 'agent-v2\\n' > agent.txt; printf 'agent-made\\n' > generated.txt; sleep 0.2")
	agent.Dir = s.root
	if err := agent.Start(); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	if err := s.shell("printf 'human-v2\\n' > human.txt"); err != nil {
		return err
	}
	if err := agent.Wait(); err != nil {
		return fmt.Errorf("run: %v", err)
	}
	if _, err := s.cli(60*time.Second, "undo"); err != nil {
		return err
	}
	agentNow, _ := s.read("agent.txt")
	humanNow, _ := s.read("human.txt")
	_, genErr := os.Stat(filepath.Join(s.root, "generated.txt"))
	switch {
	case agentNow != "agent-v1\n":
		return fmt.Errorf("agent.txt = %q, want the pre-turn version", agentNow)
	case humanNow != "human-v2\n":
		return fmt.Errorf("human.txt = %q; the human edit must survive", humanNow)
	case genErr == nil:
		return fmt.Errorf("generated.txt should have been removed")
	}
	return nil
}

func (s *session) agentDeleteUndone() error {
	if !s.feed {
		return fmt.Errorf("skip: this filesystem does not report who deletes files (no change feed), so an agent's delete cannot be undone here")
	}
	s.cli(30*time.Second, "save")
	if _, err := s.cli(30*time.Second, "run", "--", "sh", "-c", "rm doomed.txt"); err != nil {
		return err
	}
	if _, err := s.cli(60*time.Second, "undo"); err != nil {
		return err
	}
	if got, err := s.read("doomed.txt"); err != nil || got != "do not delete me\n" {
		return fmt.Errorf("doomed.txt = %q, %v", got, err)
	}
	return nil
}

func (s *session) secretsNeverCaptured() error {
	secret := "secret-marker-" + marker()
	if err := s.shell("printf 'TOKEN=" + secret + "\\n' > .env"); err != nil {
		return err
	}
	if _, err := s.cli(30*time.Second, "save", "--name", "with-secret"); err != nil {
		return err
	}
	control, _ := s.read("README.md")
	control = strings.TrimSpace(control)
	secretHits, controlHits := 0, 0
	filepath.WalkDir(s.store, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, _ := os.ReadFile(p)
		if zr, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
			if plain, err := io.ReadAll(zr); err == nil {
				data = append(data, plain...)
			}
		}
		if bytes.Contains(data, []byte(secret)) {
			secretHits++
		}
		if bytes.Contains(data, []byte(control)) {
			controlHits++
		}
		return nil
	})
	if controlHits == 0 {
		return fmt.Errorf("the control marker from README.md was not found in the store, so the search itself is broken")
	}
	if secretHits > 0 {
		return fmt.Errorf("the .env marker was found in %d store file(s)", secretHits)
	}
	out, _ := s.cli(10*time.Second, "history")
	if !strings.Contains(out, ".env") {
		return fmt.Errorf(".env should be listed as a named exception in history")
	}
	return nil
}

func (s *session) latestID() (int, error) {
	out, err := s.cli(10*time.Second, "history", "--json")
	if err != nil {
		return 0, err
	}
	var h struct {
		Checkpoints []struct {
			ID int `json:"id"`
		} `json:"checkpoints"`
	}
	if err := json.Unmarshal([]byte(out), &h); err != nil || len(h.Checkpoints) == 0 {
		return 0, fmt.Errorf("history --json: %s", out)
	}
	return h.Checkpoints[0].ID, nil
}

// fingerprint describes a tree by kind, mode, content hash and link target.
func fingerprint(root string) map[string]string {
	fp := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		fi, _ := d.Info()
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			fp[rel] = "symlink:" + target
		case fi.IsDir():
			fp[rel] = fmt.Sprintf("dir:%o", fi.Mode().Perm())
		default:
			b, _ := os.ReadFile(p)
			sum := sha256.Sum256(b)
			fp[rel] = fmt.Sprintf("file:%o:%s", fi.Mode().Perm(), hex.EncodeToString(sum[:8]))
		}
		return nil
	})
	return fp
}

func diffFingerprints(want, got map[string]string) string {
	var diffs []string
	for rel, w := range want {
		if g, ok := got[rel]; !ok {
			diffs = append(diffs, "missing "+rel)
		} else if g != w {
			diffs = append(diffs, fmt.Sprintf("%s: %s != %s", rel, g, w))
		}
	}
	for rel := range got {
		if _, ok := want[rel]; !ok {
			diffs = append(diffs, "extra "+rel)
		}
	}
	sort.Strings(diffs)
	return strings.Join(diffs, "; ")
}

// marker is a string unlikely to exist anywhere in the store by accident.
func marker() string {
	return hex.EncodeToString([]byte(fmt.Sprint(time.Now().UnixNano())))[:12]
}
