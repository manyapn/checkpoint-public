//go:build !linux

package lineage

func StartTime(pid int) (uint64, bool) { return 0, false }
