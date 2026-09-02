// Package store holds the commits of checkpoint's session history: one
// Manifest per checkpoint, in checkpoint's own format, addressed by ID, over
// content held in a git-backed object store (internal/objstore). A manifest is
// the whole workspace at a boundary, not a diff, so checking out a durable
// manifest restores the tree byte-exact, including what git alone cannot
// represent (empty dirs, permissions, symlinks).
//
// Manifests are built by an INCREMENTAL BOUNDARY SCAN (walk the tree; reuse a
// prior ref for a file whose size+mtime are unchanged; capture changed/new files
// into the object store; detect deletions by absence). A scan cannot miss a
// delete, which is what keeps a checkpoint a complete state rather than a
// partial one: write files, rm -rf the project, restore the latest manifest
// byte-exact. Folding a change-set over the previous manifest instead
// (SnapshotFold) costs O(changes) rather than O(tree), but is only correct given
// a COMPLETE feed of directory-entry events (which needs fanotify in FID mode),
// so the scan stays the fallback whenever that feed reports a hole.
//
// Transients that never reach a boundary, created and written and deleted
// between two checkpoints, are covered by the per-write ledger
// (internal/versionlog), not by manifests.
package store

// Coverage is a manifest's honesty label: how much of its window it can vouch for.
type Coverage string

const (
	// DURABLE: this named manifest is recoverable (restores byte-exact). Not a
	// claim that no unsupported write happened anywhere in the project.
	DURABLE Coverage = "DURABLE"
	// PARTIAL: the daemon detected unbounded loss/race/overflow in this window.
	PARTIAL Coverage = "PARTIAL"
)

// Entry kinds. Regular files carry Ref+Mode; symlinks carry Link; dirs carry
// Mode. Anything else (fifo/device/socket) is out of contract and omitted.
const (
	KindFile    = "file"
	KindDir     = "dir"
	KindSymlink = "symlink"
)

// Entry is one path's state in a manifest.
type Entry struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref,omitempty"`  // file: object-store ref of content
	Mode    uint32 `json:"mode,omitempty"` // file/dir: unix mode bits
	Link    string `json:"link,omitempty"` // symlink: target
	Size    int64  `json:"size,omitempty"` // file: for incremental reuse
	MtimeNS int64  `json:"mtime_ns,omitempty"`
	// CtimeNS is the inode change time, the second half of the reuse key.
	// Mtime is USER-SETTABLE (utimensat, which every "preserve timestamps"
	// tool calls), so size+mtime alone can be forged: rewrite a file with
	// same-length content, put the old mtime back, and a size+mtime scan
	// reuses the previous ref, i.e. the checkpoint claims bytes that are not
	// on disk and says nothing about it. Ctime is bumped by the kernel on
	// every write and cannot be set from userspace, so it closes that hole.
	// Additive: an entry from an older manifest carries 0 and is never
	// reused, costing one rehash pass and then self-healing.
	CtimeNS int64 `json:"ctime_ns,omitempty"`
}

// Exception is one NAMED item a checkpoint could not cover: the path (relative
// to the root it sits under) plus why. Bounded, listed loss: the rest of the
// manifest still restores. This is what makes "Recoverable with exceptions"
// honest: we say exactly what is missing, never hide a skip.
type Exception struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Manifest is a complete description of a workspace at one instant, recovered by
// ID. Root is the absolute path the entries are relative to.
type Manifest struct {
	ID       int              `json:"id"`
	TimeNS   int64            `json:"time_ns"`
	Root     string           `json:"root"`
	Coverage Coverage         `json:"coverage"`
	Missed   int              `json:"missed"`
	Entries  map[string]Entry `json:"entries"` // key: path relative to Root

	// Boundary metadata (set by the daemon at checkpoint time; zero-valued for
	// manifests cut by the plain `create` path). Source labels the trigger (e.g.
	// "run: go build"). SettleTimedOut is true when the post-boundary settle hit
	// its hard ceiling with writes still in flight. It is an honesty signal that
	// the snapshot may have caught a logical operation mid-flight, NOT a claim of
	// lost data (Coverage still governs recoverability).
	Source         string `json:"source,omitempty"`
	SettleTimedOut bool   `json:"settle_timed_out,omitempty"`

	// ScanNS is the wall clock when this manifest finished capturing content. It
	// is the trust anchor for the NEXT incremental scan: inode timestamps come
	// from a coarse clock (and some filesystems only keep whole seconds), so a
	// write landing in the same timestamp tick as our read leaves size, mtime
	// AND ctime untouched and is therefore invisible to a stat comparison. An
	// entry whose ctime is not comfortably older than the scan that recorded it
	// is not reused; it is read again (see reusable). Set by Snapshot and
	// SnapshotFold, never by the caller. Additive: 0 on older manifests, which
	// disables reuse for one pass and then self-heals.
	ScanNS int64 `json:"scan_ns,omitempty"`

	// Name is the user's label for a named checkpoint (`save --name`). Additive;
	// absent on automatic checkpoints. A named checkpoint survives pruning.
	Name string `json:"name,omitempty"`

	// Exceptions are the NAMED items this checkpoint does not cover (unreadable
	// entries, unsupported kinds, uncaptured writes). Additive; absent on older
	// manifests. A DURABLE manifest with exceptions is Recoverable-with-
	// exceptions, listed by name. Paths under Root are relative to it; paths
	// under an extra protected folder are absolute (unambiguous across roots).
	Exceptions []Exception `json:"exceptions,omitempty"`

	// Extra holds the trees of additional protected folders (the project is
	// protected by default; more are added with --protect), keyed by their
	// absolute root. Additive; absent on older manifests and when
	// no extra folders are protected. Extra folders restore IN PLACE (to their
	// absolute roots) because they only make sense at their real location.
	Extra map[string]map[string]Entry `json:"extra,omitempty"`
}
