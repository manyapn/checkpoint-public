package doctor

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/manyapn/checkpoint-public/internal/watch"
)

const needRoot = "run under sudo, or grant the binary the capability once: sudo setcap cap_sys_admin+ep $(command -v checkpoint). In Docker, start the container with --cap-add SYS_ADMIN"

func kernelCheck() Check {
	c := Check{Name: "kernel"}
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		c.Detail = "could not read the kernel version"
		return c
	}
	release := string(u.Release[:strings.IndexByte(string(u.Release[:]), 0)])
	major, minor := parseVersion(release)
	switch {
	case major < 5 || (major == 5 && minor < 1):
		c.Fatal = true
		c.Detail = "Linux " + release + " is too old; checkpoint needs 5.1 or newer"
		c.Remedy = "upgrade the kernel"
	case major == 5 && minor < 17:
		c.OK = true
		c.Detail = "Linux " + release + " supports capture; the change feed needs 5.17+, so deletions are not attributed"
	default:
		c.OK = true
		c.Detail = "Linux " + release
	}
	return c
}

func parseVersion(release string) (major, minor int) {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return 0, 0
	}
	major, _ = strconv.Atoi(parts[0])
	digits := strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' })
	minor, _ = strconv.Atoi(digits)
	return major, minor
}

// fanotifyCheck arms a watch on the workspace itself and writes a probe file
// there, because some filesystems (Docker Desktop's shared macOS folders)
// accept the mark and then never report a write.
func fanotifyCheck(root string) Check {
	c := Check{Name: "fanotify capability", Fatal: true}
	w, err := watch.OpenWrites(root)
	switch {
	case err == nil:
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
		c.Detail = "permission denied; checkpoint needs CAP_SYS_ADMIN to watch the filesystem"
		c.Remedy = needRoot
		return c
	case errors.Is(err, unix.ENOSYS):
		c.Detail = "this kernel has no fanotify support"
		c.Remedy = "use a kernel built with CONFIG_FANOTIFY=y (every mainstream distro kernel is)"
		return c
	default:
		c.Detail = err.Error()
		c.Remedy = "report this error with `uname -r` and the filesystem type"
		return c
	}
	defer w.Close()
	probe, err := os.CreateTemp(root, ".checkpoint-doctor-")
	if err != nil {
		c.Detail = "could not write a probe file in the workspace: " + err.Error()
		c.Remedy = "fix permissions on " + root
		return c
	}
	probe.WriteString("probe\n")
	probe.Close()
	os.Remove(probe.Name())
	seen := false
	for deadline := time.Now().Add(time.Second); !seen && time.Now().Before(deadline); {
		watch.Poll(100, w.Fd())
		w.Drain(func(path string, fd int, pid int) { seen = seen || path == probe.Name() })
	}
	if !seen {
		c.Detail = "the kernel accepted the watch but never reported a write in " + root + "; writes on this filesystem are invisible to checkpoint"
		c.Remedy = "keep the project on a local ext4/xfs/btrfs filesystem. In Docker Desktop, a folder shared from macOS does not work; use a volume or a directory inside the container"
		return c
	}
	c.OK = true
	c.Detail = "a write in the workspace was reported by the kernel"
	return c
}

func filesystemCheck(root string) Check {
	c := Check{Name: "workspace filesystem"}
	var st unix.Statfs_t
	name := "unknown"
	if unix.Statfs(root, &st) == nil {
		name = fsName(uint32(st.Type))
	}
	ch, err := watch.OpenChanges(root)
	switch {
	case err == nil:
		ch.Close()
		c.OK = true
		c.Detail = name + ": change feed available (deletions are attributed; checkpoints scale with changes)"
	case errors.Is(err, watch.ErrUnsupported):
		c.Detail = name + ": no change feed, so deletions are not attributed and every checkpoint scans the whole tree"
		c.Remedy = "nothing to fix on " + name + "; for delete attribution keep the project on ext4/xfs/btrfs (in containers, bind-mount it from the host)"
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
		c.Detail = name + ": not probed, fanotify is not permitted for this process"
		c.Remedy = needRoot
	default:
		c.Detail = fmt.Sprintf("%s: change feed unavailable (%v)", name, err)
	}
	return c
}

func fsName(magic uint32) string {
	switch magic {
	case 0xEF53:
		return "ext4"
	case 0x58465342:
		return "xfs"
	case 0x9123683E:
		return "btrfs"
	case 0x794c7630:
		return "overlayfs"
	case 0x01021994:
		return "tmpfs"
	case 0x6969:
		return "nfs"
	case 0x65735546:
		return "fuse"
	case 0x01021997:
		return "9p"
	case 0x6a656a63:
		return "fakeowner (Docker Desktop shared folder)"
	}
	return fmt.Sprintf("filesystem %#x", magic)
}
