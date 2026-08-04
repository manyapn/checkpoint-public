package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Meta is the store's identity record (store.json at the store root): which
// machine created it and which workspace it protects. Recorded at creation so
// operations that write to ABSOLUTE paths (extra-root restore) can refuse a
// store that was carried to a different machine or project. It is cheap to
// stamp at creation and impossible to reconstruct afterwards.
type Meta struct {
	Format    int    `json:"format"` // store format version; currently 1
	MachineID string `json:"machine_id"`
	Workspace string `json:"workspace"` // absolute protected root this store was created for
	CreatedNS int64  `json:"created_ns"`
}

const metaFormat = 1

// CheckStoreLocation refuses a store that lives inside the workspace it
// protects (or a workspace inside its own store). The headline guarantee is
// that destroying the project cannot destroy its recovery data. An in-tree
// store makes `rm -rf project` delete the checkpoints too, and the store's own
// churn would be captured as project content. Equal paths are refused for the
// same reason. Callers pass absolute, cleaned paths.
//
// The check runs TWICE: once on the paths as written, and once on their
// symlink-RESOLVED forms. A lexical comparison alone is trivially bypassed by
// `--store /tmp/apparent-store` pointing at <project>/inside-store: the guard
// would pass and `rm -rf project` would still take the recovery store with it.
// Only the real location decides.
func CheckStoreLocation(storeDir, workspace string) error {
	if err := checkStoreLocationPaths(storeDir, workspace); err != nil {
		return err
	}
	rs, rw := resolveForCompare(storeDir), resolveForCompare(workspace)
	if rs == filepath.Clean(storeDir) && rw == filepath.Clean(workspace) {
		return nil // nothing resolved differently; the lexical pass is the whole answer
	}
	if err := checkStoreLocationPaths(rs, rw); err != nil {
		return fmt.Errorf("%w (via symlink: %s resolves to %s, %s resolves to %s)",
			err, storeDir, rs, workspace, rw)
	}
	return nil
}

// resolveForCompare returns path's real location. Stores are created on demand,
// so the path often does not exist yet: resolve the nearest existing ancestor
// and re-attach the missing tail, which is enough to catch a symlinked parent.
// Unresolvable paths degrade to the cleaned lexical form.
func resolveForCompare(path string) string {
	p := filepath.Clean(path)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(r)
	}
	var tail []string
	cur := p
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return p // reached the root without an existing ancestor
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Clean(filepath.Join(append([]string{r}, tail...)...))
		}
	}
}

func checkStoreLocationPaths(storeDir, workspace string) error {
	s := filepath.Clean(storeDir)
	w := filepath.Clean(workspace)
	switch {
	case s == w:
		return fmt.Errorf("refusing store %s: a store cannot BE the protected folder, so "+
			"deleting the project would delete its checkpoints (use --store outside %s)", s, w)
	case strings.HasPrefix(s, w+string(filepath.Separator)):
		return fmt.Errorf("refusing store %s: it is inside the protected folder %s, so "+
			"deleting the project would delete its checkpoints, defeating out-of-tree recovery "+
			"(use --store outside the project, or omit --store for the default out-of-tree location)", s, w)
	case strings.HasPrefix(w, s+string(filepath.Separator)):
		return fmt.Errorf("refusing store %s: the protected folder %s is inside it, so "+
			"checkpoint would capture its own store as project content", s, w)
	}
	return nil
}

// EnsureMeta loads the store's identity, creating it (for a new store, or one
// from before identity stamping) when absent. THE validation rule: a store
// whose metadata cannot be read or parsed is refused with the reason named. No
// operation proceeds against a store that cannot say what it is.
func EnsureMeta(storeDir, workspace string) (Meta, error) {
	m, err := LoadMeta(storeDir)
	if err == nil {
		return m, nil
	}
	if !os.IsNotExist(err) {
		return Meta{}, err // corrupt/unreadable: refuse, never overwrite
	}
	m = Meta{Format: metaFormat, MachineID: machineID(), Workspace: workspace, CreatedNS: time.Now().UnixNano()}
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return Meta{}, err
	}
	b, _ := json.Marshal(m)
	tmp, err := os.CreateTemp(storeDir, "meta-")
	if err != nil {
		return Meta{}, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return Meta{}, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return Meta{}, err
	}
	if err := os.Rename(tmpName, filepath.Join(storeDir, "store.json")); err != nil {
		return Meta{}, err
	}
	return m, nil
}

// LoadMeta reads the identity record. A missing file returns an IsNotExist
// error (callers may EnsureMeta); anything else is a named refusal.
func LoadMeta(storeDir string) (Meta, error) {
	b, err := os.ReadFile(filepath.Join(storeDir, "store.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return Meta{}, err
		}
		return Meta{}, fmt.Errorf("store refused: metadata unreadable: %w", err)
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return Meta{}, fmt.Errorf("store refused: metadata corrupt (%s): %v", filepath.Join(storeDir, "store.json"), err)
	}
	if m.Format != metaFormat {
		return Meta{}, fmt.Errorf("store refused: format %d not supported (this build supports %d)", m.Format, metaFormat)
	}
	return m, nil
}

// CheckWorkspaceIdentity refuses a store stamped for a DIFFERENT workspace than
// the one the caller is about to operate on. Stamping identity is not enough on
// its own: unverified, a daemon or a `create` can be pointed at another
// project's store and quietly interleave two projects' checkpoints in one
// history, after which "restore checkpoint N" restores someone else's tree
// over yours. Both workspaces are named so the user can see which is which.
//
// Deliberately NOT an auto-migration: rewriting the stamp to match would erase
// the only evidence that the store already holds another project's history.
// The fix is a different --store (or a deliberate, manual decision), not a
// silent re-label. A store with no stamp yet is EnsureMeta's business, not this
// one's.
func CheckWorkspaceIdentity(m Meta, workspace string) error {
	if m.Workspace != workspace {
		return fmt.Errorf("store refused: it was created for workspace %s, but this operation protects %s. "+
			"Using one store for two projects mixes their checkpoints into one history "+
			"(use a separate --store for %s)", m.Workspace, workspace, workspace)
	}
	return nil
}

// CheckExtraRestoreIdentity is the identity gate for operations that write to
// ABSOLUTE paths outside the workspace: refuse when the store was created on a
// different machine or for a different workspace than the manifest claims.
// In-place extra-root paths from another machine/project would rewrite the
// wrong filesystem locations.
func CheckExtraRestoreIdentity(m Meta, manifestRoot string) error {
	if cur := machineID(); m.MachineID != cur {
		return fmt.Errorf("extra-root restore refused: store was created on machine %s, this is machine %s (extra folders restore to absolute paths; restore them on the original machine or copy files manually)", m.MachineID, cur)
	}
	if m.Workspace != manifestRoot {
		return fmt.Errorf("extra-root restore refused: store belongs to workspace %s, checkpoint claims %s", m.Workspace, manifestRoot)
	}
	return nil
}

// machineID identifies this machine: /etc/machine-id when present (Linux),
// hostname otherwise.
func machineID() string {
	if b, err := os.ReadFile("/etc/machine-id"); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return "host:" + h
}
