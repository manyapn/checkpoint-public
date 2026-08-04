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

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/manyapn/checkpoint-public/internal/objstore"
)

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

// defaultExcludes is the default-skip list of rebuildable dirs (protect
// meaningful work, not regenerable bulk), plus .git. Git is excluded because
// .git/objects churns on every operation, and capturing it poisons both runtime
// overhead and store size for content git is already keeping safe. The store
// dir lives out-of-tree, so it needs no self-exclusion. The list is fixed, not
// user-configurable.
var defaultExcludes = map[string]bool{
	".git": true, "node_modules": true, "build": true, "target": true,
	"dist": true, "__pycache__": true, ".venv": true,
}

// readFile is a seam so tests can prove the unreadable-file exception path even
// when running as root (root ignores permission bits, so a chmod-0 fixture
// cannot fail a read).
var readFile = os.ReadFile

// ScanProgress, when non-nil, receives a running entries-scanned count during
// Snapshot (every scanProgressStride entries and once at the end). The daemon
// sets it during first-time setup so `status` can show honest progress
// ("Setting up", with a running files-scanned count); nil costs nothing. Not
// synchronized: set it before scanning starts, clear it after.
var ScanProgress func(scanned int)

const scanProgressStride = 128

// ErrSymlinkRoot marks a refusal to treat a SYMLINK as a protected/restore root.
// filepath.WalkDir on a symlink visits only the link itself, so a symlinked root
// would snapshot ZERO entries and then be badged "Fully recoverable", the worst
// possible outcome (a confident green label over an empty checkpoint). This
// layer must never lie, so it refuses and names the resolved path to use
// instead; resolving roots for the user is the CLI's job, not the store's.
var ErrSymlinkRoot = errors.New("root is a symlink")

// checkNotSymlinkRoot refuses root when it is a symlink. what is the role of
// the path in the message ("protected root" / "restore target"). A root that
// does not exist is not this check's problem: the walk (or the restore's
// MkdirAll) reports it.
func checkNotSymlinkRoot(what, root string) error {
	fi, err := os.Lstat(root)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return nil
	}
	hint := "pass the resolved path instead"
	if resolved, rerr := filepath.EvalSymlinks(root); rerr == nil {
		hint = fmt.Sprintf("pass the resolved path instead (%s)", resolved)
	}
	return fmt.Errorf("refusing %s %s: %w, and a symlink root captures nothing while still being badged recoverable; %s",
		what, root, ErrSymlinkRoot, hint)
}

// Snapshot walks root (and any extra protected folders) and builds a manifest,
// reusing refs from prev for files whose size+mtime are unchanged (incremental).
// New/changed files are captured into oc. cov/missed are stamped by the caller
// (the daemon knows the window's loss state; the plain create path passes
// DURABLE, 0). prev may be nil. Extra folders land under Manifest.Extra, keyed
// by their absolute root.
//
// Nothing is skipped silently: an entry the scan cannot read (permissions, IO)
// or cannot represent (fifo/device/socket) becomes a NAMED exception on the
// manifest instead of quietly vanishing from it.
func Snapshot(root string, oc *objstore.Store, prev *Manifest, id int, timeNS int64, cov Coverage, missed int, extras ...string) (*Manifest, error) {
	if err := checkNotSymlinkRoot("protected root", root); err != nil {
		return nil, err
	}
	for _, ex := range extras {
		if err := checkNotSymlinkRoot("extra protected folder", ex); err != nil {
			return nil, err
		}
	}
	m := &Manifest{ID: id, TimeNS: timeNS, Root: root, Coverage: cov, Missed: missed, Entries: map[string]Entry{}}
	scanned := 0
	if ScanProgress != nil {
		defer func() { ScanProgress(scanned) }()
	}
	var prevEntries map[string]Entry
	var prevScanNS int64
	if prev != nil {
		prevEntries = prev.Entries
		prevScanNS = prev.ScanNS
	}
	progress := func() {
		scanned++
		if ScanProgress != nil && scanned%scanProgressStride == 0 {
			ScanProgress(scanned)
		}
	}
	err := scanTree(root, oc, prevEntries, prevScanNS, m.Entries, func(rel, reason string) {
		m.Exceptions = append(m.Exceptions, Exception{Path: rel, Reason: reason})
	}, progress)
	if err != nil {
		return nil, err
	}
	for _, ex := range extras {
		entries := map[string]Entry{}
		var prevExtra map[string]Entry
		if prev != nil {
			prevExtra = prev.Extra[ex]
		}
		exRoot := ex // exceptions under an extra root are named by absolute path
		err := scanTree(ex, oc, prevExtra, prevScanNS, entries, func(rel, reason string) {
			m.Exceptions = append(m.Exceptions, Exception{Path: filepath.Join(exRoot, rel), Reason: reason})
		}, progress)
		if err != nil {
			return nil, err
		}
		if m.Extra == nil {
			m.Extra = map[string]map[string]Entry{}
		}
		m.Extra[ex] = entries
	}
	sort.Slice(m.Exceptions, func(i, j int) bool { return m.Exceptions[i].Path < m.Exceptions[j].Path })
	m.ScanNS = time.Now().UnixNano() // capture finished: the next scan's trust anchor
	return m, nil
}

// scanTree walks one protected root, filling out with its entries (incremental
// against prevEntries, which were recorded by a scan that finished at
// prevScanNS) and reporting per-path skips through except.
func scanTree(root string, oc *objstore.Store, prevEntries map[string]Entry, prevScanNS int64, out map[string]Entry, except func(rel, reason string), progress func()) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if p == root {
			return nil
		}
		if progress != nil {
			progress()
		}
		rel, _ := filepath.Rel(root, p)
		if err != nil {
			except(rel, "unreadable: "+err.Error())
			return nil // don't abort the whole snapshot
		}
		base := filepath.Base(p)
		if d.IsDir() && defaultExcludes[base] {
			return filepath.SkipDir
		}
		// Credential material is never captured (security default), and never
		// silently: it is named as an exception so the user can see precisely
		// what is unprotected.
		if IsSecretPath(p) || IsSecretPath(rel) {
			except(rel, secretReason)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			except(rel, "unreadable: "+err.Error())
			return nil
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				except(rel, "unreadable symlink: "+err.Error())
				return nil
			}
			out[rel] = Entry{Kind: KindSymlink, Link: target}
		case fi.IsDir():
			out[rel] = Entry{Kind: KindDir, Mode: unixMode(fi.Mode())}
		case fi.Mode().IsRegular():
			e := fileEntry(fi)
			if pe, ok := prevEntries[rel]; ok && reusable(pe, e, prevScanNS, oc) {
				e.Ref = pe.Ref // incremental reuse: unchanged file
				out[rel] = e
				return nil
			}
			content, err := readFile(p)
			if err != nil {
				except(rel, "unreadable: "+err.Error())
				return nil
			}
			ref, _, err := oc.Put(content)
			if err != nil {
				return err
			}
			e.Ref = ref
			out[rel] = e
		default:
			// A kind of change we cannot capture (fifo/device/socket): a per-file
			// "not supported" fact, named and never silently omitted.
			except(rel, "unsupported kind: "+fi.Mode().Type().String())
		}
		return nil
	})
}

// fileEntry is the stat-only part of a regular file's entry (everything but
// Ref). Built identically by the scan and the fold so their manifests compare
// equal for a file neither had to re-read.
func fileEntry(fi fs.FileInfo) Entry {
	return Entry{
		Kind:    KindFile,
		Mode:    unixMode(fi.Mode()),
		Size:    fi.Size(),
		MtimeNS: fi.ModTime().UnixNano(),
		CtimeNS: ctimeNS(fi),
	}
}

// statSettleNS is how much older than the recording scan a file's ctime must be
// before that ctime is accepted as proof of "unchanged". Inode timestamps are
// stamped from a COARSE clock (millisecond-ish ticks), and some filesystems
// store only whole seconds, so two writes inside one tick are indistinguishable
// by stat. One second covers both. The cost is bounded and self-limiting: only
// files touched within a second of a checkpoint are re-read at the next one.
const statSettleNS = int64(1e9)

// reusable reports whether prior entry pe can stand in for the file just
// statted as e, i.e. whether the scan may skip reading the bytes. prevScanNS is
// the ScanNS of the manifest pe came from.
//
// Size, mtime AND ctime must match, and pe's ctime must predate its own scan by
// statSettleNS:
//
//   - mtime alone is worthless as a change key: utimensat lets any process put
//     an old mtime back after rewriting a file, so "same size, same mtime" can
//     be manufactured at will (see Entry.CtimeNS).
//   - ctime cannot be set from userspace, but it is only as fine as the clock
//     that stamps it, hence the settle window (see Manifest.ScanNS).
//
// Anything we cannot check (no recorded ctime, no recorded scan time: an older
// manifest, or a platform without ctime) means no reuse. The penalty is a
// re-read; the alternative is a checkpoint that silently references content
// that is no longer on disk.
func reusable(pe, e Entry, prevScanNS int64, oc *objstore.Store) bool {
	return pe.Kind == KindFile &&
		pe.Size == e.Size &&
		pe.MtimeNS == e.MtimeNS &&
		pe.CtimeNS == e.CtimeNS &&
		pe.CtimeNS != 0 &&
		prevScanNS != 0 &&
		pe.CtimeNS < prevScanNS-statSettleNS &&
		oc.Has(pe.Ref)
}

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

// Restore materializes manifest m's PRIMARY tree into targetDir from the object
// store. Dirs are created parents-first; every referenced blob must be present or
// Restore errors (a manifest that cannot fully restore is not DURABLE).
// targetDir need not exist. Extra protected folders are NOT touched; they
// restore in place only, via RestoreExtras (an explicit, separate step: a caller
// restoring the workspace to an inspection copy must not clobber live external
// folders as a side effect).
func Restore(m *Manifest, oc *objstore.Store, targetDir string) error {
	return restoreTree(m.Entries, oc, targetDir)
}

// RestoreExact restores m's primary tree into targetDir AND removes paths the
// checkpoint does not contain, so the result is the tree byte-exact, the
// guarantee a whole-checkpoint restore makes. Returns the paths removed
// (relative to targetDir) so the caller can report them; the caller is
// expected to have cut a pre-restore checkpoint first, which is what makes
// removal recoverable. keptExceptions are the paths it deliberately did NOT
// touch because the manifest names them as exceptions. The caller must report
// them as "could not restore, left alone" (see pruneToManifest).
//
// What it must NEVER remove, and why:
//
//   - The default-skipped rebuildable directories (node_modules, .git, build, …)
//     are ABSENT FROM EVERY MANIFEST BY DESIGN, so "not in the manifest" does not
//     mean "not wanted" for them. Deleting them would destroy exactly the state
//     checkpoint promises never to touch. They are skipped whole-subtree.
//   - Anything the manifest NAMES as an exception (a fifo/device, an unreadable
//     file): also absent from Entries by design, and (unlike an ordinary
//     untracked file) NOT recoverable from the pre-restore checkpoint, which
//     records it as an exception too. Removing it would be irreversible.
//
// Use Restore (not RestoreExact) for a partial restore, where "not in the
// manifest" means nothing at all.
func RestoreExact(m *Manifest, oc *objstore.Store, targetDir string) (removed []string, keptExceptions []string, err error) {
	if err := restoreTree(m.Entries, oc, targetDir); err != nil {
		return nil, nil, err
	}
	return pruneToManifest(m, targetDir)
}

// pruneToManifest removes everything under targetDir that m.Entries does not
// describe, skipping default-excluded trees and every path m names as an
// exception (see RestoreExact). Directories that are themselves unwanted are
// removed whole and not descended into, unless they contain a named exception,
// in which case the directory survives (an exception must stay reachable) and
// its other children are pruned individually.
//
// Returns the removed paths and the named exceptions it found and preserved,
// both relative to targetDir, both sorted.
func pruneToManifest(m *Manifest, targetDir string) (removed []string, keptExceptions []string, err error) {
	entries := m.Entries
	protected := exceptionRels(m)
	sep := string(filepath.Separator)
	// protectedAt reports the exception covering rel (rel itself or an ancestor).
	protectedAt := func(rel string) (string, bool) {
		if protected[rel] {
			return rel, true
		}
		for p := range protected {
			if strings.HasPrefix(rel, p+sep) {
				return p, true
			}
		}
		return "", false
	}
	// holdsProtected reports whether rel is an ancestor DIRECTORY of an exception.
	holdsProtected := func(rel string) bool {
		for p := range protected {
			if strings.HasPrefix(p, rel+sep) {
				return true
			}
		}
		return false
	}

	var victims []string
	kept := map[string]bool{}
	err = filepath.WalkDir(targetDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == targetDir {
			return nil //nolint:nilerr // an unreadable entry is not ours to delete
		}
		rel, relErr := filepath.Rel(targetDir, p)
		if relErr != nil {
			return nil
		}
		// Never descend into a default-skipped tree, and never remove one.
		if d.IsDir() && defaultExcludes[filepath.Base(p)] {
			return filepath.SkipDir
		}
		// Never remove a NAMED exception (or anything beneath it): the manifest
		// says it exists and could not be captured, so deleting it is a loss no
		// checkpoint can undo.
		if ex, ok := protectedAt(rel); ok {
			kept[ex] = true
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if _, wanted := entries[rel]; wanted {
			return nil
		}
		if d.IsDir() && holdsProtected(rel) {
			return nil // keep the ancestor so the exception under it survives; prune its other children
		}
		victims = append(victims, rel)
		if d.IsDir() {
			return filepath.SkipDir // removed whole; children need no separate visit
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(victims)
	for _, rel := range victims {
		full, err := safeJoin(targetDir, rel)
		if err != nil {
			return nil, nil, err
		}
		if err := os.RemoveAll(full); err != nil {
			return nil, nil, fmt.Errorf("restore: removing %s: %w", rel, err)
		}
	}
	for rel := range kept {
		keptExceptions = append(keptExceptions, rel)
	}
	sort.Strings(keptExceptions)
	return victims, keptExceptions, nil
}

// exceptionRels is the set of PRIMARY-TREE paths m names as exceptions.
// Exceptions under an extra protected folder are recorded as ABSOLUTE paths
// (unambiguous across roots) and are irrelevant to a primary-tree prune, so
// they are dropped here, along with any path that would escape the target.
func exceptionRels(m *Manifest) map[string]bool {
	sep := string(filepath.Separator)
	out := map[string]bool{}
	for _, ex := range m.Exceptions {
		p := filepath.Clean(ex.Path)
		if p == "." || p == ".." || filepath.IsAbs(p) || strings.HasPrefix(p, ".."+sep) {
			continue
		}
		out[p] = true
	}
	return out
}

// RestoreExtras materializes every extra protected folder in m back to its
// absolute root (in place). No-op when the manifest has none. Deliberately
// materialize-only (no pruning): extra roots are live directories outside the
// workspace, where "absent from the manifest" is far weaker evidence of
// unwantedness than it is inside the project.
func RestoreExtras(m *Manifest, oc *objstore.Store) error {
	var roots []string
	for r := range m.Extra {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	for _, r := range roots {
		if err := restoreTree(m.Extra[r], oc, r); err != nil {
			return fmt.Errorf("extra root %s: %w", r, err)
		}
	}
	return nil
}

// safeJoin resolves rel under targetDir, refusing anything that escapes it.
// Manifests are built with filepath.Rel so entries are already contained; this
// is defense in depth against a hand-edited or hostile manifest.
func safeJoin(targetDir, rel string) (string, error) {
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", fmt.Errorf("refusing manifest entry %q: escapes the restore target", rel)
	}
	return filepath.Join(targetDir, clean), nil
}

// ensureDirPath makes every component of relDir under targetDir a REAL
// directory. A component that exists but is not a directory is removed first,
// notably a SYMLINK, which os.MkdirAll would happily follow, letting a restore
// write outside the target. The manifest is authoritative for the
// tree it describes: if it says this path is a directory, a symlink sitting
// there is stale state to replace, never a route to follow.
func ensureDirPath(targetDir, relDir string, mode fs.FileMode) error {
	if relDir == "." || relDir == "" {
		return os.MkdirAll(targetDir, 0o755)
	}
	cur := targetDir
	for _, comp := range strings.Split(filepath.Clean(relDir), string(filepath.Separator)) {
		if comp == "" || comp == "." {
			continue
		}
		cur = filepath.Join(cur, comp)
		fi, err := os.Lstat(cur)
		switch {
		case err == nil && fi.IsDir():
			// already a real directory
		case err == nil:
			if err := os.Remove(cur); err != nil { // symlink / file in the way
				return fmt.Errorf("restore: replacing non-directory %s: %w", cur, err)
			}
			if err := os.Mkdir(cur, mode); err != nil {
				return err
			}
		default:
			if err := os.Mkdir(cur, mode); err != nil && !os.IsExist(err) {
				return err
			}
		}
	}
	return nil
}

// clearDst removes whatever currently occupies dst so a file/symlink entry is
// written to the path itself rather than THROUGH an existing symlink (which
// would escape the target) or onto a directory.
func clearDst(dst string) error {
	fi, err := os.Lstat(dst)
	if err != nil {
		return nil // absent: nothing to clear
	}
	if fi.IsDir() {
		return os.RemoveAll(dst)
	}
	return os.Remove(dst)
}

func restoreTree(entries map[string]Entry, oc *objstore.Store, targetDir string) error {
	// A symlinked target root is refused for the same reason a symlinked
	// protected root is: every write (and, for RestoreExact, every removal)
	// would land somewhere other than the path the user named.
	if err := checkNotSymlinkRoot("restore target", targetDir); err != nil {
		return err
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}
	// dirs first, shallowest to deepest
	var dirs []string
	for rel, e := range entries {
		if e.Kind == KindDir {
			dirs = append(dirs, rel)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.Count(dirs[i], "/") < strings.Count(dirs[j], "/") })
	for _, rel := range dirs {
		if _, err := safeJoin(targetDir, rel); err != nil {
			return err
		}
		if err := ensureDirPath(targetDir, rel, fs.FileMode(entries[rel].Mode)&os.ModePerm|0o700); err != nil {
			return err
		}
	}
	for rel, e := range entries {
		dst, err := safeJoin(targetDir, rel)
		if err != nil {
			return err
		}
		switch e.Kind {
		case KindFile:
			content, err := oc.Get(e.Ref)
			if err != nil {
				return fmt.Errorf("restore %s: %w", rel, err)
			}
			if err := ensureDirPath(targetDir, filepath.Dir(rel), 0o755); err != nil {
				return err
			}
			if err := clearDst(dst); err != nil { // never write through a symlink
				return err
			}
			if err := os.WriteFile(dst, content, fs.FileMode(e.Mode)); err != nil {
				return err
			}
			if err := os.Chmod(dst, fs.FileMode(e.Mode)); err != nil { // WriteFile is umask'd
				return err
			}
		case KindSymlink:
			if err := ensureDirPath(targetDir, filepath.Dir(rel), 0o755); err != nil {
				return err
			}
			if err := clearDst(dst); err != nil {
				return err
			}
			if err := os.Symlink(e.Link, dst); err != nil {
				return err
			}
		}
	}
	// re-apply dir modes after children exist
	for _, rel := range dirs {
		if err := os.Chmod(filepath.Join(targetDir, rel), fs.FileMode(entries[rel].Mode)&os.ModePerm); err != nil {
			return err
		}
	}
	return nil
}

func unixMode(m fs.FileMode) uint32 {
	bits := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}
