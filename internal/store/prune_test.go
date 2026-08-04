// History & retention, store half: the prune engine. Pinned contract:
// candidates are UNNAMED manifests older than KeepDays, never the latest
// DURABLE baseline and never a named one; versionlog records older than the
// oldest retained manifest expire; GC then removes exactly the objects no
// remaining manifest (Entries + Extra) and no remaining versionlog record
// references; DryRun computes the full report and deletes nothing.
package store

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

const dayNS = int64(24 * time.Hour)

// bareManifest builds a minimal manifest for age/name retention tests (no
// entries: retention rules must not depend on content).
func bareManifest(id int, timeNS int64, cov Coverage, name string) *Manifest {
	return &Manifest{ID: id, TimeNS: timeNS, Coverage: cov, Name: name, Entries: map[string]Entry{}}
}

// seedVersionLog writes recs to the store's version log and releases the lock
// (Prune requires exclusive store access: no daemon, no open log).
func seedVersionLog(t *testing.T, storeDir string, recs ...versionlog.Version) {
	t.Helper()
	l, err := versionlog.Open(filepath.Join(storeDir, "versionlog"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

// sortedInts returns a sorted copy (nil in, nil out) so report assertions do
// not pin an ordering the contract never promised.
func sortedInts(xs []int) []int {
	if len(xs) == 0 {
		return nil
	}
	out := make([]int, len(xs))
	copy(out, xs)
	sort.Ints(out)
	return out
}

func normalizeReport(r PruneReport) PruneReport {
	r.RemovedManifests = sortedInts(r.RemovedManifests)
	r.KeptNamed = sortedInts(r.KeptNamed)
	return r
}

// TestPruneKeepsNamedAndLatestBaseline: of five manifests all older than
// KeepDays, prune removes only the unnamed non-baseline ones. The named
// manifest survives (and is reported under KeptNamed), and the latest DURABLE
// manifest survives regardless of age (it is the incremental/undo baseline).
// PARTIAL manifests are prunable like unnamed ones and never count as baseline.
func TestPruneKeepsNamedAndLatestBaseline(t *testing.T) {
	storeDir := t.TempDir()
	oc := mustObj(t, storeDir)
	now := time.Now().UnixNano()
	old := now - 30*dayNS

	for _, m := range []*Manifest{
		bareManifest(0, old, DURABLE, ""),          // unnamed, old: removed
		bareManifest(1, old+1, DURABLE, "keep-me"), // named, old: kept, reported
		bareManifest(2, old+2, PARTIAL, ""),        // PARTIAL: prunable like unnamed
		bareManifest(3, old+3, DURABLE, ""),        // latest DURABLE: kept despite age
		bareManifest(4, old+4, PARTIAL, ""),        // newer than 3 but PARTIAL: no baseline claim
	} {
		if err := Write(storeDir, m); err != nil {
			t.Fatal(err)
		}
	}
	seedVersionLog(t, storeDir) // a quiet store still has a (possibly empty) log

	rep, err := Prune(storeDir, oc, PruneOpts{KeepDays: 7, NowNS: now})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := sortedInts(rep.RemovedManifests); !reflect.DeepEqual(got, []int{0, 2, 4}) {
		t.Fatalf("removed manifests: got %v, want [0 2 4]", got)
	}
	if got := sortedInts(rep.KeptNamed); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("manifest 1 was old enough to be a candidate and must be reported kept-by-name, got %v", rep.KeptNamed)
	}
	if rep.RemovedObjects != 0 {
		t.Fatalf("no objects are referenced here; RemovedObjects must be 0, got %d", rep.RemovedObjects)
	}
	for _, id := range []int{0, 2, 4} {
		if _, err := Load(storeDir, id); err == nil {
			t.Fatalf("manifest %d must be removed from disk", id)
		}
	}
	for _, id := range []int{1, 3} {
		if _, err := Load(storeDir, id); err != nil {
			t.Fatalf("manifest %d must survive pruning: %v", id, err)
		}
	}
	if m, ok, err := Latest(storeDir); err != nil || !ok || m.ID != 3 {
		t.Fatalf("the latest DURABLE baseline must survive pruning, got %+v ok=%v err=%v", m, ok, err)
	}
}

// TestPruneGCRemovesOnlyUnreferencedObjects: GC deletes exactly the objects no
// remaining manifest and no remaining versionlog record references. A salvage
// object held ONLY by a live versionlog record survives; an object held only by
// the pruned manifest or an expired record does not; and the retained
// checkpoint (workspace and extra root) still restores byte-exact afterwards.
func TestPruneGCRemovesOnlyUnreferencedObjects(t *testing.T) {
	storeDir := t.TempDir()
	oc := mustObj(t, storeDir)
	project := filepath.Join(t.TempDir(), "proj")
	extra := t.TempDir()
	now := time.Now().UnixNano()
	old := now - 30*dayNS

	// m0 (old and unnamed, so it will be pruned): a doomed file plus a shared one.
	writeFile(t, filepath.Join(project, "doomed.txt"), "unique to the pruned checkpoint\n", 0o644)
	writeFile(t, filepath.Join(project, "shared.txt"), "shared across checkpoints\n", 0o644)
	m0, err := Snapshot(project, oc, nil, 0, old, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(storeDir, m0); err != nil {
		t.Fatal(err)
	}

	// m1 (recent, the latest DURABLE, so retained): drops doomed.txt, adds kept.txt,
	// and covers an extra protected root (Extra trees count as references too).
	if err := os.Remove(filepath.Join(project, "doomed.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(project, "kept.txt"), "unique to the retained checkpoint\n", 0o644)
	writeFile(t, filepath.Join(extra, "e.conf"), "extra-root content\n", 0o644)
	m1, err := Snapshot(project, oc, m0, 1, now-dayNS, DURABLE, 0, extra)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(storeDir, m1); err != nil {
		t.Fatal(err)
	}

	// Version log: one record older than the oldest retained manifest (expires
	// with its object), one newer whose object NO manifest holds: pure salvage
	// bytes that must survive GC on the versionlog reference alone.
	expiredRef := putContent(t, oc, "captured before the oldest retained checkpoint\n")
	liveRef := putContent(t, oc, "still-salvageable transient\n")
	seedVersionLog(t, storeDir,
		versionlog.Version{Op: versionlog.OpCreate, Path: project + "/expired.tmp", Ref: expiredRef, TimeNS: old},
		versionlog.Version{Op: versionlog.OpCreate, Path: project + "/live.tmp", Ref: liveRef, TimeNS: now},
	)

	doomedRef := m0.Entries["doomed.txt"].Ref
	sharedRef := m1.Entries["shared.txt"].Ref
	keptRef := m1.Entries["kept.txt"].Ref
	extraRef := m1.Extra[extra]["e.conf"].Ref
	before := fingerprint(t, project)

	rep, err := Prune(storeDir, oc, PruneOpts{KeepDays: 7, NowNS: now})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := sortedInts(rep.RemovedManifests); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("removed manifests: got %v, want [0]", got)
	}
	if rep.ExpiredVersions != 1 {
		t.Fatalf("exactly the pre-baseline record expires, got %d", rep.ExpiredVersions)
	}
	if rep.RemovedObjects != 2 || rep.RemovedBytes <= 0 {
		t.Fatalf("exactly doomedRef+expiredRef are unreferenced (2 objects, >0 bytes), got %+v", rep)
	}
	for ref, want := range map[string]bool{
		doomedRef:  false, // only the pruned manifest held it
		expiredRef: false, // only the expired record held it
		sharedRef:  true,  // retained manifest reference
		keptRef:    true,  // retained manifest reference
		extraRef:   true,  // retained Extra-tree reference
		liveRef:    true,  // live versionlog reference alone
	} {
		if oc.Has(ref) != want {
			t.Fatalf("after GC, Has(%s) = %v, want %v", ref, !want, want)
		}
	}
	recs, err := versionlog.Read(filepath.Join(storeDir, "versionlog"))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Ref != liveRef {
		t.Fatalf("only the live record must remain, got %+v", recs)
	}

	// The proof GC touched nothing referenced: the retained checkpoint still
	// restores byte-exact: workspace from a fresh dir, extra root in place.
	if err := os.RemoveAll(project); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(storeDir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := Restore(loaded, oc, project); err != nil {
		t.Fatalf("Restore after GC: %v", err)
	}
	if diff := diffTrees(before, fingerprint(t, project)); diff != "" {
		t.Fatalf("retained checkpoint not byte-exact after GC:\n%s", diff)
	}
	if err := os.RemoveAll(extra); err != nil {
		t.Fatal(err)
	}
	if err := RestoreExtras(loaded, oc); err != nil {
		t.Fatalf("RestoreExtras after GC: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(extra, "e.conf"))
	if err != nil || string(b) != "extra-root content\n" {
		t.Fatalf("extra root must restore from surviving objects: %q err=%v", b, err)
	}
}

// TestPruneDryRunDeletesNothing: DryRun computes the FULL report, the same one
// the real run then produces, while touching nothing: manifests, objects, and
// versionlog records all stay on disk.
func TestPruneDryRunDeletesNothing(t *testing.T) {
	storeDir := t.TempDir()
	oc := mustObj(t, storeDir)
	project := filepath.Join(t.TempDir(), "proj")
	now := time.Now().UnixNano()
	old := now - 30*dayNS

	writeFile(t, filepath.Join(project, "old-only.txt"), "held by the old checkpoint alone\n", 0o644)
	m0, err := Snapshot(project, oc, nil, 0, old, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(storeDir, m0); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(project, "old-only.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(project, "current.txt"), "held by the retained checkpoint\n", 0o644)
	m1, err := Snapshot(project, oc, m0, 1, now-dayNS, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(storeDir, m1); err != nil {
		t.Fatal(err)
	}
	expiredRef := putContent(t, oc, "expired salvage bytes\n")
	liveRef := putContent(t, oc, "live salvage bytes\n")
	seedVersionLog(t, storeDir,
		versionlog.Version{Op: versionlog.OpCreate, Path: project + "/expired.tmp", Ref: expiredRef, TimeNS: old},
		versionlog.Version{Op: versionlog.OpCreate, Path: project + "/live.tmp", Ref: liveRef, TimeNS: now},
	)
	oldRef := m0.Entries["old-only.txt"].Ref

	opts := PruneOpts{KeepDays: 7, NowNS: now, DryRun: true}
	dry, err := Prune(storeDir, oc, opts)
	if err != nil {
		t.Fatalf("Prune(DryRun): %v", err)
	}
	// The report is fully computed...
	if got := sortedInts(dry.RemovedManifests); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("dry-run removed manifests: got %v, want [0]", got)
	}
	if dry.ExpiredVersions != 1 || dry.RemovedObjects != 2 || dry.RemovedBytes <= 0 {
		t.Fatalf("dry run must report the full outcome, got %+v", dry)
	}
	// ...and nothing on disk moved.
	if ids, err := IDs(storeDir); err != nil || !reflect.DeepEqual(ids, []int{0, 1}) {
		t.Fatalf("dry run must leave every manifest, got %v err=%v", ids, err)
	}
	for _, ref := range []string{oldRef, expiredRef, liveRef} {
		if !oc.Has(ref) {
			t.Fatalf("dry run must leave every object, missing %s", ref)
		}
	}
	if recs, err := versionlog.Read(filepath.Join(storeDir, "versionlog")); err != nil || len(recs) != 2 {
		t.Fatalf("dry run must leave the version log, got %d records err=%v", len(recs), err)
	}

	// The real run performs exactly what the dry run reported.
	opts.DryRun = false
	real, err := Prune(storeDir, oc, opts)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !reflect.DeepEqual(normalizeReport(dry), normalizeReport(real)) {
		t.Fatalf("dry-run report must match the real run:\n dry  %+v\n real %+v", dry, real)
	}
	if _, err := Load(storeDir, 0); err == nil {
		t.Fatal("the real run must remove manifest 0")
	}
	if oc.Has(oldRef) || oc.Has(expiredRef) {
		t.Fatal("the real run must GC the unreferenced objects")
	}
}

// TestCompactVersionsDropsOnlyExpired: compaction drops exactly the records
// with TimeNS strictly before keepAfterNS (a record AT the cutoff survives),
// preserving order and every field; keepAfterNS=0 is a no-op; a second pass
// removes nothing; and the rewritten log is still a valid, appendable log.
func TestCompactVersionsDropsOnlyExpired(t *testing.T) {
	storeDir := t.TempDir()
	logPath := filepath.Join(storeDir, "versionlog")
	seedVersionLog(t, storeDir,
		versionlog.Version{Op: versionlog.OpCreate, Path: "/ws/a.txt", Ref: "refa", TimeNS: 100, Writer: "agent"},
		versionlog.Version{Op: versionlog.OpModify, Path: "/ws/b.txt", Ref: "refb", TimeNS: 200, Writer: "human"},
		versionlog.Version{Op: versionlog.OpDelete, Path: "/ws/a.txt", TimeNS: 300, Writer: "agent"},
	)

	removed, err := CompactVersions(storeDir, 200)
	if err != nil {
		t.Fatalf("CompactVersions: %v", err)
	}
	if removed != 1 {
		t.Fatalf("only TimeNS < 200 expires (the 100 record), got removed=%d", removed)
	}
	recs, err := versionlog.Read(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 surviving records, got %+v", recs)
	}
	// Order and full record fidelity survive the rewrite; the record exactly at
	// the cutoff is retained.
	if recs[0].Path != "/ws/b.txt" || recs[0].TimeNS != 200 || recs[0].Ref != "refb" || recs[0].Writer != "human" || recs[0].Op != versionlog.OpModify {
		t.Fatalf("record at the cutoff must survive intact, got %+v", recs[0])
	}
	if recs[1].Op != versionlog.OpDelete || recs[1].TimeNS != 300 || recs[1].Writer != "agent" {
		t.Fatalf("later record must survive intact, got %+v", recs[1])
	}

	// A second pass at the same cutoff removes nothing (idempotent).
	if removed, err = CompactVersions(storeDir, 200); err != nil || removed != 0 {
		t.Fatalf("recompaction must be a no-op, removed=%d err=%v", removed, err)
	}
	// keepAfterNS = 0 (no retained manifest) is a no-op, never a wipe.
	if removed, err = CompactVersions(storeDir, 0); err != nil || removed != 0 {
		t.Fatalf("keepAfterNS=0 must be a no-op, removed=%d err=%v", removed, err)
	}
	if recs, _ = versionlog.Read(logPath); len(recs) != 2 {
		t.Fatalf("no-op passes must not lose records, got %+v", recs)
	}

	// The atomic rewrite left a real log: it opens, locks, and appends.
	l, err := versionlog.Open(logPath)
	if err != nil {
		t.Fatalf("compacted log must still open as a version log: %v", err)
	}
	if err := l.Append(versionlog.Version{Op: versionlog.OpCreate, Path: "/ws/c.txt", Ref: "refc", TimeNS: 400}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if recs, _ = versionlog.Read(logPath); len(recs) != 3 || recs[2].Path != "/ws/c.txt" {
		t.Fatalf("compacted log must accept appends, got %+v", recs)
	}
}
