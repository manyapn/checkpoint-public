//go:build linux

// Adversarial provenance tests. The contract under attack: nothing may classify
// Agent unless the writer's chain PROVABLY terminates at a registered agent
// root. Every staged failure (hostile comm, depth-bound overflow, vanished
// interiors, garbage stat content, recycled pids) must land on
// anything-but-Agent; and correct parses of hostile-but-valid input must NOT
// decay legitimate descendants to Unknown.
package provenance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// deadPid returns a pid that is genuinely dead and unreadable in /proc,
// skipping the test if the pid was recycled before we could confirm.
func deadPid(t *testing.T) int {
	t.Helper()
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	pid := c.Process.Pid
	if _, _, ok := readStat(pid); ok {
		t.Skipf("pid %d still readable after reaping (recycled?); cannot stage a vanished process", pid)
	}
	return pid
}

// seedChain seeds tr's birth cache with a synthetic parent chain of the given
// depth: writer = base, each pid's parent the next pid up, the last entry the
// intended terminal. Base pids start above PID_MAX_LIMIT (4194304) so no live
// /proc entry can ever contradict the cache. Returns the writer pid and the
// terminal's identity.
func seedChain(tr *Tracker, base, depth int) (int, Identity) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for i := 0; i < depth; i++ {
		tr.cache[base+i] = procInfo{start: uint64(90000 + i), ppid: base + i + 1}
	}
	last := base + depth - 1
	return base, Identity{Pid: last, Start: uint64(90000 + depth - 1)}
}

// TestReadStatHostileComm: a writer whose comm contains spaces, parens, and
// digits (so /proc/<pid>/stat renders `<pid> (b) 12 (3 x) S <ppid> ...`) must
// still parse ppid and starttime correctly (fields are located after the LAST
// ')'), and the full lineage machinery must still classify its writes. The
// hostile comm is staged by exec'ing a symlink whose basename is the comm.
func TestReadStatHostileComm(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary on PATH")
	}
	link := filepath.Join(t.TempDir(), "b) 12 (3 x")
	if err := os.Symlink(sleepPath, link); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(link, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	pid := cmd.Process.Pid

	// Guard the staging itself: the kernel must actually have taken the
	// hostile basename as comm, or the test would prove nothing.
	comm, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSuffix(string(comm), "\n"); !strings.Contains(got, ")") || !strings.Contains(got, " ") {
		t.Skipf("kernel did not adopt hostile comm (got %q); cannot exercise the parse", got)
	}

	start, ppid, ok := readStat(pid)
	if !ok {
		t.Fatal("readStat failed on a live process with hostile comm")
	}
	if ppid != os.Getpid() {
		t.Fatalf("hostile comm shifted the stat fields: parsed ppid %d, want %d", ppid, os.Getpid())
	}
	if start == 0 {
		t.Fatal("hostile comm produced starttime 0")
	}

	// Correct parse end-to-end: this is a legitimate descendant of the
	// registered root and must classify Agent. A mis-parse would surface as
	// Unknown, and over-conservatism for real descendants is also a defect.
	self := selfIdentity(t)
	tr := NewTracker()
	chain, resolved := tr.Lineage(pid, map[Identity]bool{self: true})
	if !resolved {
		t.Fatal("lineage of hostile-comm child should resolve")
	}
	if got := Classify(chain, resolved, map[Identity]bool{self: true}); got != Agent {
		t.Fatalf("hostile-comm descendant of the root must classify Agent, got %s (chain %v)", got, chain)
	}
}

// TestLineageDepthBoundIsUnknown: a 129-link chain exhausts the walk's 128-step
// bound before reaching its terminal. Even though the chain truly ends at the
// registered agent root, an over-deep walk must come back unresolved and
// classify Unknown (the Classify contract for unresolved chains), never Agent.
// The companion case pins the boundary: exactly 128 links, terminal included,
// must still resolve to Agent so the bound cannot eat legitimate deep trees.
func TestLineageDepthBoundIsUnknown(t *testing.T) {
	tr := NewTracker()
	writer, root := seedChain(tr, 100000000, 129)
	roots := map[Identity]bool{root: true}
	chain, resolved := tr.Lineage(writer, roots)
	if resolved || chain != nil {
		t.Fatalf("129-deep chain must not resolve: resolved=%v chain len %d", resolved, len(chain))
	}
	if got := Classify(chain, resolved, roots); got != Unknown {
		t.Fatalf("beyond-bound chain must classify Unknown, got %s", got)
	}

	tr2 := NewTracker()
	writer2, root2 := seedChain(tr2, 110000000, 128)
	roots2 := map[Identity]bool{root2: true}
	chain2, resolved2 := tr2.Lineage(writer2, roots2)
	if !resolved2 || len(chain2) != 128 {
		t.Fatalf("exactly-at-bound chain should resolve fully: resolved=%v len %d", resolved2, len(chain2))
	}
	if got := Classify(chain2, resolved2, roots2); got != Agent {
		t.Fatalf("at-bound descendant of the root must remain Agent, got %s", got)
	}
}

// TestVanishedMidWalkIsUnknown: the writer is cached, but its birth-parent is
// dead and was never cached, so the walk dies mid-chain. Per the Lineage contract
// that is (nil, false), never a partial chain dressed as complete, and Classify
// on an unresolved chain is Unknown (contractual), never Agent.
func TestVanishedMidWalkIsUnknown(t *testing.T) {
	dead := deadPid(t)
	tr := NewTracker()
	const writer = 100000001 // above PID_MAX_LIMIT: the cache is its only source
	tr.mu.Lock()
	tr.cache[writer] = procInfo{start: 4242, ppid: dead}
	tr.mu.Unlock()

	self := selfIdentity(t)
	roots := map[Identity]bool{self: true}
	chain, resolved := tr.Lineage(writer, roots)
	if resolved || chain != nil {
		t.Fatalf("chain through a vanished, uncached interior must not resolve (chain %v)", chain)
	}
	if got := Classify(chain, resolved, roots); got != Unknown {
		t.Fatalf("vanished mid-walk must classify Unknown (never Agent), got %s", got)
	}
}

// TestGarbageStatNeverAgent: truncated or garbage stat content must yield
// ok=false from the parser, and the resulting resolution failure must end at
// Unknown, never Agent. Also pins that a hostile-but-WELL-FORMED line parses
// to the right fields (everything before the last ')' is comm).
func TestGarbageStatNeverAgent(t *testing.T) {
	garbage := []struct{ name, in string }{
		{"empty", ""},
		{"no close paren", "123 (comm R 42 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 777"},
		{"nothing after comm", "123 (comm)"},
		{"truncated fields", "123 (comm) R 42 1 1 0 -1"},
		{"non-numeric ppid", "123 (comm) R zz 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 777"},
		{"negative ppid", "123 (comm) R -5 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 777"},
		{"non-numeric starttime", "123 (comm) R 42 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 xx"},
		{"negative starttime", "123 (comm) R 42 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 -777"},
		{"binary garbage", "\x00\xff\x7f)("},
	}
	for _, c := range garbage {
		if start, ppid, ok := parseStat([]byte(c.in)); ok {
			t.Errorf("%s: garbage stat parsed as ok (start=%d ppid=%d): %q", c.name, start, ppid, c.in)
		}
	}

	// Hostile-but-valid: comm holding parens, spaces, and digits must not shift
	// the fields: ppid 42, starttime 777.
	start, ppid, ok := parseStat([]byte("123 (a) b (c 12) R 42 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 777 0"))
	if !ok || ppid != 42 || start != 777 {
		t.Fatalf("hostile-but-valid stat mis-parsed: ok=%v ppid=%d start=%d (want ok, 42, 777)", ok, ppid, start)
	}

	// The live ok=false path: a writer that exited between the event and
	// resolution, never cached. The walk fails and must never yield Agent.
	dead := deadPid(t)
	tr := NewTracker()
	self := selfIdentity(t)
	roots := map[Identity]bool{self: true}
	chain, resolved := tr.Lineage(dead, roots)
	if resolved {
		t.Fatalf("dead uncached writer must not resolve, got chain %v", chain)
	}
	if got := Classify(chain, resolved, roots); got == Agent {
		t.Fatal("dead uncached writer classified Agent")
	}
}

// TestRecycledPidStartTimeMismatchNeverAgent (tracker level): the registered
// root identity carries a start-time that does not match the live process at
// that pid, so the registration is stale (the root died, its pid was recycled).
// A real descendant of the LIVE process must not ride the bare pid match into
// Agent. Human is the correct verdict here (the chain resolves cleanly outside
// every registered root), but the assertion is only what the contract demands:
// not Agent.
func TestRecycledPidStartTimeMismatchNeverAgent(t *testing.T) {
	realStart, ok := StartTime(os.Getpid())
	if !ok {
		t.Fatal("cannot read own start-time")
	}
	staleRoot := Identity{Pid: os.Getpid(), Start: realStart + 777}
	roots := map[Identity]bool{staleRoot: true}

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	tr := NewTracker()
	chain, resolved := tr.Lineage(cmd.Process.Pid, roots)
	if got := Classify(chain, resolved, roots); got == Agent {
		t.Fatalf("stale root registration (pid match, start mismatch) must never yield Agent (chain %v, resolved %v)", chain, resolved)
	}
}

// TestStaleWriterCacheLiveMismatchNeverAgent stages the inverse recycling: the
// birth cache holds a DEAD predecessor of the writer's pid, recorded as a
// child of the agent root, while the pid's LIVE occupant is a different,
// non-descendant process. This state is reachable in a long-lived daemon: the
// scanner caches each pid at first sighting and never evicts, so once the pid
// wraps and is reused, the cache describes the corpse. Walking the corpse's
// ancestry would credit the live stranger's write to the agent, the one
// direction that must never happen.
func TestStaleWriterCacheLiveMismatchNeverAgent(t *testing.T) {
	// A real, live process registered as the agent root.
	rootCmd := exec.Command("sleep", "30")
	if err := rootCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { rootCmd.Process.Kill(); rootCmd.Wait() }()
	rootStart, ok := StartTime(rootCmd.Process.Pid)
	if !ok {
		t.Fatal("cannot read root start-time")
	}
	roots := map[Identity]bool{{Pid: rootCmd.Process.Pid, Start: rootStart}: true}

	// The live occupant of the writer pid: NOT descended from the root.
	writerCmd := exec.Command("sleep", "30")
	if err := writerCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { writerCmd.Process.Kill(); writerCmd.Wait() }()
	writerPid := writerCmd.Process.Pid
	liveStart, ok := StartTime(writerPid)
	if !ok {
		t.Fatal("cannot read writer start-time")
	}

	// Stage the stale cache: the pid's dead predecessor, born of the agent root.
	tr := NewTracker()
	tr.mu.Lock()
	tr.cache[writerPid] = procInfo{start: liveStart + 999, ppid: rootCmd.Process.Pid}
	tr.mu.Unlock()

	chain, resolved := tr.Lineage(writerPid, roots)
	if got := Classify(chain, resolved, roots); got == Agent {
		t.Fatalf("live pid contradicting the birth cache must never classify Agent (chain %v, resolved %v)", chain, resolved)
	}
}

// TestIsSelfWriteOnlyMatchesOwnBinary: self-attribution must be exact. Our own
// pid matches; an unrelated live process (a shell) must not, or a human's
// writes could be laundered into "checkpoint's own" and silently reverted.
func TestIsSelfWriteOnlyMatchesOwnBinary(t *testing.T) {
	if !IsSelfWrite(os.Getpid()) {
		t.Fatal("this process runs the test binary itself and must be self")
	}
	other := exec.Command("sleep", "5")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { other.Process.Kill(); other.Wait() }()
	if IsSelfWrite(other.Process.Pid) {
		t.Fatal("an unrelated binary must never be attributed to checkpoint itself")
	}
	if IsSelfWrite(0) || IsSelfWrite(-1) || IsSelfWrite(1<<30) {
		t.Fatal("invalid or dead pids must never be self (uncertainty is not ownership)")
	}
}
