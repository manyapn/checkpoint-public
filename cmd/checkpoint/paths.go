package main

import (
	"fmt"
	"path/filepath"

	"github.com/manyapn/checkpoint-public/internal/daemon"
)

// resolveDir turns a user-supplied directory argument into an absolute path
// with symlinks resolved. A symlinked path is an alias for its target
// everywhere in this CLI: leaving it unresolved lets a symlinked project root
// snapshot NOTHING while reporting "Fully recoverable", and lets a store that
// resolves inside the project slip past the in-tree guard.
// A path that does not exist yet keeps its (cleaned, absolute) form, because
// restore targets are allowed not to exist.
func resolveDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs, nil //nolint:nilerr // absent path: nothing to resolve
	}
	return resolved, nil
}

// checkSocketPath refuses a store whose socket path would exceed the kernel's
// sockaddr_un limit. The failure is otherwise a bare "bind: invalid argument"
// arriving after a 15-second readiness wait, which tells the user nothing.
func checkSocketPath(storeDir string) error {
	sock := daemon.SocketPath(storeDir)
	if len(sock)+1 > daemon.SunPathMax {
		return fmt.Errorf("store path is too long for a Unix socket: %s would need %d bytes and the "+
			"kernel allows %d.\n  use a shorter --store (e.g. --store %s)",
			sock, len(sock)+1, daemon.SunPathMax, filepath.Join("/tmp", "ckpt-"+filepath.Base(storeDir)))
	}
	return nil
}
