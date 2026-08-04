package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

// TestStoreValidationRefusesWithReason is THE validation rule: a store whose
// metadata cannot be parsed is refused, and the refusal names the reason: no
// silent proceed, no silent re-stamp.
func TestStoreValidationRefusesWithReason(t *testing.T) {
	dir := t.TempDir()

	// A fresh store is stamped once and reloads identically.
	m, err := EnsureMeta(dir, "/w")
	if err != nil {
		t.Fatal(err)
	}
	if m.Workspace != "/w" || m.MachineID == "" || m.Format != metaFormat {
		t.Fatalf("stamped meta wrong: %+v", m)
	}
	m2, err := EnsureMeta(dir, "/other") // existing meta wins; never overwritten
	if err != nil || m2.Workspace != "/w" {
		t.Fatalf("existing identity must be preserved, got %+v err=%v", m2, err)
	}

	// Corrupt metadata: refused with the reason named.
	if err := os.WriteFile(filepath.Join(dir, "store.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = EnsureMeta(dir, "/w")
	if err == nil || !strings.Contains(err.Error(), "store refused") {
		t.Fatalf("corrupt meta must refuse with a named reason, got %v", err)
	}
}

// TestExtraRestoreRefusesForeignStore: the identity gate. Extra folders
// restore to absolute paths, so a store from another machine or workspace is
// refused by name.
func TestExtraRestoreRefusesForeignStore(t *testing.T) {
	local := Meta{Format: metaFormat, MachineID: machineID(), Workspace: "/w"}
	if err := CheckExtraRestoreIdentity(local, "/w"); err != nil {
		t.Fatalf("matching identity must pass: %v", err)
	}
	foreignMachine := Meta{Format: metaFormat, MachineID: "some-other-machine", Workspace: "/w"}
	if err := CheckExtraRestoreIdentity(foreignMachine, "/w"); err == nil ||
		!strings.Contains(err.Error(), "machine") {
		t.Fatalf("foreign machine must refuse by name, got %v", err)
	}
	foreignWS := Meta{Format: metaFormat, MachineID: machineID(), Workspace: "/other"}
	if err := CheckExtraRestoreIdentity(foreignWS, "/w"); err == nil ||
		!strings.Contains(err.Error(), "workspace") {
		t.Fatalf("foreign workspace must refuse by name, got %v", err)
	}
}

// TestSalvageCapEvictsOldest: beyond MaxSalvageEntries, the oldest offers (by
// last captured write) are evicted; the newest survive.
func TestSalvageCapEvictsOldest(t *testing.T) {
	orig := MaxSalvageEntries
	MaxSalvageEntries = 3
	defer func() { MaxSalvageEntries = orig }()

	oc := mustObj(t, t.TempDir())
	var versions []versionlog.Version
	refs := map[string]string{}
	for _, p := range []string{"/w/a", "/w/b", "/w/c", "/w/d", "/w/e"} {
		ref, _, err := oc.Put([]byte("content of " + p))
		if err != nil {
			t.Fatal(err)
		}
		refs[p] = ref
		versions = append(versions, versionlog.Version{Op: versionlog.OpCreate, Path: p, Ref: ref})
	}
	got, _ := Salvage(versions, nil, oc)
	if len(got) != 3 {
		t.Fatalf("cap of 3 must keep 3 offers, got %d: %v", len(got), got)
	}
	for _, p := range []string{"/w/c", "/w/d", "/w/e"} { // newest three
		if got[p] != refs[p] {
			t.Fatalf("newest path %s must survive the cap, got %v", p, got)
		}
	}
	for _, p := range []string{"/w/a", "/w/b"} { // oldest two evicted
		if _, ok := got[p]; ok {
			t.Fatalf("oldest path %s must be evicted, got %v", p, got)
		}
	}
}
