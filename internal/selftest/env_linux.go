//go:build linux

package selftest

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// kernelRelease is the `uname -r` string. It goes in the report verbatim: the
// single most useful fact in a filesystem-watcher bug report.
func kernelRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "unknown: " + err.Error()
	}
	rel := utsString(u.Release[:])
	ver := utsString(u.Version[:])
	if ver != "" {
		return rel + " (" + ver + ")"
	}
	return rel
}

// filesystemName names the filesystem holding path. Which filesystem the
// workspace and the store live on decides whether the change feed can arm and
// therefore which guarantees apply, so it is reported for both.
func filesystemName(path string) string {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "unknown: " + err.Error()
	}
	// st.Type is int64 on 64-bit arches and int32 on 32-bit ones; every known
	// magic fits in 32 bits, so narrow then widen to avoid sign extension.
	return fsMagicName(uint64(uint32(st.Type)))
}

// fsMagicName maps a statfs magic to a name a user recognises. It mirrors the
// table in internal/doctor deliberately: selftest is a black-box driver of
// the shipped binary and imports no checkpoint internals, so that a broken
// internal package cannot make the self-test report itself healthy. An
// unrecognised magic is printed as hex, never guessed at.
func fsMagicName(magic uint64) string {
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
