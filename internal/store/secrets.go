package store

import (
	"path/filepath"
	"strings"
)

// Secret material is NEVER captured: ~/.ssh, ~/.aws, ~/.config/gh, .env*,
// *.pem, *.key, browser profiles and friends. Without this, protecting a
// project would copy its .env and private keys verbatim into the object store.
// Copying a credential somewhere the user did not choose is a harm the tool
// must not cause in exchange for recoverability they did not ask for, so these
// paths are skipped, and skipped LOUDLY: each one becomes a named exception on
// the checkpoint, because silently not protecting a file is the failure mode
// this project exists to avoid.
//
// The trade is deliberate and stated: a skipped secret is not recoverable by
// checkpoint. Users who genuinely want a credential file protected can keep it
// outside the denylist's naming conventions; a configurable allowlist is
// deliberately not built: it would be a security switch, and there is no way to
// make one safe by default.
var (
	// Directory names whose entire subtree is credential material.
	secretDirs = map[string]bool{
		".ssh": true, ".aws": true, ".gnupg": true, ".docker": true,
		// Browser profiles hold saved passwords, cookies and session tokens.
		".mozilla": true, ".thunderbird": true,
		"google-chrome": true, "chromium": true, "BraveSoftware": true,
		"Microsoft Edge": true, "vivaldi": true,
	}
	// Directories that are credential stores only under a specific parent.
	// "gh" is the GitHub CLI's token directory under ~/.config, but a plain
	// project directory called "gh" is ordinary source and must stay protected.
	secretDirsUnder = map[string]string{
		"gh": ".config", "gcloud": ".config", "op": ".config", "doctl": ".config",
	}
	// Exact file names that are credentials wherever they appear.
	secretNames = map[string]bool{
		".netrc": true, ".pgpass": true, ".npmrc": true, ".pypirc": true,
		"id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true,
		"credentials": true, // ~/.aws/credentials and friends
	}
	// Extensions that are key material by convention.
	secretExts = map[string]bool{
		".pem": true, ".key": true, ".p12": true, ".pfx": true, ".jks": true, ".keystore": true,
	}
)

// IsSecretPath reports whether path is credential material that must never be
// captured. Matching is on names, not content: it is deliberately
// over-inclusive (a file called server.key is skipped even if it holds no key)
// because the cost of skipping a non-secret is a named exception the user can
// see, while the cost of capturing a real secret is a credential copied into
// storage they did not choose.
//
// `.env` is matched as a prefix so `.env`, `.env.local`, `.env.production` are
// all covered; `.envrc` (direnv config, not itself a secret store) is NOT
// matched, because it is normal project source people expect to be protected.
func IsSecretPath(path string) bool {
	comps := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	for i, comp := range comps {
		if comp == "" || comp == "." {
			continue
		}
		if secretDirs[comp] {
			return true
		}
		// Parent-qualified credential directories. The parent may be absent
		// when that parent IS the protected root (protecting ~/.config makes
		// the path "gh/hosts.yml"), so a leading match counts too. Callers
		// pass absolute paths where they have them, which removes the
		// ambiguity entirely.
		if parent, ok := secretDirsUnder[comp]; ok {
			if i == 0 || comps[i-1] == parent {
				return true
			}
		}
	}
	base := filepath.Base(path)
	if secretNames[base] {
		return true
	}
	if secretExts[strings.ToLower(filepath.Ext(base))] {
		return true
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return true
	}
	return false
}

// secretReason is the exception text recorded for a skipped secret, so the
// user can see exactly what is not being protected and why.
const secretReason = "not captured: looks like credential material (security default)"
