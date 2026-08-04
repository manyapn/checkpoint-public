// Storage accounting: what the store costs on disk, and what a prune would give
// back. The pinned contract is honesty: every byte figure is a real on-disk
// byte, an empty store is zeroes rather than an error, an unparsable manifest
// costs bytes but is never counted as a checkpoint, and the reclaim estimate is
// never larger than what a real prune actually frees.
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

// fileSize is the test's own view of a file's on-disk size, deliberately a
// plain stat, so expectations are computed from the disk rather than from the
// product's accounting code.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

// TestMeasureUsageEmptyStore: a store that does not exist, an empty directory,
// and a store with identity but no checkpoints are all zeroes and a nil error.
// "Nothing stored yet" is a fact a status command must be able to print, not a
// failure it has to explain.
func TestMeasureUsageEmptyStore(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created")
	u, err := MeasureUsage(missing)
	if err != nil {
		t.Fatalf("a missing store must not be an error: %v", err)
	}
	if (u != Usage{}) {
		t.Fatalf("missing store must measure as zeroes, got %+v", u)
	}

	empty := t.TempDir()
	u, err = MeasureUsage(empty)
	if err != nil {
		t.Fatal(err)
	}
	if (u != Usage{}) {
		t.Fatalf("empty store must measure as zeroes, got %+v", u)
	}
	if got := u.Human(); got != "empty (nothing stored yet)" {
		t.Fatalf("empty store renders %q", got)
	}

	// Identity stamped, nothing checkpointed: bytes on disk, no checkpoints.
	stamped := t.TempDir()
	if _, err := EnsureMeta(stamped, "/tmp/some-workspace"); err != nil {
		t.Fatal(err)
	}
	if _, err := objstore.Open(stamped); err != nil {
		t.Fatal(err)
	}
	u, err = MeasureUsage(stamped)
	if err != nil {
		t.Fatal(err)
	}
	if u.Manifests != 0 || u.Objects != 0 || u.ObjectBytes != 0 {
		t.Fatalf("a stamped-but-empty store holds no checkpoints or objects, got %+v", u)
	}
	if want := fileSize(t, filepath.Join(stamped, "store.json")); u.TotalBytes != want {
		t.Fatalf("TotalBytes must count store.json (%d bytes), got %d", want, u.TotalBytes)
	}
	if u.OldestNS != 0 || u.NewestNS != 0 {
		t.Fatalf("no checkpoints means no age window, got oldest=%d newest=%d", u.OldestNS, u.NewestNS)
	}
}

// TestMeasureUsagePopulatedStore: counts and bytes on a real store, including
// the two things that must NOT inflate the counts: a tmp- file left by a
// crashed Put and a torn manifest. Both still cost disk, so both are inside
// TotalBytes; neither is a stored object or a restorable checkpoint.
func TestMeasureUsagePopulatedStore(t *testing.T) {
	storeDir := t.TempDir()
	oc, err := objstore.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureMeta(storeDir, "/tmp/some-workspace"); err != nil {
		t.Fatal(err)
	}

	contents := []string{"alpha\n", "beta\n", strings.Repeat("gamma\n", 500)}
	var refs []string
	var wantObjectBytes int64
	for _, c := range contents {
		ref, _, err := oc.Put([]byte(c))
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
		sz, err := oc.Size(ref)
		if err != nil {
			t.Fatal(err)
		}
		wantObjectBytes += sz
	}
	// A duplicate Put must not add an object: dedup is why the store is small.
	if _, isNew, err := oc.Put([]byte(contents[0])); err != nil || isNew {
		t.Fatalf("duplicate Put should dedup (isNew=%v err=%v)", isNew, err)
	}

	now := time.Now().UnixNano()
	manifests := []*Manifest{
		{ID: 0, TimeNS: now - 6*dayNS, Coverage: DURABLE, Entries: map[string]Entry{"a": {Kind: KindFile, Ref: refs[0]}}},
		{ID: 1, TimeNS: now - 2*dayNS, Coverage: DURABLE, Name: "before-refactor", Entries: map[string]Entry{"b": {Kind: KindFile, Ref: refs[1]}}},
		{ID: 2, TimeNS: now, Coverage: DURABLE, Entries: map[string]Entry{"c": {Kind: KindFile, Ref: refs[2]}}},
	}
	for _, m := range manifests {
		if err := Write(storeDir, m); err != nil {
			t.Fatal(err)
		}
	}
	seedVersionLog(t, storeDir, versionlog.Version{Op: versionlog.OpCreate, Path: "/w/a", Ref: refs[0], TimeNS: now})

	// Debris: a crashed Put's tmp file, and a torn manifest.
	tmpObj := filepath.Join(storeDir, "objects", refs[0][:2], "tmp-halfwritten")
	if err := os.WriteFile(tmpObj, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	tornManifest := filepath.Join(storeDir, "manifests", "9.json")
	if err := os.WriteFile(tornManifest, []byte(`{"id":9,"entries":`), 0o600); err != nil {
		t.Fatal(err)
	}

	u, err := MeasureUsage(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if u.Objects != len(refs) {
		t.Fatalf("Objects: got %d, want %d (a tmp- file is not a stored object)", u.Objects, len(refs))
	}
	if u.ObjectBytes != wantObjectBytes {
		t.Fatalf("ObjectBytes: got %d, want %d (compressed on-disk bytes)", u.ObjectBytes, wantObjectBytes)
	}
	if u.Manifests != len(manifests) {
		t.Fatalf("Manifests: got %d, want %d (a torn manifest is not a checkpoint)", u.Manifests, len(manifests))
	}
	if u.NamedManifests != 1 {
		t.Fatalf("NamedManifests: got %d, want 1", u.NamedManifests)
	}
	if want := fileSize(t, filepath.Join(storeDir, "versionlog")); u.VersionLogBytes != want {
		t.Fatalf("VersionLogBytes: got %d, want %d", u.VersionLogBytes, want)
	}
	if u.OldestNS != manifests[0].TimeNS || u.NewestNS != manifests[2].TimeNS {
		t.Fatalf("age window: got [%d,%d], want [%d,%d]", u.OldestNS, u.NewestNS, manifests[0].TimeNS, manifests[2].TimeNS)
	}

	// TotalBytes is every regular file: the broken-out parts plus the manifest
	// files, store.json, and the debris. Computed here from the disk itself.
	want := wantObjectBytes + u.VersionLogBytes + fileSize(t, filepath.Join(storeDir, "store.json")) +
		fileSize(t, tmpObj) + fileSize(t, tornManifest)
	for _, m := range manifests {
		want += fileSize(t, filepath.Join(storeDir, "manifests", fmt.Sprintf("%d.json", m.ID)))
	}
	if u.TotalBytes != want {
		t.Fatalf("TotalBytes: got %d, want %d (sum of every regular file in the store)", u.TotalBytes, want)
	}
	if u.TotalBytes < u.ObjectBytes+u.VersionLogBytes {
		t.Fatalf("TotalBytes (%d) must never be smaller than the sum of its parts (%d)", u.TotalBytes, u.ObjectBytes+u.VersionLogBytes)
	}

	if got := u.Human(); !strings.Contains(got, "3 objects") || !strings.Contains(got, "3 checkpoints") ||
		!strings.Contains(got, "1 named") || !strings.Contains(got, "oldest 6 days ago") {
		t.Fatalf("Human() must summarize the real numbers, got %q", got)
	}
}

// TestEstimateReclaimMatchesRealPrune: the estimate is not a guess. It is
// measured against the real thing (measure, estimate, actually prune, measure
// again), and the deltas must agree exactly, field by field. This is the
// property that lets `prune --dry-run` be trusted before the destructive run.
func TestEstimateReclaimMatchesRealPrune(t *testing.T) {
	storeDir := t.TempDir()
	oc, err := objstore.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureMeta(storeDir, "/tmp/some-workspace"); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UnixNano()
	old := now - 30*dayNS
	put := func(s string) string {
		ref, _, err := oc.Put([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	refA := put(strings.Repeat("doomed content\n", 200)) // only manifest 0 + an expiring record
	refB := put("named checkpoint content\n")            // held by the named checkpoint
	refC := put("baseline content\n")                    // held by the latest durable baseline

	for _, m := range []*Manifest{
		{ID: 0, TimeNS: old, Coverage: DURABLE, Entries: map[string]Entry{"a": {Kind: KindFile, Ref: refA}}},
		{ID: 1, TimeNS: old + 1, Coverage: DURABLE, Name: "keep-me", Entries: map[string]Entry{"b": {Kind: KindFile, Ref: refB}}},
		{ID: 2, TimeNS: old + 2, Coverage: DURABLE, Entries: map[string]Entry{"c": {Kind: KindFile, Ref: refC}}},
	} {
		if err := Write(storeDir, m); err != nil {
			t.Fatal(err)
		}
	}
	seedVersionLog(t, storeDir,
		versionlog.Version{Op: versionlog.OpCreate, Path: "/w/a", Ref: refA, TimeNS: old - dayNS}, // expires
		versionlog.Version{Op: versionlog.OpModify, Path: "/w/c", Ref: refC, TimeNS: now},         // survives
	)

	before, err := MeasureUsage(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	est, err := EstimateReclaim(storeDir, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if est.NamedManifests != 0 {
		t.Fatalf("prune never removes a named checkpoint; estimate claims %d", est.NamedManifests)
	}
	if est.Manifests != 1 || est.Objects != 1 {
		t.Fatalf("expected to reclaim exactly manifest 0 and object A, got %+v", est)
	}
	if est.OldestNS != old || est.NewestNS != old {
		t.Fatalf("the estimate's age window must describe the checkpoints that would go, got [%d,%d] want [%d,%d]", est.OldestNS, est.NewestNS, old, old)
	}

	// Nothing may exceed what is actually stored.
	assertReclaimBounded(t, est, before)

	rep, err := Prune(storeDir, oc, PruneOpts{KeepDays: 7, NowNS: now})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(rep.RemovedManifests) != 1 || rep.RemovedObjects != 1 || rep.ExpiredVersions != 1 {
		t.Fatalf("prune did not do what the estimate described: %+v", rep)
	}

	after, err := MeasureUsage(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := before.Objects-after.Objects, est.Objects; got != want {
		t.Fatalf("objects freed: prune freed %d, estimate said %d", got, want)
	}
	if got, want := before.ObjectBytes-after.ObjectBytes, est.ObjectBytes; got != want {
		t.Fatalf("object bytes freed: prune freed %d, estimate said %d", got, want)
	}
	if got, want := before.Manifests-after.Manifests, est.Manifests; got != want {
		t.Fatalf("checkpoints freed: prune freed %d, estimate said %d", got, want)
	}
	if got, want := before.VersionLogBytes-after.VersionLogBytes, est.VersionLogBytes; got != want {
		t.Fatalf("versionlog bytes freed: prune freed %d, estimate said %d", got, want)
	}
	if got, want := before.TotalBytes-after.TotalBytes, est.TotalBytes; got != want {
		t.Fatalf("total bytes freed: prune freed %d, estimate said %d", got, want)
	}
	if est.TotalBytes <= 0 {
		t.Fatalf("this fixture must reclaim something; estimate was %d bytes", est.TotalBytes)
	}
}

// TestEstimateReclaimNeverExceedsUsage: across keep-days settings, including 0
// (the most aggressive prune the CLI can ask for), the estimate stays bounded by
// what is actually stored, and a store with nothing old enough reclaims nothing.
// A missing store estimates zero rather than erroring.
func TestEstimateReclaimNeverExceedsUsage(t *testing.T) {
	storeDir := t.TempDir()
	oc, err := objstore.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	for i, body := range []string{"one\n", "two\n", "three\n", "four\n"} {
		ref, _, err := oc.Put([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		m := &Manifest{ID: i, TimeNS: now - int64(i)*dayNS, Coverage: DURABLE,
			Entries: map[string]Entry{"f": {Kind: KindFile, Ref: ref}}}
		if i == 1 {
			m.Name = "keep"
		}
		if err := Write(storeDir, m); err != nil {
			t.Fatal(err)
		}
	}
	seedVersionLog(t, storeDir, versionlog.Version{Op: versionlog.OpCreate, Path: "/w/f", TimeNS: now - 10*dayNS})

	cur, err := MeasureUsage(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, keepDays := range []int{0, 1, 2, 7, 3650} {
		est, err := EstimateReclaim(storeDir, keepDays, now)
		if err != nil {
			t.Fatalf("keepDays=%d: %v", keepDays, err)
		}
		assertReclaimBounded(t, est, cur)
	}

	// Every checkpoint is younger than 30 days, so a 30-day retention removes no
	// checkpoint and GCs no object. It is NOT a no-op though: the ledger still
	// compacts to the oldest retained checkpoint, so the pre-history record here
	// expires. The estimate must say that, and must match the real prune.
	est, err := EstimateReclaim(storeDir, 30, now)
	if err != nil {
		t.Fatal(err)
	}
	if est.Manifests != 0 || est.Objects != 0 {
		t.Fatalf("nothing is older than 30 days; no checkpoint or object may be reclaimed, got %+v", est)
	}
	if est.VersionLogBytes == 0 {
		t.Fatal("a ledger record older than every retained checkpoint expires; estimate claimed 0 bytes")
	}
	if _, err := Prune(storeDir, oc, PruneOpts{KeepDays: 30, NowNS: now}); err != nil {
		t.Fatal(err)
	}
	afterPrune, err := MeasureUsage(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cur.TotalBytes - afterPrune.TotalBytes; got != est.TotalBytes {
		t.Fatalf("prune freed %d bytes, estimate said %d", got, est.TotalBytes)
	}

	missing := filepath.Join(t.TempDir(), "never-created")
	if est, err := EstimateReclaim(missing, 7, now); err != nil {
		t.Fatalf("a missing store must estimate zero, not error: %v", err)
	} else if (est != Usage{}) {
		t.Fatalf("missing store estimate: got %+v, want zeroes", est)
	}
}

func assertReclaimBounded(t *testing.T, est, cur Usage) {
	t.Helper()
	if est.Objects > cur.Objects || est.ObjectBytes > cur.ObjectBytes {
		t.Fatalf("estimate frees more objects than exist: est %+v, current %+v", est, cur)
	}
	if est.Manifests > cur.Manifests || est.NamedManifests > cur.NamedManifests {
		t.Fatalf("estimate frees more checkpoints than exist: est %+v, current %+v", est, cur)
	}
	if est.VersionLogBytes > cur.VersionLogBytes {
		t.Fatalf("estimate frees %d versionlog bytes but only %d exist", est.VersionLogBytes, cur.VersionLogBytes)
	}
	if est.TotalBytes > cur.TotalBytes {
		t.Fatalf("estimate frees %d bytes but the store only holds %d", est.TotalBytes, cur.TotalBytes)
	}
}

// TestUsageHumanRendering: the one line a stranger reads. Zero, singular, and
// plural all have to be grammatical: "1 objects and 1 checkpoints" is the kind
// of detail that makes a tool feel unfinished.
func TestUsageHumanRendering(t *testing.T) {
	now := time.Now().UnixNano()
	cases := []struct {
		name string
		u    Usage
		want string
	}{
		{"zero", Usage{}, "empty (nothing stored yet)"},
		{
			"singular",
			Usage{Objects: 1, ObjectBytes: 1000, Manifests: 1, TotalBytes: 1234, OldestNS: now - 25*int64(time.Hour), NewestNS: now},
			"1.2 kB across 1 object and 1 checkpoint (oldest 1 day ago)",
		},
		{
			"plural",
			Usage{Objects: 1203, Manifests: 37, NamedManifests: 3, TotalBytes: 412_000_000, OldestNS: now - 6*dayNS, NewestNS: now},
			"412 MB across 1,203 objects and 37 checkpoints (3 named, oldest 6 days ago)",
		},
		{
			"no checkpoints yet but bytes on disk",
			Usage{Objects: 2, TotalBytes: 500},
			"500 B across 2 objects and 0 checkpoints",
		},
		{
			"fresh store, checkpoint seconds old",
			Usage{Objects: 4, Manifests: 1, TotalBytes: 4096, OldestNS: now - int64(3*time.Second), NewestNS: now},
			"4.1 kB across 4 objects and 1 checkpoint (oldest just now)",
		},
	}
	for _, tc := range cases {
		if got := tc.u.Human(); got != tc.want {
			t.Errorf("%s: Human() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestHumanBytesAndAge pins the two renderers Human() leans on, including the
// boundaries where a unit rolls over and the clock-skew case (a checkpoint
// stamped in the future must not render as a negative age).
func TestHumanBytesAndAge(t *testing.T) {
	bytes := []struct {
		n    int64
		want string
	}{
		{0, "0 B"}, {1, "1 B"}, {999, "999 B"}, {1000, "1.0 kB"}, {1500, "1.5 kB"},
		{999_999, "1000 kB"}, {1_000_000, "1.0 MB"}, {412_000_000, "412 MB"},
		{9_500_000_000, "9.5 GB"}, {2_500_000_000_000, "2.5 TB"},
	}
	for _, c := range bytes {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
	ages := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "just now"}, {30 * time.Second, "just now"},
		{time.Minute, "1 minute ago"}, {90 * time.Second, "1 minute ago"},
		{5 * time.Minute, "5 minutes ago"}, {time.Hour, "1 hour ago"},
		{23 * time.Hour, "23 hours ago"}, {24 * time.Hour, "1 day ago"},
		{6 * 24 * time.Hour, "6 days ago"},
	}
	for _, c := range ages {
		if got := humanAge(int64(c.d)); got != c.want {
			t.Errorf("humanAge(%s) = %q, want %q", c.d, got, c.want)
		}
	}
	if got := commas(1234567); got != "1,234,567" {
		t.Errorf("commas(1234567) = %q", got)
	}
}
