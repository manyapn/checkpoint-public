package store

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/manyapn/checkpoint-public/internal/objstore"
)

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
