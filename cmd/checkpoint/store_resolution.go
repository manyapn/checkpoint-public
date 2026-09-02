package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/manyapn/checkpoint-public/internal/store"
)

// resolveStore returns the store directory: the explicit flag if set, otherwise
// a per-project area under $XDG_DATA_HOME/checkpoint keyed by the project's
// absolute path (base name + short path hash), so distinct projects never share
// a store and the same project always resolves to the same one.
// Every command resolves its store here, so the out-of-tree invariant is
// enforced once, at the choke point: an explicitly configured --store that
// lives inside the protected/target folder (or contains it) is REFUSED. An
// in-tree store would also be captured as project content, and it lets
// `rm -rf project` destroy the recovery data along with the project, which is
// the exact failure the whole design exists to prevent.
func resolveStore(flagVal, projectPath string) (string, error) {
	if flagVal != "" {
		abs, err := filepath.Abs(flagVal)
		if err != nil {
			return "", err
		}
		project, err := filepath.Abs(projectPath)
		if err != nil {
			return "", err
		}
		if err := store.CheckStoreLocation(abs, project); err != nil {
			return "", err
		}
		return abs, nil
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	sum := sha256.Sum256([]byte(projectPath))
	key := filepath.Base(projectPath) + "-" + hex.EncodeToString(sum[:])[:12]
	def := filepath.Join(dataHome, "checkpoint", key)
	// The default can only land in-tree if the project contains $XDG_DATA_HOME
	// (or $HOME); refuse that too rather than silently protecting nothing.
	project, err := filepath.Abs(projectPath)
	if err != nil {
		return "", err
	}
	if err := store.CheckStoreLocation(def, project); err != nil {
		return "", err
	}
	return def, nil
}

// requireStoreFor binds a store to the workspace it protects. A store stamped
// for another project must not be written to or read as this project's history:
// mixing two projects into one history makes "restore checkpoint N" restore
// someone else's tree. Mutating and workspace-semantic commands call
// this; `restore` deliberately does NOT, because restoring a checkpoint into a
// fresh directory is a legitimate, non-destructive use of a foreign store.
func requireStoreFor(storeDir, workspace string) error {
	meta, err := store.EnsureMeta(storeDir, workspace)
	if err != nil {
		return err
	}
	return store.CheckWorkspaceIdentity(meta, workspace)
}

// warnStoreFor is the read-only counterpart: a mismatch is surfaced but does
// not block diagnostics (history/status must stay usable for figuring out what
// a store actually holds).
func warnStoreFor(storeDir, workspace string) {
	meta, err := store.LoadMeta(storeDir)
	if err != nil {
		return
	}
	if err := store.CheckWorkspaceIdentity(meta, workspace); err != nil {
		fmt.Fprintf(os.Stderr, "%s: WARNING: %v\n", prog(), err)
	}
}
