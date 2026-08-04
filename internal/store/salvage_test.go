package store

import (
	"testing"

	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

// putContent stores content and returns its ref, so tests reference only refs
// that genuinely exist in the object store (Salvage must never offer bytes it
// cannot actually recover).
func putContent(t *testing.T, oc *objstore.Store, s string) string {
	t.Helper()
	ref, _, err := oc.Put([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestSalvageReturnsTransientAbsentFromManifest(t *testing.T) {
	oc := mustObj(t, t.TempDir())
	refT := putContent(t, oc, "transient bytes\n")
	refS := putContent(t, oc, "survivor bytes\n")

	// The manifest (a checkpoint) lists only the survivor.
	m := &Manifest{Root: "/w", Entries: map[string]Entry{
		"survivor.txt": {Kind: KindFile, Ref: refS, Mode: 0o644},
	}}
	versions := []versionlog.Version{
		{Op: versionlog.OpCreate, Path: "/w/survivor.txt", Ref: refS, Mode: 0o644},
		{Op: versionlog.OpCreate, Path: "/w/transient.txt", Ref: refT, Mode: 0o644}, // created then deleted, never in a checkpoint
	}

	got, _ := Salvage(versions, []*Manifest{m}, oc)
	if len(got) != 1 || got["/w/transient.txt"] != refT {
		t.Fatalf("expected only the transient salvaged, got %+v", got)
	}
}

func TestSalvageIgnoresPathsPresentInManifest(t *testing.T) {
	oc := mustObj(t, t.TempDir())
	ref := putContent(t, oc, "x\n")
	m := &Manifest{Root: "/w", Entries: map[string]Entry{
		"a.txt": {Kind: KindFile, Ref: ref, Mode: 0o644},
	}}
	versions := []versionlog.Version{{Op: versionlog.OpCreate, Path: "/w/a.txt", Ref: ref}}
	if got, _ := Salvage(versions, []*Manifest{m}, oc); len(got) != 0 {
		t.Fatalf("a path in the manifest needs no salvage, got %+v", got)
	}
}

func TestSalvageUsesLatestVersionOfAPath(t *testing.T) {
	oc := mustObj(t, t.TempDir())
	ref1 := putContent(t, oc, "v1\n")
	ref2 := putContent(t, oc, "v2\n")
	m := &Manifest{Root: "/w", Entries: map[string]Entry{}}
	versions := []versionlog.Version{
		{Op: versionlog.OpCreate, Path: "/w/f", Ref: ref1},
		{Op: versionlog.OpModify, Path: "/w/f", Ref: ref2}, // newer
	}
	got, _ := Salvage(versions, []*Manifest{m}, oc)
	if got["/w/f"] != ref2 {
		t.Fatalf("salvage should offer the latest version %s, got %s", ref2, got["/w/f"])
	}
}

func TestSalvageSkipsRefsMissingFromStore(t *testing.T) {
	oc := mustObj(t, t.TempDir())
	m := &Manifest{Root: "/w", Entries: map[string]Entry{}}
	versions := []versionlog.Version{
		{Op: versionlog.OpCreate, Path: "/w/gone", Ref: "abcdef0000000000000000000000000000000000"},
	}
	if got, _ := Salvage(versions, []*Manifest{m}, oc); len(got) != 0 {
		t.Fatalf("cannot salvage a ref that is not in the store, got %+v", got)
	}
}

// TestSalvageOffersDeletedTransient: a created-then-deleted transient is the
// canonical "Recovered file", so a delete record must NOT clear a path's
// salvageability. The change feed journals every unlink as OpDelete, so a
// delete-clears rule would mean salvage never offers anything again.
func TestSalvageOffersDeletedTransient(t *testing.T) {
	oc := mustObj(t, t.TempDir())
	ref := putContent(t, oc, "then deleted\n")
	m := &Manifest{Root: "/w", Entries: map[string]Entry{}}
	versions := []versionlog.Version{
		{Op: versionlog.OpCreate, Path: "/w/f", Ref: ref},
		{Op: versionlog.OpDelete, Path: "/w/f", Writer: "agent"},
	}
	got, _ := Salvage(versions, []*Manifest{m}, oc)
	if len(got) != 1 || got["/w/f"] != ref {
		t.Fatalf("a deleted transient no checkpoint holds must be salvageable, got %+v", got)
	}
}

// TestSalvageSkipsPathAnyManifestHolds: a file some OLDER checkpoint lists is
// restorable by id, so it is not a "recovered file" even if the latest manifest
// no longer has it (it was deleted after being checkpointed).
func TestSalvageSkipsPathAnyManifestHolds(t *testing.T) {
	oc := mustObj(t, t.TempDir())
	ref := putContent(t, oc, "checkpointed, later deleted\n")
	older := &Manifest{Root: "/w", Entries: map[string]Entry{
		"f": {Kind: KindFile, Ref: ref, Mode: 0o644},
	}}
	latest := &Manifest{Root: "/w", Entries: map[string]Entry{}}
	versions := []versionlog.Version{
		{Op: versionlog.OpModify, Path: "/w/f", Ref: ref},
		{Op: versionlog.OpDelete, Path: "/w/f"},
	}
	if got, _ := Salvage(versions, []*Manifest{older, latest}, oc); len(got) != 0 {
		t.Fatalf("a path an older checkpoint holds is restore-by-id territory, got %+v", got)
	}
}

func TestSalvageNilManifestOffersAllLiveVersions(t *testing.T) {
	oc := mustObj(t, t.TempDir())
	ref := putContent(t, oc, "no checkpoint yet\n")
	versions := []versionlog.Version{{Op: versionlog.OpCreate, Path: "/w/f", Ref: ref}}
	got, _ := Salvage(versions, nil, oc)
	if len(got) != 1 || got["/w/f"] != ref {
		t.Fatalf("with no manifest, all live captured versions are salvageable, got %+v", got)
	}
}
