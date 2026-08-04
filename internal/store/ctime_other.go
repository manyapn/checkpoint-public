//go:build !linux

package store

import "io/fs"

// ctimeNS has no portable answer off Linux. Returning 0 makes every file
// ineligible for stat-only reuse (see reusable), so a non-Linux build rereads
// and rehashes instead of trusting a forgeable size+mtime pair.
func ctimeNS(fs.FileInfo) int64 { return 0 }
