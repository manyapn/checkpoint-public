//go:build linux

package doctor

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/manyapn/checkpoint-public/internal/feed"
)

// The kernel floors checkpoint depends on. These are ABI facts, and they are
// used only to EXPLAIN a probe result, never as a substitute for probing.
//
//	2.6.37  fanotify (FAN_CLASS_NOTIF), mount marks, FAN_CLOSE_WRITE
//	         -> this is what internal/capture arms to record every write.
//	5.1     FAN_REPORT_FID           -> checkpoint's declared supported floor
//	5.9     FAN_REPORT_DIR_FID | FAN_REPORT_NAME
//	5.17    FAN_RENAME (one event carrying old+new)
//	         -> 5.9+5.17 are what internal/feed needs: the change feed.
const (
	minKernelMajor, minKernelMinor   = 5, 1  // supported floor
	feedKernelMajor, feedKernelMinor = 5, 17 // full change-feed support
)

func probeKernel() Check {
	c := Check{Name: "kernel"}
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		c.Detail = fmt.Sprintf("could not read the kernel version: %v (checkpoint needs Linux %d.%d or newer)",
			err, minKernelMajor, minKernelMinor)
		c.Remedy = "run `uname -r` and compare by hand; the capability probe below is the authoritative test"
		return c
	}
	release := utsString(u.Release[:])
	major, minor, ok := parseKernelVersion(release)
	if !ok {
		c.Detail = fmt.Sprintf("found %q, which does not parse as a Linux version (checkpoint needs %d.%d or newer)",
			release, minKernelMajor, minKernelMinor)
		c.Remedy = "ignore this line if the capability probe below passed; that probe is the real test"
		return c
	}
	switch {
	case older(major, minor, minKernelMajor, minKernelMinor):
		c.Fatal = true
		c.Detail = fmt.Sprintf("Linux %s is too old: checkpoint needs %d.%d or newer (fanotify FAN_REPORT_FID)",
			release, minKernelMajor, minKernelMinor)
		c.Remedy = fmt.Sprintf("upgrade the kernel to %d.%d+ (any current distro kernel), or run checkpoint on a newer host",
			minKernelMajor, minKernelMinor)
	case older(major, minor, feedKernelMajor, feedKernelMinor):
		c.OK = true
		c.Detail = fmt.Sprintf("Linux %s supports capture (needs %d.%d+); the dirent change feed needs %d.%d+, so deletions are not attributed and checkpoints use full scans",
			release, minKernelMajor, minKernelMinor, feedKernelMajor, feedKernelMinor)
	default:
		c.OK = true
		c.Detail = fmt.Sprintf("Linux %s is supported (needs %d.%d+; change feed needs %d.%d+)",
			release, minKernelMajor, minKernelMinor, feedKernelMajor, feedKernelMinor)
	}
	return c
}

// capabilityRemedy is the one remedy a stranger most needs to read, so it is
// spelled out in full: every way to obtain the capability, in order of how
// likely it is to be the right one.
const capabilityRemedy = "run checkpoint as root (`sudo checkpoint ...`), or grant the binary the capability once:\n" +
	"`sudo setcap cap_sys_admin+ep $(command -v checkpoint)`\n" +
	"inside Docker/Podman, start the container with `--cap-add SYS_ADMIN` (unprivileged containers block fanotify by default)"

// probeFanotify ARMS a real fanotify group on a throwaway temp directory and
// tears it down. Nothing short of this answers the question: kernel config,
// capabilities, seccomp filters, LSM policy and user namespaces all decide it,
// and only the syscall knows all four.
func probeFanotify() Check {
	c := Check{Name: "fanotify capability", Fatal: true}
	dir, err := os.MkdirTemp("", "checkpoint-doctor-")
	if err != nil {
		c.Detail = fmt.Sprintf("could not create a temp directory to probe with: %v", err)
		c.Remedy = "set TMPDIR to a writable directory and re-run"
		return c
	}
	defer os.RemoveAll(dir)

	fan, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK,
		unix.O_RDONLY|unix.O_CLOEXEC)
	if err != nil {
		c.Detail, c.Remedy = fanotifyFailure("fanotify_init", err)
		return c
	}
	defer unix.Close(fan)

	// The same mark capture uses: mount-wide, close-write, fd class.
	if err := unix.FanotifyMark(fan, unix.FAN_MARK_ADD|unix.FAN_MARK_MOUNT,
		unix.FAN_CLOSE_WRITE, unix.AT_FDCWD, dir); err != nil {
		c.Detail, c.Remedy = fanotifyFailure("fanotify_mark", err)
		return c
	}
	_ = unix.FanotifyMark(fan, unix.FAN_MARK_REMOVE|unix.FAN_MARK_MOUNT,
		unix.FAN_CLOSE_WRITE, unix.AT_FDCWD, dir)

	c.OK = true
	c.Detail = "fanotify armed and released successfully (a real group was created and marked)"
	return c
}

// fanotifyFailure translates the errno a stranger would otherwise see raw.
func fanotifyFailure(call string, err error) (detail, remedy string) {
	switch {
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
		return fmt.Sprintf("%s: permission denied (EPERM); checkpoint needs CAP_SYS_ADMIN to watch the filesystem", call),
			capabilityRemedy
	case errors.Is(err, unix.ENOSYS):
		return fmt.Sprintf("%s: not implemented (ENOSYS); this kernel has no fanotify support (CONFIG_FANOTIFY is off)", call),
			"use a kernel built with CONFIG_FANOTIFY=y (all mainstream distro kernels are); checkpoint cannot capture writes without it"
	case errors.Is(err, unix.ENOMEM):
		return fmt.Sprintf("%s: out of memory (ENOMEM); the per-user fanotify group limit may be exhausted", call),
			"raise /proc/sys/fs/fanotify/max_user_groups, or stop other fanotify users (antivirus, backup agents)"
	case errors.Is(err, unix.EMFILE):
		return fmt.Sprintf("%s: limit reached (EMFILE); too many fanotify groups or open files for this user", call),
			"raise /proc/sys/fs/fanotify/max_user_groups and `ulimit -n`, or stop an existing checkpoint daemon"
	default:
		return fmt.Sprintf("%s failed: %v", call, err),
			"report this errno with `uname -r` and the filesystem type; checkpoint cannot capture writes until fanotify can be armed"
	}
}

// probeFilesystem names the workspace filesystem and ARMS the dirent change
// feed on it. Feed availability is not fatal: capture (the durability core)
// does not depend on it. What is lost is named exactly.
func probeFilesystem(workspace string, valid bool) Check {
	c := Check{Name: "workspace filesystem"}
	if !valid {
		c.Detail = "not checked: the workspace path above is unusable"
		c.Remedy = "fix the workspace path, then re-run"
		return c
	}
	fsName, _, _, serr := fsStat(workspace)
	if serr != nil {
		fsName = "unknown"
	}

	f, err := feed.New([]string{workspace})
	switch {
	case err == nil:
		f.Close()
		c.OK = true
		c.Detail = fmt.Sprintf("%s: change feed available (deletions are attributed; checkpoints scale with changes, not tree size)", fsName)
	case errors.Is(err, feed.ErrUnsupported):
		c.Detail = fmt.Sprintf("%s: no dirent change feed here, because the kernel refuses a filesystem-wide mark. Capture and restore still work; what is lost is that deletions are not attributed (an agent-deleted file is not undone) and every checkpoint does a full tree scan (slower on large trees)", fsName)
		c.Remedy = fmt.Sprintf("nothing to fix, because this is a property of %s. For delete attribution, keep the project on ext4/xfs/btrfs (in containers, bind-mount the project from the host instead of working on the container's overlay layer)", fsName)
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
		c.Detail = fmt.Sprintf("%s: change feed not probed, because fanotify is not permitted for this process", fsName)
		c.Remedy = capabilityRemedy
	default:
		c.Detail = fmt.Sprintf("%s: change feed unavailable (%v). Capture and restore still work; deletions are not attributed and checkpoints use full scans", fsName, err)
		c.Remedy = "no action needed for basic protection; report this error if you expected delete attribution on this filesystem"
	}
	return c
}

// fsStat reports the filesystem type name and byte capacity of the filesystem
// holding path.
func fsStat(path string) (fsType string, free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "", 0, 0, err
	}
	bs := uint64(st.Bsize)
	// st.Type is int64 on 64-bit arches and int32 on 32-bit ones; every known
	// magic fits in 32 bits, so narrow then widen (avoids sign extension).
	return fsName(uint64(uint32(st.Type))), st.Bavail * bs, st.Blocks * bs, nil
}

// fsName maps a statfs magic to a name a user would recognize. Unknown magics
// are reported as the hex magic rather than guessed at.
func fsName(magic uint64) string {
	switch magic {
	case 0xEF53:
		return "ext2/ext3/ext4"
	case 0x58465342:
		return "xfs"
	case 0x9123683E:
		return "btrfs"
	case 0x794c7630:
		return "overlayfs"
	case 0x01021994:
		return "tmpfs"
	case 0x858458F6:
		return "ramfs"
	case 0x2FC12FC1:
		return "zfs"
	case 0xF2F52010:
		return "f2fs"
	case 0xCA451A4E:
		return "bcachefs"
	case 0x6969:
		return "nfs"
	case 0x65735546:
		return "fuse"
	case 0xFF534D42:
		return "cifs/smb"
	case 0x01021997:
		return "9p (VM shared folder)"
	case 0x61756673:
		return "aufs"
	case 0xF15F:
		return "ecryptfs"
	case 0x73717368:
		return "squashfs (read-only)"
	case 0x4D44:
		return "vfat"
	case 0x2011BAB0:
		return "exfat"
	case 0x52654973:
		return "reiserfs"
	case 0x3153464A:
		return "jfs"
	case 0x24051905:
		return "ubifs"
	default:
		return fmt.Sprintf("unrecognized filesystem (statfs magic %#x)", magic)
	}
}

func utsString(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// parseKernelVersion pulls major.minor out of a release string such as
// "6.12.76-linuxkit" or "5.15.0-91-generic".
func parseKernelVersion(release string) (major, minor int, ok bool) {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minorField := parts[1]
	for i, r := range minorField {
		if r < '0' || r > '9' {
			minorField = minorField[:i]
			break
		}
	}
	minor, err = strconv.Atoi(minorField)
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

func older(major, minor, wantMajor, wantMinor int) bool {
	return major < wantMajor || (major == wantMajor && minor < wantMinor)
}
