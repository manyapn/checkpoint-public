package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Write persists a manifest atomically (tmp write + rename, so a visible
// manifest is always complete and a torn write is never seen as DURABLE).
// storeDir holds a manifests/ subdir; the file is manifests/<id>.json.
func Write(storeDir string, m *Manifest) error {
	dir := filepath.Join(storeDir, "manifests")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	// Link-then-remove instead of rename: an id that already exists is REFUSED
	// (os.ErrExist), never silently replaced. The daemon's autonomous cuts
	// (setup / baseline rescans) and CLI pre-op snapshots race for ids, and a
	// rename would let one silently destroy the other's checkpoint.
	final := filepath.Join(dir, fmt.Sprintf("%d.json", m.ID))
	if err := os.Link(tmpName, final); err != nil {
		os.Remove(tmpName)
		if os.IsExist(err) {
			return fmt.Errorf("manifest %d already exists: %w", m.ID, os.ErrExist)
		}
		return err
	}
	return os.Remove(tmpName)
}

// Load reads manifest <id>. A torn/unparsable manifest is an error, never a
// half-valid manifest presented as DURABLE.
func Load(storeDir string, id int) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(storeDir, "manifests", fmt.Sprintf("%d.json", id)))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Latest returns the highest-id manifest whose file parses and is marked DURABLE.
// ok is false when the store has no durable manifest (incl. an empty store). A
// daemon crash mid-write leaves a tmp-* file (never a <id>.json), so the latest
// visible manifest is always complete; a torn or PARTIAL manifest is skipped.
func Latest(storeDir string) (m *Manifest, ok bool, err error) {
	ids, err := manifestIDs(storeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ids)))
	for _, id := range ids {
		cand, err := Load(storeDir, id)
		if err != nil {
			continue // torn/unparsable: skip to an older one
		}
		if cand.Coverage == DURABLE {
			return cand, true, nil
		}
	}
	return nil, false, nil
}

// NextID returns the id to assign the next manifest: one past the highest id
// present (any coverage), or 0 for an empty store.
func NextID(storeDir string) (int, error) {
	ids, err := manifestIDs(storeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	max := -1
	for _, id := range ids {
		if id > max {
			max = id
		}
	}
	return max + 1, nil
}

// IDs returns all manifest ids present, ascending. Empty (not an error) when the
// store has no checkpoints yet.
func IDs(storeDir string) ([]int, error) {
	ids, err := manifestIDs(storeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Ints(ids)
	return ids, nil
}

// ValidIDs returns the ids whose manifest file actually PARSES, ascending: the
// honest count of checkpoints a user can act on. IDs reports every id
// present, including a torn/half-written file, which is right for numbering
// (NextID must never reuse one) and wrong for counting: history and latest
// already skip unparsable manifests, so a status count built on IDs claims
// checkpoints that cannot be restored. Empty (not an error) for a store with no
// manifests directory.
func ValidIDs(storeDir string) ([]int, error) {
	ids, err := IDs(storeDir)
	if err != nil {
		return nil, err
	}
	var valid []int
	for _, id := range ids {
		if _, err := Load(storeDir, id); err != nil {
			continue // torn/unparsable: never counted as a checkpoint
		}
		valid = append(valid, id)
	}
	return valid, nil
}

func manifestIDs(storeDir string) ([]int, error) {
	ents, err := os.ReadDir(filepath.Join(storeDir, "manifests"))
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, e := range ents {
		var id int
		if n, _ := fmt.Sscanf(e.Name(), "%d.json", &id); n == 1 {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
