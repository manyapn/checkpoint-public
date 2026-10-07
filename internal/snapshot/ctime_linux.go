package snapshot

import (
	"io/fs"
	"syscall"
)

func ctimeNS(fi fs.FileInfo) int64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return st.Ctim.Sec*1e9 + st.Ctim.Nsec
}
