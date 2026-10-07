// Package watch is the thin layer over fanotify. Writes reports every file
// close after a write, with an open descriptor to the file, so content can be
// read even if the file was deleted right after. Changes reports names
// created, deleted and renamed, which Writes cannot see.
package watch

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const eventSize = 24 // sizeof(struct fanotify_event_metadata)

type Writes struct {
	fan        int
	buf        []byte
	Overflowed bool
}

// OpenWrites marks the whole mount, so files in directories created later
// are covered too. Needs CAP_SYS_ADMIN.
func OpenWrites(root string) (*Writes, error) {
	fan, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK, unix.O_RDONLY|unix.O_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("fanotify_init (needs CAP_SYS_ADMIN): %w", err)
	}
	if err := unix.FanotifyMark(fan, unix.FAN_MARK_ADD|unix.FAN_MARK_MOUNT, unix.FAN_CLOSE_WRITE, unix.AT_FDCWD, root); err != nil {
		unix.Close(fan)
		return nil, fmt.Errorf("fanotify_mark %s: %w", root, err)
	}
	return &Writes{fan: fan, buf: make([]byte, 256*1024)}, nil
}

func (w *Writes) Fd() int { return w.fan }

// Drain hands every queued close-write to handle. The descriptor is closed
// after handle returns.
func (w *Writes) Drain(handle func(path string, fd int, pid int)) {
	for {
		n, err := unix.Read(w.fan, w.buf)
		if err != nil || n <= 0 {
			return
		}
		for off := 0; off+eventSize <= n; {
			length := int(binary.LittleEndian.Uint32(w.buf[off:]))
			if length < eventSize || off+length > n {
				return
			}
			mask := binary.LittleEndian.Uint64(w.buf[off+8:])
			fd := int(int32(binary.LittleEndian.Uint32(w.buf[off+16:])))
			pid := int(binary.LittleEndian.Uint32(w.buf[off+20:]))
			off += length
			if mask&unix.FAN_Q_OVERFLOW != 0 {
				w.Overflowed = true
				continue
			}
			if fd < 0 {
				continue
			}
			if mask&unix.FAN_CLOSE_WRITE != 0 {
				if link, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd)); err == nil {
					handle(strings.TrimSuffix(link, " (deleted)"), fd, pid)
				}
			}
			unix.Close(fd)
		}
	}
}

func (w *Writes) Close() { unix.Close(w.fan) }

// Poll waits up to timeout ms for any of the descriptors to become readable.
func Poll(timeoutMs int, fds ...int) bool {
	pfds := make([]unix.PollFd, len(fds))
	for i, fd := range fds {
		pfds[i] = unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}
	}
	n, err := unix.Poll(pfds, timeoutMs)
	return err == nil && n > 0
}
