package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIsSecretPathMatchesCredentialsNotSource pins the denylist's shape: it
// must catch credential material wherever it sits, and must NOT swallow
// ordinary source (over-blocking silently un-protects real work).
func TestIsSecretPathMatchesCredentialsNotSource(t *testing.T) {
	secret := []string{
		".env", ".env.local", ".env.production",
		"config/.env", "server.pem", "tls/server.key", "certs/client.p12",
		".ssh/id_rsa", "home/.ssh/known_hosts", ".aws/credentials",
		".gnupg/secring.gpg", "id_ed25519", ".netrc", ".npmrc",
	}
	for _, p := range secret {
		if !IsSecretPath(p) {
			t.Errorf("%q is credential material and must never be captured", p)
		}
	}
	// Parent-qualified credential directories (~/.config/gh, browser profiles)
	// are easy to miss, and matter because `--protect ~/.config` is a supported
	// flag: a user protecting their config directory would otherwise have their
	// GitHub token copied into the store.
	parentQualified := []string{
		"/home/u/.config/gh/hosts.yml", // matched via absolute path
		"gh/hosts.yml",                 // and when ~/.config IS the protected root
		".config/gh/hosts.yml",
		"/home/u/.mozilla/firefox/p.default/key4.db",
		"/home/u/.config/google-chrome/Default/Login Data",
		"/home/u/.config/gcloud/credentials.db",
	}
	for _, p := range parentQualified {
		if !IsSecretPath(p) {
			t.Errorf("%q is credential material and must never be captured", p)
		}
	}
	// ...but a project directory that merely happens to be called "gh" is
	// ordinary source and must stay protected.
	for _, p := range []string{"src/gh/client.go", "gh/README.md"} {
		if IsSecretPath(p) && !strings.HasPrefix(p, "gh/") {
			t.Errorf("%q is ordinary source under a differently-parented dir", p)
		}
	}

	notSecret := []string{
		"main.go", "README.md", ".envrc", "environment.ts", "keyboard.go",
		"src/keys.go", "docs/pemberton.md", "env/config.yaml", "public.crt",
	}
	for _, p := range notSecret {
		if IsSecretPath(p) {
			t.Errorf("%q is ordinary project content and must stay protected", p)
		}
	}
}

// TestSnapshotNeverCapturesSecrets is the guarantee that matters: a .env or a
// private key in a protected project is NOT copied into the object store, and
// the user is told so by name rather than left to assume it was protected.
func TestSnapshotNeverCapturesSecrets(t *testing.T) {
	root := t.TempDir()
	storeDir := t.TempDir()
	const secretContent = "AWS_SECRET_ACCESS_KEY=hunter2\n"
	writeFile(t, filepath.Join(root, ".env"), secretContent, 0o600)
	writeFile(t, filepath.Join(root, "certs/server.key"), "-----BEGIN PRIVATE KEY-----\n", 0o600)
	mkdir(t, filepath.Join(root, ".ssh"), 0o700)
	writeFile(t, filepath.Join(root, ".ssh/id_rsa"), "-----BEGIN OPENSSH PRIVATE KEY-----\n", 0o600)
	writeFile(t, filepath.Join(root, "main.go"), "package main\n", 0o644)

	oc := mustObj(t, storeDir)
	m, err := Snapshot(root, oc, nil, 0, 1, DURABLE, 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{".env", "certs/server.key", ".ssh/id_rsa", ".ssh"} {
		if _, ok := m.Entries[rel]; ok {
			t.Fatalf("%s is credential material and must not be a manifest entry", rel)
		}
	}
	if _, ok := m.Entries["main.go"]; !ok {
		t.Fatal("ordinary source must still be captured")
	}
	// Named, not silent: the user can see what is unprotected.
	named := map[string]bool{}
	for _, ex := range m.Exceptions {
		named[ex.Path] = true
		if ex.Path == ".env" && !strings.Contains(ex.Reason, "credential") {
			t.Fatalf(".env exception must say why: %q", ex.Reason)
		}
	}
	for _, rel := range []string{".env", "certs/server.key", ".ssh"} {
		if !named[rel] {
			t.Fatalf("%s was skipped but never reported; silent non-protection is the failure mode we forbid", rel)
		}
	}
	// The bytes must not be anywhere in the object store.
	refs, err := oc.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		b, err := oc.Get(ref)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "PRIVATE KEY") {
			t.Fatalf("credential content reached the object store as %s", ref)
		}
	}
	// And an exact restore must not delete the secret it refused to protect.
	kept, _, err := func() ([]string, []string, error) {
		rm, kept, err := RestoreExact(m, oc, root)
		return kept, rm, err
	}()
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, ".env")); err != nil || string(b) != secretContent {
		t.Fatalf(".env must survive an exact restore untouched: %q err=%v (kept=%v)", b, err, kept)
	}
}
