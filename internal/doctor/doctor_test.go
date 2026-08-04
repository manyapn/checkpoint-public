package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// find returns the named check from a report (t.Fatal if absent, because the
// set of check names is part of this package's contract with the CLI).
func find(t *testing.T, r Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("report has no check named %q; got %v", name, names(r))
	return Check{}
}

func names(r Report) []string {
	var out []string
	for _, c := range r.Checks {
		out = append(out, c.Name)
	}
	return out
}

// TestHealthyIsFatalOnly pins the core semantics: Healthy answers "can
// checkpoint work at all", so ONLY a failed Fatal check makes a report
// unhealthy. A degraded-but-working machine (overlayfs, low disk) stays
// healthy; otherwise every container user would be told to give up.
func TestHealthyIsFatalOnly(t *testing.T) {
	allGood := Report{Checks: []Check{{Name: "a", OK: true, Fatal: true}, {Name: "b", OK: true}}}
	if !allGood.Healthy() {
		t.Fatal("all-passing report must be healthy")
	}
	warn := Report{Checks: []Check{
		{Name: "a", OK: true, Fatal: true},
		{Name: "b", OK: false, Fatal: false, Detail: "degraded"},
	}}
	if !warn.Healthy() {
		t.Fatal("a non-fatal failure must NOT make the report unhealthy")
	}
	fatal := Report{Checks: []Check{
		{Name: "a", OK: true, Fatal: true},
		{Name: "b", OK: false, Fatal: true, Detail: "no fanotify"},
		{Name: "c", OK: true},
	}}
	if fatal.Healthy() {
		t.Fatal("a failed fatal check must make the report unhealthy")
	}
	if (Report{}).Healthy() != true {
		t.Fatal("an empty report has no fatal failures; Healthy must not panic or invent one")
	}
}

// TestTextShowsRemediesForFailedChecksOnly pins the rendering contract: one
// line per check, remedies indented under the checks that FAILED, and a
// passing check's remedy (if any) never shown as an action item.
func TestTextShowsRemediesForFailedChecksOnly(t *testing.T) {
	r := Report{Checks: []Check{
		{Name: "kernel", OK: true, Detail: "Linux 6.12", Remedy: "unused-remedy-text"},
		{Name: "fanotify capability", OK: false, Fatal: true, Detail: "EPERM", Remedy: "run as root\nor setcap"},
		{Name: "workspace filesystem", OK: false, Detail: "overlayfs", Remedy: "bind-mount from the host"},
	}}
	out := r.Text()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	if strings.Contains(out, "unused-remedy-text") {
		t.Error("a PASSING check's remedy must not be printed")
	}
	if !strings.Contains(out, "remedy: run as root") || !strings.Contains(out, "remedy: or setcap") {
		t.Errorf("failed fatal check must print every remedy line; got:\n%s", out)
	}
	if !strings.Contains(out, "remedy: bind-mount from the host") {
		t.Errorf("failed non-fatal check must print its remedy; got:\n%s", out)
	}
	for _, l := range lines {
		if strings.Contains(l, "remedy:") && !strings.HasPrefix(l, "      ") {
			t.Errorf("remedy lines must be indented, got %q", l)
		}
	}
	// Each check gets exactly one status line, labelled by severity.
	if got := countPrefix(lines, "FAIL"); got != 1 {
		t.Errorf("want 1 FAIL line, got %d in:\n%s", got, out)
	}
	if got := countPrefix(lines, "WARN"); got != 1 {
		t.Errorf("want 1 WARN line, got %d in:\n%s", got, out)
	}
	if got := countPrefix(lines, "ok"); got != 1 {
		t.Errorf("want 1 ok line, got %d in:\n%s", got, out)
	}
	if !strings.Contains(out, "cannot protect this workspace") {
		t.Errorf("an unhealthy report's verdict must say checkpoint cannot protect; got:\n%s", out)
	}
}

func countPrefix(lines []string, prefix string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// TestTextVerdicts pins the three verdict sentences: a stranger must be able to
// read the LAST line and know whether to proceed.
func TestTextVerdicts(t *testing.T) {
	clean := Report{Checks: []Check{{Name: "kernel", OK: true, Detail: "fine"}}}
	if !strings.Contains(clean.Text(), "should work on this machine") {
		t.Errorf("clean verdict wrong:\n%s", clean.Text())
	}
	degraded := Report{Checks: []Check{
		{Name: "kernel", OK: true, Detail: "fine"},
		{Name: "workspace filesystem", OK: false, Detail: "overlayfs", Remedy: "none"},
	}}
	if !strings.Contains(degraded.Text(), "will work here, with 1 limitation") {
		t.Errorf("degraded verdict wrong:\n%s", degraded.Text())
	}
	broken := Report{Checks: []Check{
		{Name: "fanotify capability", OK: false, Fatal: true, Detail: "EPERM", Remedy: "sudo"},
		{Name: "store location", OK: false, Fatal: true, Detail: "in-tree", Remedy: "--store"},
	}}
	if !strings.Contains(broken.Text(), "2 blocking problems") {
		t.Errorf("fatal verdict wrong:\n%s", broken.Text())
	}
}

// TestRunNamesEveryCheckAndAlwaysActionable is the honesty invariant over a
// REAL Run: whatever this machine reports, no failing check is left without
// something the user can do about it, and the check set is stable.
func TestRunNamesEveryCheckAndAlwaysActionable(t *testing.T) {
	ws := t.TempDir()
	store := filepath.Join(t.TempDir(), "store")
	r := Run(ws, store)

	want := []string{"kernel", "fanotify capability", "workspace", "workspace filesystem",
		"store location", "store free space"}
	if len(r.Checks) != len(want) {
		t.Fatalf("check set changed: got %v, want %v", names(r), want)
	}
	for i, n := range want {
		if r.Checks[i].Name != n {
			t.Errorf("check %d: got %q, want %q", i, r.Checks[i].Name, n)
		}
	}
	for _, c := range r.Checks {
		if c.Detail == "" {
			t.Errorf("check %q has no Detail; every line must say what was found", c.Name)
		}
		if !c.OK && c.Remedy == "" {
			t.Errorf("check %q failed with no Remedy; a finding the user cannot act on is not shippable", c.Name)
		}
	}
	if t.Failed() {
		t.Logf("report was:\n%s", r.Text())
	}
}

// TestRunFreshWorkspaceAndStorePass pins the happy path on this machine: a
// normal temp workspace with an out-of-tree store must produce a clean
// workspace/store verdict. The fanotify probe is allowed to fail here (some
// CI containers lack CAP_SYS_ADMIN); that case is asserted separately.
func TestRunFreshWorkspaceAndStorePass(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "node_modules", "junk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "node_modules", "junk", "b.txt"), []byte("skipped"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Run(ws, filepath.Join(t.TempDir(), "store"))

	wsc := find(t, r, "workspace")
	if !wsc.OK {
		t.Fatalf("fresh workspace must pass: %+v", wsc)
	}
	if !strings.Contains(wsc.Detail, "1 file,") {
		t.Errorf("workspace detail should count the one countable file (node_modules is excluded like the store excludes it): %q", wsc.Detail)
	}
	if sl := find(t, r, "store location"); !sl.OK {
		t.Errorf("out-of-tree store must pass: %+v", sl)
	}
	if sp := find(t, r, "store free space"); !sp.OK {
		t.Logf("free space check failed (may be legitimate on a full disk): %+v", sp)
	} else if !strings.Contains(sp.Detail, "free of") {
		t.Errorf("free space detail must be plain language, got %q", sp.Detail)
	}
}

// TestRunRefusesInTreeStore pins the headline guarantee at preflight time: a
// store inside the workspace dies with `rm -rf project`, so it is FATAL and the
// remedy names an out-of-tree location.
func TestRunRefusesInTreeStore(t *testing.T) {
	ws := t.TempDir()
	r := Run(ws, filepath.Join(ws, ".checkpoint"))
	c := find(t, r, "store location")
	if c.OK || !c.Fatal {
		t.Fatalf("in-tree store must be a FATAL failure, got %+v", c)
	}
	if r.Healthy() {
		t.Fatal("a report with an in-tree store must not be Healthy")
	}
	if !strings.Contains(c.Remedy, "outside the project") {
		t.Errorf("remedy must tell the user where to put the store, got %q", c.Remedy)
	}
	if !strings.Contains(c.Detail, "rm -rf") && !strings.Contains(c.Remedy, "rm -rf") {
		t.Errorf("the reason (destroyed with the project) must be stated, got detail=%q remedy=%q", c.Detail, c.Remedy)
	}
}

// TestRunRefusesSymlinkWorkspace mirrors store.ErrSymlinkRoot semantics: a
// symlinked root would snapshot ZERO entries and still be badged recoverable,
// so preflight refuses it and names the resolved path.
func TestRunRefusesSymlinkWorkspace(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	r := Run(link, filepath.Join(t.TempDir(), "store"))
	c := find(t, r, "workspace")
	if c.OK || !c.Fatal {
		t.Fatalf("symlink workspace must be a FATAL failure, got %+v", c)
	}
	if !strings.Contains(c.Remedy, real) {
		t.Errorf("remedy must name the resolved path %q, got %q", real, c.Remedy)
	}
	if r.Healthy() {
		t.Fatal("symlink workspace must make the report unhealthy")
	}
	// The filesystem probe must not pretend it checked something.
	if fsc := find(t, r, "workspace filesystem"); fsc.OK || !strings.Contains(fsc.Detail, "not checked") {
		t.Errorf("filesystem probe must report that it did not run, got %+v", fsc)
	}
}

// TestRunMissingWorkspaceIsFatalWithMkdirRemedy: a stranger who mistypes the
// path gets the path back and a command, not a walk error.
func TestRunMissingWorkspaceIsFatalWithMkdirRemedy(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-project")
	r := Run(missing, filepath.Join(t.TempDir(), "store"))
	c := find(t, r, "workspace")
	if c.OK || !c.Fatal {
		t.Fatalf("missing workspace must be FATAL, got %+v", c)
	}
	if !strings.Contains(c.Detail, missing) || !strings.Contains(c.Remedy, "mkdir -p") {
		t.Errorf("detail must name the path and remedy must be runnable, got detail=%q remedy=%q", c.Detail, c.Remedy)
	}
}

// TestRunEmptyPathsDoNotPanic: the CLI may hand us empty strings; report them.
func TestRunEmptyPathsDoNotPanic(t *testing.T) {
	r := Run("", "")
	if find(t, r, "workspace").OK {
		t.Error("empty workspace must fail")
	}
	if find(t, r, "store location").OK {
		t.Error("empty store must fail")
	}
	if r.Healthy() {
		t.Error("empty paths must make the report unhealthy")
	}
	if !strings.Contains(find(t, r, "store free space").Detail, "not checked") {
		t.Error("free space must say it was not checked rather than reporting zero")
	}
}

// TestStoreLocationUnderMissingParentsReportsCreation: the default store path
// usually does not exist yet. That is fine and must read as fine, provided a
// writable ancestor exists.
func TestStoreLocationUnderMissingParentsReportsCreation(t *testing.T) {
	base := t.TempDir()
	deep := filepath.Join(base, "a", "b", "c", "store")
	c := storeLocationCheck(deep, nil, t.TempDir(), true)
	if !c.OK {
		t.Fatalf("a not-yet-created store under a writable ancestor must pass: %+v", c)
	}
	if !strings.Contains(c.Detail, "will be created") || !strings.Contains(c.Detail, base) {
		t.Errorf("detail must explain it will be created and name the existing ancestor, got %q", c.Detail)
	}
}

// TestStoreLocationUnwritableIsFatal proves writability by writing, which is
// the only way to catch read-only mounts and ownership problems.
func TestStoreLocationUnwritableIsFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny root, so unwritability cannot be staged honestly here")
	}
	base := t.TempDir()
	ro := filepath.Join(base, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	c := storeLocationCheck(filepath.Join(ro, "store"), nil, t.TempDir(), true)
	if c.OK || !c.Fatal {
		t.Fatalf("unwritable store parent must be FATAL, got %+v", c)
	}
	if !strings.Contains(c.Detail, "not writable") || c.Remedy == "" {
		t.Errorf("must name the problem and a remedy, got %+v", c)
	}
}

// TestWorkspaceWalkIsBoundedAndSaysSo: a preflight check must never hang on a
// huge tree, and a capped count must never be presented as a total.
func TestWorkspaceWalkIsBoundedAndSaysSo(t *testing.T) {
	ws := t.TempDir()
	for i := 0; i < 20; i++ {
		if err := os.WriteFile(filepath.Join(ws, string(rune('a'+i))+".txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := maxWalkEntries
	maxWalkEntries = 5
	defer func() { maxWalkEntries = old }()

	c := workspaceCheck(ws, nil)
	if !c.OK {
		t.Fatalf("a capped walk is not a failure: %+v", c)
	}
	if !strings.Contains(c.Detail, "at least") || !strings.Contains(c.Detail, "bigger") {
		t.Errorf("a capped count must be labelled a lower bound, got %q", c.Detail)
	}
	files, _, _, capped := walkSize(ws)
	if !capped {
		t.Error("walkSize must report capped=true when it stopped early")
	}
	if files > maxWalkEntries+1 {
		t.Errorf("walk did not stop near the cap: counted %d with cap %d", files, maxWalkEntries)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 bytes"},
		{512, "512 bytes"},
		{1500, "1.5 kB"},
		{2_000_000, "2.0 MB"},
		{3_400_000_000, "3.4 GB"},
		{5_000_000_000_000, "5.0 TB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCommas(t *testing.T) {
	for in, want := range map[int]string{0: "0", 42: "42", 999: "999", 1000: "1,000", 1234567: "1,234,567"} {
		if got := commas(in); got != want {
			t.Errorf("commas(%d) = %q, want %q", in, got, want)
		}
	}
}
