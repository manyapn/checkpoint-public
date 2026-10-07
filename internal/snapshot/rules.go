package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Rebuildable bulk that is never captured. git keeps .git safe itself.
var excludedDirs = map[string]bool{
	".git": true, "node_modules": true, "build": true, "target": true,
	"dist": true, "__pycache__": true, ".venv": true,
}

func ExcludedDir(name string) bool { return excludedDirs[name] }

func excludedRel(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if excludedDirs[seg] {
			return true
		}
	}
	return false
}

const secretReason = "not captured: looks like credential material"

var (
	secretDirs = map[string]bool{
		".ssh": true, ".aws": true, ".gnupg": true, ".docker": true,
		".mozilla": true, ".thunderbird": true, "google-chrome": true, "chromium": true,
		"BraveSoftware": true, "Microsoft Edge": true, "vivaldi": true,
	}
	secretDirsUnderConfig = map[string]bool{"gh": true, "gcloud": true, "op": true, "doctl": true}
	secretNames           = map[string]bool{
		".netrc": true, ".pgpass": true, ".npmrc": true, ".pypirc": true,
		"id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true, "credentials": true,
	}
	secretExts = map[string]bool{".pem": true, ".key": true, ".p12": true, ".pfx": true, ".jks": true, ".keystore": true}
)

// IsSecret matches on names, never content, and errs toward skipping: a
// skipped file shows up as a named exception, a captured key is a leak.
func IsSecret(path string) bool {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	for i, p := range parts {
		if secretDirs[p] || (secretDirsUnderConfig[p] && i > 0 && parts[i-1] == ".config") {
			return true
		}
	}
	base := filepath.Base(path)
	return secretNames[base] || secretExts[strings.ToLower(filepath.Ext(base))] ||
		base == ".env" || strings.HasPrefix(base, ".env.")
}

// A symlinked root walks as a single entry, which would produce an empty
// checkpoint that still looks healthy.
func refuseSymlinkRoot(root string) error {
	fi, err := os.Lstat(root)
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("snapshot: %s is a symlink; pass the real path", root)
	}
	return nil
}

// CheckStoreLocation refuses a store inside the project (or vice versa),
// comparing real paths so a symlink cannot sneak one in.
func CheckStoreLocation(storeDir, root string) error {
	s, r := realPath(storeDir), realPath(root)
	switch {
	case s == r:
		return fmt.Errorf("store %s cannot be the protected folder itself", storeDir)
	case strings.HasPrefix(s, r+"/"):
		return fmt.Errorf("store %s is inside the protected folder %s; deleting the project would delete its history", storeDir, root)
	case strings.HasPrefix(r, s+"/"):
		return fmt.Errorf("protected folder %s is inside the store %s", root, storeDir)
	}
	return nil
}

func realPath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	parent, base := filepath.Split(p)
	if parent == "" || parent == p {
		return p
	}
	return filepath.Join(realPath(filepath.Clean(parent)), base)
}

// Stamp records which project a store belongs to, so two projects can never
// be mixed into one history. A store stamped for another project is refused.
func Stamp(storeDir, root string) error {
	path := filepath.Join(storeDir, "root")
	existing, err := os.ReadFile(path)
	if err == nil {
		if strings.TrimSpace(string(existing)) != root {
			return fmt.Errorf("store %s belongs to %s, not %s; use a different --store", storeDir, strings.TrimSpace(string(existing)), root)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(root+"\n"), 0o600)
}
