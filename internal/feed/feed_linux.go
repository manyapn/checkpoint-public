//go:build linux

// Package feed is checkpoint's dirent change-feed: a FID-mode fanotify group
// (filesystem mark) that reports CREATE/DELETE/RENAME/CLOSE_WRITE with the
// acting pid and the entry NAME, resolved to absolute paths via the parent-dir
// file handle. Filesystem marks work on ext4 and friends; overlayfs rejects
// them outright, hence the degrade below.
//
// It exists for two things the fd-class capture group cannot do:
//   - DELETE PROVENANCE: a deletion event names what died and who deleted it,
//     so an agent-deleted file becomes restorable by undo.
//   - O(changes) boundaries: the set of changed paths since the last checkpoint
//     lets the daemon fold a manifest instead of re-walking the whole tree.
//
// The feed is an ENHANCEMENT with a declared degrade: on filesystems that
// refuse FAN_MARK_FILESYSTEM or handle resolution (overlayfs), New returns
// ErrUnsupported and the daemon stays in scan mode (correct, just O(tree) and
// without delete provenance).
package feed

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ErrUnsupported: this filesystem cannot host the feed (degrade to scan mode).
var ErrUnsupported = errors.New("feed: unsupported on this filesystem")

const (
	fanRename       = 0x10000000 // FAN_RENAME (stable ABI, kernel >= 5.17)
	infoDFIDName    = 2          // FAN_EVENT_INFO_TYPE_DFID_NAME
	infoOldDFIDName = 10         // FAN_EVENT_INFO_TYPE_OLD_DFID_NAME
	infoNewDFIDName = 12         // FAN_EVENT_INFO_TYPE_NEW_DFID_NAME
	metaSize        = 24
)

// Op is what happened to a path.
type Op string

const (
	OpCreate     Op = "create"
	OpWrite      Op = "write"
	OpDelete     Op = "delete"
	OpRenameFrom Op = "rename-from" // the old name (content left this path)
	OpRenameTo   Op = "rename-to"   // the new name
)

// Event is one dirent change under a protected root.
type Event struct {
	Op   Op
	Path string // absolute
	Pid  int32  // acting process (listener pid ns)
	Dir  bool   // the object is a directory
}

// Feed drains dirent events for a set of protected roots. Single-goroutine use,
// like capture.Watcher.
type Feed struct {
	fan        int
	buf        []byte
	roots      []string
	mounts     []int             // mount fds anchoring open_by_handle_at, one per root
	dirCache   map[string]string // handleKey -> resolved dir path
	overflowed bool
}

// New arms the feed for roots (all absolute). Returns ErrUnsupported (wrapped)
// when the filesystem refuses filesystem marks; the caller degrades to scan
// mode. A feed that arms successfully still verifies handle resolution lazily;
// per-event resolution failures surface as dropped events via Overflow-like
// honesty (the daemon treats resolution failure as a full-scan trigger).
func New(roots []string) (*Feed, error) {
	fan, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_REPORT_FID|
		unix.FAN_REPORT_DIR_FID|unix.FAN_REPORT_NAME|unix.FAN_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("feed: fanotify_init: %w", err)
	}
	mask := uint64(unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_CLOSE_WRITE |
		fanRename | unix.FAN_ONDIR)
	f := &Feed{fan: fan, buf: make([]byte, 256*1024), roots: roots, dirCache: map[string]string{}}
	for _, r := range roots {
		if err := unix.FanotifyMark(fan, unix.FAN_MARK_ADD|unix.FAN_MARK_FILESYSTEM,
			mask, unix.AT_FDCWD, r); err != nil {
			f.Close()
			if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) ||
				errors.Is(err, unix.ENODEV) || errors.Is(err, unix.EXDEV) {
				return nil, fmt.Errorf("%w: mark %s: %v", ErrUnsupported, r, err)
			}
			return nil, fmt.Errorf("feed: mark %s: %w", r, err)
		}
		mfd, err := unix.Open(r, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("feed: open mount fd %s: %w", r, err)
		}
		f.mounts = append(f.mounts, mfd)
	}
	return f, nil
}

// Drain reads and decodes every queued dirent event, returning those under a
// protected root. An unresolvable parent handle or a kernel queue overflow sets
// Overflowed, and the daemon must fall back to a full scan for the next boundary
// (a change-set with a known hole must not be trusted for folding).
func (f *Feed) Drain() []Event {
	var out []Event
	for {
		n, err := unix.Read(f.fan, f.buf)
		if err != nil || n <= 0 {
			return out
		}
		for off := 0; off+metaSize <= n; {
			evLen := int(binary.LittleEndian.Uint32(f.buf[off : off+4]))
			if evLen < metaSize || off+evLen > n {
				break
			}
			rec := f.buf[off : off+evLen]
			mask := binary.LittleEndian.Uint64(rec[8:16])
			pid := int32(binary.LittleEndian.Uint32(rec[20:24]))
			if mask&unix.FAN_Q_OVERFLOW != 0 {
				f.overflowed = true
				off += evLen
				continue
			}
			out = append(out, f.decode(mask, pid, rec)...)
			off += evLen
		}
	}
}

// decode turns one event record into 0+ Events (a rename yields from+to).
func (f *Feed) decode(mask uint64, pid int32, rec []byte) []Event {
	isDir := mask&unix.FAN_ONDIR != 0
	var out []Event
	emit := func(op Op, typ uint8) {
		for _, in := range parseInfos(rec) {
			if in.infoType != typ || in.name == "" {
				continue
			}
			parent, ok := f.resolveDir(in.handleType, in.handle)
			if !ok {
				f.overflowed = true // a hole in the change-set: force full scan
				continue
			}
			p := filepath.Join(parent, in.name)
			if !f.protected(p) {
				continue
			}
			if isDir {
				// A directory changed shape: cached descendant paths may be stale.
				f.dirCache = map[string]string{}
			}
			out = append(out, Event{Op: op, Path: p, Pid: pid, Dir: isDir})
		}
	}
	if mask&fanRename != 0 {
		emit(OpRenameFrom, infoOldDFIDName)
		emit(OpRenameTo, infoNewDFIDName)
		return out
	}
	if mask&unix.FAN_CREATE != 0 {
		emit(OpCreate, infoDFIDName)
	}
	if mask&unix.FAN_DELETE != 0 {
		emit(OpDelete, infoDFIDName)
	}
	if mask&unix.FAN_CLOSE_WRITE != 0 {
		emit(OpWrite, infoDFIDName)
	}
	return out
}

// resolveDir turns a parent-directory handle into its absolute path, caching by
// handle bytes (stable per dir; the cache is flushed on directory-shape events).
func (f *Feed) resolveDir(handleType int32, handle []byte) (string, bool) {
	key := fmt.Sprintf("%d:%x", handleType, handle)
	if p, ok := f.dirCache[key]; ok {
		return p, true
	}
	buf := make([]byte, 8+len(handle))
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(handle)))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(handleType))
	copy(buf[8:], handle)
	for _, mfd := range f.mounts {
		fd, _, errno := unix.Syscall(unix.SYS_OPEN_BY_HANDLE_AT,
			uintptr(mfd), uintptr(unsafe.Pointer(&buf[0])), uintptr(unix.O_RDONLY))
		if errno != 0 {
			continue
		}
		p, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
		unix.Close(int(fd))
		if err != nil {
			continue
		}
		f.dirCache[key] = p
		return p, true
	}
	return "", false
}

func (f *Feed) protected(path string) bool {
	for _, r := range f.roots {
		if path == r || strings.HasPrefix(path, r+"/") {
			return true
		}
	}
	return false
}

// Overflowed reports whether this window's change-set has a known hole (kernel
// overflow or an unresolvable event). The next boundary must full-scan.
func (f *Feed) Overflowed() bool { return f.overflowed }

// ResetWindow clears the overflow flag after a boundary consumed the window.
func (f *Feed) ResetWindow() { f.overflowed = false }

// Fd exposes the fanotify fd for poll multiplexing.
func (f *Feed) Fd() int { return f.fan }

// Close releases the group and mount fds.
func (f *Feed) Close() error {
	for _, m := range f.mounts {
		unix.Close(m)
	}
	unix.Close(f.fan)
	return nil
}

// parseInfos walks the variable-length info records trailing an event's fixed
// metadata, extracting the dirent-name records plus the handle_type that
// open_by_handle_at needs. Every length is bounds-checked against the record:
// the buffer is kernel-supplied but decoding is done with raw offsets, so a
// short or truncated record must stop the scan rather than read past it.
func parseInfos(rec []byte) []finfo {
	metaLen := int(binary.LittleEndian.Uint16(rec[6:8]))
	var out []finfo
	p := metaLen
scan:
	for p+4 <= len(rec) {
		infoType := rec[p]
		l := int(binary.LittleEndian.Uint16(rec[p+2 : p+4]))
		if l == 0 || p+l > len(rec) {
			break
		}
		switch infoType {
		case infoDFIDName, infoOldDFIDName, infoNewDFIDName:
			if p+20 > len(rec) {
				break scan
			}
			hb := int(binary.LittleEndian.Uint32(rec[p+12 : p+16]))
			ht := int32(binary.LittleEndian.Uint32(rec[p+16 : p+20]))
			hStart := p + 20
			if hStart+hb > p+l {
				break scan
			}
			handle := append([]byte(nil), rec[hStart:hStart+hb]...)
			name := string(rec[hStart+hb : p+l])
			if i := strings.IndexByte(name, 0); i >= 0 {
				name = name[:i]
			}
			out = append(out, finfo{infoType: infoType, handleType: ht, handle: handle, name: name})
		}
		p += l
	}
	return out
}

type finfo struct {
	infoType   uint8
	handleType int32
	handle     []byte
	name       string
}
