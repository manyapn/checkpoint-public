// Sandbox plumbing: one throwaway workspace plus its own daemon and store per
// scenario round, and the disk-state comparison every scenario is scored with.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// env is one scenario's sandbox: a workspace, an out-of-tree store, and the
// daemon protecting them. Nothing is shared between rounds.
type env struct {
	ws, store string
	daemon    *exec.Cmd
	log       string
}

func newEnv(base, name string, round int) (*env, error) {
	dir := filepath.Join(base, fmt.Sprintf("%s-%d", name, round))
	os.RemoveAll(dir)
	e := &env{ws: filepath.Join(dir, "ws"), store: filepath.Join(dir, "store"), log: filepath.Join(dir, "daemon.log")}
	if err := os.MkdirAll(e.ws, 0o755); err != nil {
		return nil, err
	}
	return e, nil
}

// start launches the daemon and waits both for it to arm and for its setup
// checkpoint to land, so a scenario never races the baseline it compares to.
func (e *env) start() error {
	f, err := os.Create(e.log)
	if err != nil {
		return err
	}
	e.daemon = exec.Command(bin, "daemon", "--store", e.store, e.ws)
	e.daemon.Stdout, e.daemon.Stderr = f, f
	if err := e.daemon.Start(); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(e.log)
		if strings.Contains(string(b), "READY") {
			for time.Now().Before(deadline) {
				if _, err := os.Stat(filepath.Join(e.store, "manifests", "0.json")); err == nil {
					return nil
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("daemon not ready: %s", e.log)
}

func (e *env) stop() {
	if e.daemon != nil && e.daemon.Process != nil {
		e.daemon.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { e.daemon.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			e.daemon.Process.Kill()
			e.daemon.Wait()
		}
	}
}

func (e *env) cli(args ...string) (string, error) {
	out, err := exec.Command(bin, args...).CombinedOutput()
	return string(out), err
}

// fingerprint maps each relative path under root to a content hash for files,
// or to a marker naming its kind for directories and symlinks. Comparing two
// fingerprints is how "byte-exact" is decided here.
func fingerprint(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			t, _ := os.Readlink(p)
			out[rel] = "symlink:" + t
		case fi.IsDir():
			out[rel] = "dir"
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h := sha256.Sum256(b)
			out[rel] = hex.EncodeToString(h[:])
		}
		return nil
	})
	return out, err
}

// same reports whether two fingerprints agree, and names the first path they
// disagree on. Extra paths count as a difference: a restore that adds files it
// was never given is not byte-exact either.
func same(a, b map[string]string) (bool, string) {
	for k, v := range a {
		if b[k] != v {
			return false, "differs at " + k
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			return false, "extra " + k
		}
	}
	return true, ""
}

// seedTree lays down the small starting project every scenario begins from.
func seedTree(ws string, round int) {
	for i := 0; i < 6; i++ {
		p := filepath.Join(ws, fmt.Sprintf("d%d/f%d.txt", i%2, i))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(fmt.Sprintf("seed r%d f%d\n", round, i)), 0o644)
	}
}

// probeFeed asks a throwaway daemon whether the dirent change feed is available
// on this filesystem. Delete provenance depends on it, so the answer is
// recorded in the report rather than assumed.
func probeFeed(base string) bool {
	e, err := newEnv(base, "probe", 0)
	if err != nil {
		return false
	}
	if err := e.start(); err != nil {
		return false
	}
	defer e.stop()
	out, err := e.cli("status", "--json", "--root", e.ws, "--store", e.store)
	if err != nil {
		return false
	}
	var st struct {
		FeedActive bool `json:"feed_active"`
	}
	json.Unmarshal([]byte(out), &st)
	return st.FeedActive
}

// latestID is the newest checkpoint in a store, read straight from the manifest
// filenames so the harness never depends on how the CLI formats history.
func latestID(store string) int {
	ents, _ := os.ReadDir(filepath.Join(store, "manifests"))
	newest := 0
	for _, e := range ents {
		var id int
		if n, _ := fmt.Sscanf(e.Name(), "%d.json", &id); n == 1 && id > newest {
			newest = id
		}
	}
	return newest
}
