package selftest

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The names selftest reports, in the order it reports them. A beta tester's
// report is compared across machines, so this order is a contract.
var wantScenarios = []string{
	"protection-starts",
	"write-captured-and-restorable",
	"rm-rf-disaster-restore",
	"transient-salvage",
	"agent-undo-preserves-human",
	"agent-delete-undone",
	"secrets-never-captured",
}

// TestJSONListFieldsAreNeverNull pins the client contract: the JSON a tester
// mails back must be iterable without nil checks even for a report that never
// got off the ground.
func TestJSONListFieldsAreNeverNull(t *testing.T) {
	b, err := Report{}.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("selftest JSON does not parse: %v\n%s", err, b)
	}
	for _, k := range []string{"results", "env", "started_ns", "ended_ns"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("field %q missing from the JSON contract:\n%s", k, b)
		}
	}
	if string(raw["results"]) != "[]" {
		t.Fatalf("results must be an empty array, not %s", raw["results"])
	}
	if string(raw["env"]) != "{}" {
		t.Fatalf("env must be an empty object, not %s", raw["env"])
	}
}

// TestOKIsFalseOnlyForFail: a skipped guarantee is not a failure, but a failed
// one must never be reported as OK.
func TestOKIsFalseOnlyForFail(t *testing.T) {
	if !(Report{Results: []Result{{Status: StatusPass}, {Status: StatusSkip}}}).OK() {
		t.Fatal("a report of passes and skips must be OK")
	}
	if (Report{Results: []Result{{Status: StatusPass}, {Status: StatusFail}}}).OK() {
		t.Fatal("a report containing a failure must not be OK")
	}
}

// TestTextExplainsFailuresAndSkips: the text summary is what a user actually
// reads, so every failure and skip must carry its reason and the verdict must
// not read green when something failed.
func TestTextExplainsFailuresAndSkips(t *testing.T) {
	r := Report{
		Results: []Result{
			{Name: "protection-starts", Status: StatusPass, Detail: "setup checkpoint #1"},
			{Name: "agent-delete-undone", Status: StatusSkip, Detail: "no change feed on overlayfs"},
			{Name: "secrets-never-captured", Status: StatusFail, Detail: "marker found in objects/ab/cdef", Err: "exit status 1"},
		},
		Env:       map[string]string{"kernel": "6.12.0", "workspace_fs": "overlayfs"},
		StartedNS: 1000, EndedNS: 2_000_000_000,
	}
	txt := r.Text()
	for _, want := range []string{
		"PASS", "SKIP", "FAIL",
		"no change feed on overlayfs",
		"marker found in objects/ab/cdef",
		"exit status 1",
		"kernel", "6.12.0", "workspace_fs",
		"DID NOT HOLD",
	} {
		if !strings.Contains(txt, want) {
			t.Fatalf("selftest text is missing %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "every guarantee held") {
		t.Fatalf("a report with a failure must not read as a clean pass:\n%s", txt)
	}
}

// TestTextSaysNothingWasTestedWhenEverythingSkipped: an all-skip report is
// evidence of an untested machine, and must say so rather than looking healthy.
func TestTextSaysNothingWasTestedWhenEverythingSkipped(t *testing.T) {
	r := Report{Results: []Result{{Name: "a", Status: StatusSkip, Detail: "no capability"}}}
	if !strings.Contains(r.Text(), "nothing could be tested") {
		t.Fatalf("an all-skip report must say nothing was tested:\n%s", r.Text())
	}
}

// TestScanStoreSeesThroughCompression is the teeth of the secrets check: a
// search that only looked at plaintext would pass happily on a store full of
// compressed credentials.
func TestScanStoreSeesThroughCompression(t *testing.T) {
	dir := t.TempDir()
	const marker = "MARKER-abc123"

	objDir := filepath.Join(dir, "objects", "ab")
	if err := os.MkdirAll(objDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write([]byte("blob 20\x00secret=" + marker + "\n")); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	if err := os.WriteFile(filepath.Join(objDir, "cdef"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain.json"), []byte("nothing here"), 0o644); err != nil {
		t.Fatal(err)
	}

	hits, err := scanStore(dir, []byte(marker))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0], "cdef") {
		t.Fatalf("compressed marker not found; hits = %v", hits)
	}
	miss, err := scanStore(dir, []byte("MARKER-not-present"))
	if err != nil {
		t.Fatal(err)
	}
	if len(miss) != 0 {
		t.Fatalf("absent marker reported as found: %v", miss)
	}
}

// TestDiffFingerprintsCatchesModeAndSymlinkDrift: a restore that gets the bytes
// right and the exec bit wrong is not byte-exact, and the comparison must say
// so; this is what the rm -rf scenario asserts on.
func TestDiffFingerprintsCatchesModeAndSymlinkDrift(t *testing.T) {
	base := map[string]string{
		"a":  "file mode=0755 sha=aa",
		"l":  "symlink -> src/main.go",
		"d":  "dir mode=0755",
		"g":  "file mode=0644 sha=bb",
		"lo": "dir mode=0755",
	}
	same := map[string]string{}
	for k, v := range base {
		same[k] = v
	}
	if d := diffFingerprints(base, same); d != "" {
		t.Fatalf("identical trees reported as different:\n%s", d)
	}
	drift := map[string]string{}
	for k, v := range base {
		drift[k] = v
	}
	drift["a"] = "file mode=0644 sha=aa"     // exec bit lost
	drift["l"] = "symlink -> somewhere/else" // symlink retargeted
	delete(drift, "lo")                      // empty directory dropped
	drift["extra"] = "file mode=0644 sha=cc" // path the checkpoint never held
	d := diffFingerprints(base, drift)
	for _, want := range []string{"a", "0644", "l", "somewhere/else", "missing after restore: lo", "unexpected after restore: extra"} {
		if !strings.Contains(d, want) {
			t.Fatalf("diff missing %q:\n%s", want, d)
		}
	}
}

// TestRunRefusesAnUnusableBinary: selftest must fail with a sentence, never a
// panic or a hang, when handed something that is not the product.
func TestRunRefusesAnUnusableBinary(t *testing.T) {
	rep := Run(filepath.Join(t.TempDir(), "does-not-exist"), t.TempDir())
	if rep.OK() {
		t.Fatal("a missing binary must not produce an OK report")
	}
	if len(rep.Results) != 1 || rep.Results[0].Status != StatusFail {
		t.Fatalf("expected exactly one failure result, got %+v", rep.Results)
	}
	if rep.EndedNS < rep.StartedNS {
		t.Fatalf("timestamps are inverted: %d..%d", rep.StartedNS, rep.EndedNS)
	}
	if _, err := rep.JSON(); err != nil {
		t.Fatalf("a failed report must still serialize: %v", err)
	}
}

// TestRunAgainstTheRealBinary is the one that matters: build the product, point
// selftest at it, and check it drives real scenarios to real verdicts. It is
// exactly what a user runs, so it is exercised here rather than only by hand.
func TestRunAgainstTheRealBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("selftest drives a real daemon; skipped under -short")
	}
	bin := filepath.Join(t.TempDir(), "checkpoint")
	build := exec.Command("go", "build", "-o", bin, "github.com/manyapn/checkpoint-public/cmd/checkpoint")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building checkpoint: %v\n%s", err, out)
	}

	// Short scratch path: the daemon socket lives under the store.
	work, err := os.MkdirTemp("/tmp", "ckself")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)

	start := time.Now()
	rep := Run(bin, work)
	elapsed := time.Since(start)
	t.Logf("selftest took %s\n%s", elapsed.Round(time.Millisecond), rep.Text())

	if len(rep.Results) != len(wantScenarios) {
		t.Fatalf("expected %d scenarios, got %d: %+v", len(wantScenarios), len(rep.Results), rep.Results)
	}
	for i, want := range wantScenarios {
		if rep.Results[i].Name != want {
			t.Fatalf("scenario %d is %q, want %q (the report order is a contract)", i, rep.Results[i].Name, want)
		}
		if rep.Results[i].Detail == "" {
			t.Fatalf("scenario %q carries no detail; a verdict a user cannot act on is not a verdict", want)
		}
	}
	for _, k := range []string{"kernel", "arch", "workspace_fs", "store_fs", "feed_active", "root", "version", "store_path"} {
		if rep.Env[k] == "" {
			t.Fatalf("env is missing %q, and a bug report needs it: %v", k, rep.Env)
		}
	}
	// The JSON is the artifact a bug report attaches, so log it: `go test -v`
	// is the cheapest way to see exactly what a maintainer receives.
	blob, err := rep.JSON()
	if err != nil {
		t.Fatalf("report does not serialize: %v", err)
	}
	t.Logf("report JSON (this is what a tester sends back):\n%s", blob)

	// Nothing left running or listening: every daemon selftest started must be
	// stopped, or it would hold the caller's scratch directory forever.
	for _, store := range []string{
		filepath.Join(work, "stores", "primary"),
		filepath.Join(work, "stores", "disaster"),
	} {
		if _, err := os.Stat(filepath.Join(store, "daemon.sock")); err == nil {
			t.Errorf("selftest left a daemon socket behind at %s", store)
		}
	}

	if rep.Results[0].Status == StatusFail {
		t.Skipf("protection could not start in this environment, so the guarantees were not exercised:\n%s", rep.Results[0].Detail)
	}
	if !rep.OK() {
		var bad []string
		for _, r := range rep.Results {
			if r.Status == StatusFail {
				bad = append(bad, r.Name+": "+r.Detail+" "+r.Err)
			}
		}
		t.Fatalf("selftest reported failures against the real binary:\n%s", strings.Join(bad, "\n"))
	}
	if elapsed > 90*time.Second {
		t.Errorf("selftest took %s; it is meant to stay well under a minute", elapsed.Round(time.Second))
	}
}

// TestRunOnExt4ExercisesDeleteProvenance is the other half of the real-binary
// test. The devcontainer's native filesystem is overlayfs, where the change
// feed cannot arm, so agent-delete-undone is legitimately SKIPPED there, which
// means the machine that most people build on never exercises it. Provisioning
// a loopback ext4 proves both branches: that the scenario runs and passes where
// the feed is available, and that the skip above really is environment-scoped
// rather than a scenario that could never pass anywhere.
func TestRunOnExt4ExercisesDeleteProvenance(t *testing.T) {
	if testing.Short() {
		t.Skip("selftest drives a real daemon; skipped under -short")
	}
	mnt := ext4Mount(t)

	bin := filepath.Join(t.TempDir(), "checkpoint")
	build := exec.Command("go", "build", "-o", bin, "github.com/manyapn/checkpoint-public/cmd/checkpoint")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building checkpoint: %v\n%s", err, out)
	}

	work := filepath.Join(mnt, "w")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	rep := Run(bin, work)
	t.Logf("selftest on ext4:\n%s", rep.Text())

	if rep.Env["workspace_fs"] != "ext2/ext3/ext4" {
		t.Fatalf("expected the workspace on ext4, env says %q", rep.Env["workspace_fs"])
	}
	if rep.Env["feed_active"] != "true" {
		t.Skipf("the change feed did not arm on this loopback ext4 (feed_active=%q); the delete guarantee cannot be exercised here either",
			rep.Env["feed_active"])
	}
	var del Result
	for _, r := range rep.Results {
		if r.Name == "agent-delete-undone" {
			del = r
		}
	}
	if del.Status != StatusPass {
		t.Fatalf("with the change feed active, an agent-deleted file must be restored by undo; got %s: %s %s",
			del.Status, del.Detail, del.Err)
	}
	if !rep.OK() {
		var bad []string
		for _, r := range rep.Results {
			if r.Status == StatusFail {
				bad = append(bad, r.Name+": "+r.Detail+" "+r.Err)
			}
		}
		// A failure here is a failure of the PRODUCT, not of selftest, and it is
		// worth more than the overlayfs run: on a change-feed filesystem a
		// boundary FOLDS the recorded change-set instead of walking the tree, so
		// this is the only place that code path is exercised end to end. The two
		// paths must agree on everything user-visible, in particular that
		// credential-named files never enter the object store, which fold.go and
		// scanTree each have to enforce independently. Fix the product; never
		// weaken this assertion to make the suite green.
		t.Fatalf("selftest reported failures on ext4:\n%s", strings.Join(bad, "\n"))
	}
}

// ext4Mount provisions a loopback ext4 filesystem, mounted at a SHORT path
// (the daemon's socket lives under the store and must fit sockaddr_un).
// It skips when the environment cannot mount.
func ext4Mount(t *testing.T) string {
	t.Helper()
	img := filepath.Join(t.TempDir(), "ext4.img")
	if out, err := exec.Command("dd", "if=/dev/zero", "of="+img, "bs=1M", "count=128").CombinedOutput(); err != nil {
		t.Skipf("dd: %v: %s", err, out)
	}
	if out, err := exec.Command("mkfs.ext4", "-q", img).CombinedOutput(); err != nil {
		t.Skipf("mkfs.ext4: %v: %s", err, out)
	}
	mnt, err := os.MkdirTemp("/tmp", "ck4")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mount", "-o", "loop", img, mnt).CombinedOutput(); err != nil {
		os.RemoveAll(mnt)
		t.Skipf("cannot mount loopback ext4 (needs privilege): %v: %s", err, out)
	}
	// A daemon that has just released its socket may still be exiting, so the
	// first umount can report "target is busy". Retry briefly, then detach
	// lazily; leaving a loopback mount behind would leak a device on the
	// developer's machine every time this test fails.
	t.Cleanup(func() {
		for i := 0; i < 20; i++ {
			if out, err := exec.Command("umount", mnt).CombinedOutput(); err == nil {
				os.RemoveAll(mnt)
				return
			} else if i == 19 {
				t.Logf("umount %s still failing after retries (%s); detaching lazily", mnt, bytes.TrimSpace(out))
			}
			time.Sleep(250 * time.Millisecond)
		}
		exec.Command("umount", "-l", mnt).Run()
		os.RemoveAll(mnt)
	})
	return mnt
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd() // the package directory under the module root
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(dir, "..", ".."))
}
