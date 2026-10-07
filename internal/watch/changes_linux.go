package watch

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

// ErrUnsupported: the filesystem refuses a filesystem-wide mark (overlayfs).
var ErrUnsupported = errors.New("change feed unsupported on this filesystem")

const (
	fanRename   = 0x10000000 // FAN_RENAME, kernel 5.17+
	infoDirName = 2          // FAN_EVENT_INFO_TYPE_DFID_NAME
	infoOldName = 10
	infoNewName = 12
	Created     = "create"
	Deleted     = "delete"
	Written     = "write"
	RenamedFrom = "rename-from"
	RenamedTo   = "rename-to"
)

type Change struct {
	Op   string
	Path string
	Pid  int
	Dir  bool
}

type Changes struct {
	fan        int
	mount      int
	root       string
	buf        []byte
	dirs       map[string]string // file handle -> directory path
	Overflowed bool
}

// OpenChanges reports directory entries changing anywhere under root. The
// kernel identifies the parent directory by file handle, which is resolved
// back to a path with open_by_handle_at.
func OpenChanges(root string) (*Changes, error) {
	fan, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_REPORT_FID|unix.FAN_REPORT_DIR_FID|unix.FAN_REPORT_NAME|unix.FAN_NONBLOCK|unix.FAN_UNLIMITED_QUEUE, 0)
	if err != nil {
		return nil, fmt.Errorf("fanotify_init: %w", err)
	}
	mask := uint64(unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_CLOSE_WRITE | fanRename | unix.FAN_ONDIR)
	if err := unix.FanotifyMark(fan, unix.FAN_MARK_ADD|unix.FAN_MARK_FILESYSTEM, mask, unix.AT_FDCWD, root); err != nil {
		unix.Close(fan)
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENODEV) || errors.Is(err, unix.EXDEV) {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		return nil, fmt.Errorf("fanotify_mark %s: %w", root, err)
	}
	mount, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(fan)
		return nil, err
	}
	return &Changes{fan: fan, mount: mount, root: root, buf: make([]byte, 256*1024), dirs: map[string]string{}}, nil
}

func (c *Changes) Fd() int { return c.fan }

func (c *Changes) Close() {
	unix.Close(c.mount)
	unix.Close(c.fan)
}

// Drain returns the queued changes under root. A kernel overflow or an
// unresolvable parent sets Overflowed: the change set has a hole and the
// next checkpoint must scan rather than fold.
func (c *Changes) Drain() []Change {
	var out []Change
	for {
		n, err := unix.Read(c.fan, c.buf)
		if err != nil || n <= 0 {
			return out
		}
		for off := 0; off+eventSize <= n; {
			length := int(binary.LittleEndian.Uint32(c.buf[off:]))
			if length < eventSize || off+length > n {
				return out
			}
			rec := c.buf[off : off+length]
			off += length
			mask := binary.LittleEndian.Uint64(rec[8:])
			pid := int(binary.LittleEndian.Uint32(rec[20:]))
			if mask&unix.FAN_Q_OVERFLOW != 0 {
				c.Overflowed = true
				continue
			}
			out = append(out, c.decode(mask, pid, rec)...)
		}
	}
}

// decode turns one kernel record into the changes it describes. A rename
// carries both names, so it yields two.
func (c *Changes) decode(mask uint64, pid int, rec []byte) []Change {
	infos := parseInfo(rec)
	isDir := mask&unix.FAN_ONDIR != 0
	var out []Change
	if mask&fanRename != 0 {
		out = append(out, c.named(RenamedFrom, infoOldName, infos, pid, isDir)...)
		return append(out, c.named(RenamedTo, infoNewName, infos, pid, isDir)...)
	}
	if mask&unix.FAN_CREATE != 0 {
		out = append(out, c.named(Created, infoDirName, infos, pid, isDir)...)
	}
	if mask&unix.FAN_DELETE != 0 {
		out = append(out, c.named(Deleted, infoDirName, infos, pid, isDir)...)
	}
	if mask&unix.FAN_CLOSE_WRITE != 0 {
		out = append(out, c.named(Written, infoDirName, infos, pid, isDir)...)
	}
	return out
}

// named resolves the records of one info type to paths under root.
func (c *Changes) named(op string, infoType byte, infos []info, pid int, isDir bool) []Change {
	var out []Change
	for _, in := range infos {
		if in.kind != infoType || in.name == "" {
			continue
		}
		dir, ok := c.resolveDir(in.handleType, in.handle)
		if !ok {
			c.Overflowed = true
			continue
		}
		path := filepath.Join(dir, in.name)
		if path != c.root && !strings.HasPrefix(path, c.root+"/") {
			continue
		}
		if isDir {
			c.dirs = map[string]string{} // a moved dir invalidates cached paths
		}
		out = append(out, Change{op, path, pid, isDir})
	}
	return out
}

func (c *Changes) resolveDir(handleType int32, handle []byte) (string, bool) {
	key := string(handle)
	if p, ok := c.dirs[key]; ok {
		return p, true
	}
	fh := make([]byte, 8+len(handle))
	binary.LittleEndian.PutUint32(fh[0:], uint32(len(handle)))
	binary.LittleEndian.PutUint32(fh[4:], uint32(handleType))
	copy(fh[8:], handle)
	fd, _, errno := unix.Syscall(unix.SYS_OPEN_BY_HANDLE_AT, uintptr(c.mount), uintptr(unsafe.Pointer(&fh[0])), uintptr(unix.O_RDONLY))
	if errno != 0 {
		return "", false
	}
	path, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	unix.Close(int(fd))
	if err != nil {
		return "", false
	}
	c.dirs[key] = path
	return path, true
}

type info struct {
	kind       byte
	handleType int32
	handle     []byte
	name       string
}

// parseInfo walks the variable-length records after the fixed header. Every
// length is bounds-checked: a short record stops the scan.
func parseInfo(rec []byte) []info {
	var out []info
	p := int(binary.LittleEndian.Uint16(rec[6:]))
	for p+4 <= len(rec) {
		kind := rec[p]
		length := int(binary.LittleEndian.Uint16(rec[p+2:]))
		if length == 0 || p+length > len(rec) {
			break
		}
		if kind == infoDirName || kind == infoOldName || kind == infoNewName {
			if p+20 > len(rec) {
				break
			}
			hlen := int(binary.LittleEndian.Uint32(rec[p+12:]))
			htype := int32(binary.LittleEndian.Uint32(rec[p+16:]))
			if p+20+hlen > p+length {
				break
			}
			name := string(rec[p+20+hlen : p+length])
			if i := strings.IndexByte(name, 0); i >= 0 {
				name = name[:i]
			}
			out = append(out, info{kind, htype, append([]byte(nil), rec[p+20:p+20+hlen]...), name})
		}
		p += length
	}
	return out
}
