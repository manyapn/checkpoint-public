//go:build linux

package store

import (
	"io/fs"
	"syscall"
)

// ctimeNS is the inode change time in nanoseconds, or 0 when the platform does
// not expose one. The kernel bumps it on every write and there is no syscall to
// set it, which is exactly why the reuse key needs it (see Entry.CtimeNS).
func ctimeNS(fi fs.FileInfo) int64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return st.Ctim.Sec*1e9 + st.Ctim.Nsec
}
