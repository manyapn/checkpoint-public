# Guarantees, and what is deliberately not guaranteed

Both halves, stated at the same length on purpose. The second list is the one worth reading.

[Back to the README](../README.md)

---

## What it guarantees

Each of these is exercised by `checkpoint selftest` on your own machine (see below), not just asserted here.

- **Every completed write under a protected root is captured**, including files created and deleted before any checkpoint existed.
- **A destroyed project restores byte-exact**: content, permission bits, exec bits, symlink targets, and empty directories.
- **`undo` reverts agent-attributed changes whole-file and leaves concurrent human edits byte-identical.** Attribution is by process lineage, so a human edit made *during* the agent's turn is preserved.
- **A file you both changed is refused, never merged.** `undo` exits nonzero and mutates nothing; `--save-both` reverts the rest and writes the checkpoint version alongside as `<file>.checkpoint-<id>`.
- **Every destructive operation is itself undoable.** `undo` and `restore` cut a pre-operation checkpoint before touching anything.
- **Credential material is never captured** (see below), and each skip is recorded as a *named exception* on the checkpoint rather than silently dropped.
- **Coverage is graded honestly.** A write outside every protected root downgrades `status` to `Limited protection` and names the path. If the kernel's event queue overflows under a write storm, the loss is detected and the checkpoint is graded `PARTIAL` rather than claimed complete.
- **Consistency against process death.** The store survives `SIGKILL` of the daemon or of a writer; a torn record is never presented as a valid checkpoint.

## What it does not guarantee

Stated at the same length, because this is the part that decides whether you can
rely on it.

- **No power-loss guarantee.** The design target is process-crash consistency, not host power loss. There is no `fsync` on every append. If the machine loses power mid-write, the last records are out of contract.
- **External side effects are never reversed.** Network requests, deploys, database migrations, emails. `checkpoint run` prints this on every invocation because it is the limitation most likely to hurt you.
- **`mmap` without a flush or close is out of contract.** A shared mapping thatis mutated and never `msync`'d, `munmap`'d or closed before the process dies is not captured, because there is no close-write for the kernel to report. `mmap` followed by a normal close *is* captured.
- **Deletions need the change feed.** On overlayfs, `undo` will not undo an agent's delete; it refuses to guess and prints the `restore --only` command instead. `restore` still brings deleted files back on every filesystem.
- **Edits made by write-temp-then-rename are not reverted by `undo`.** That includes `sed -i` and the atomic saves some editors and formatters perform.  The content is captured under the *temporary* path, so `undo` sees an unrelated new file and never links it to the destination. The content is **not lost**: `checkpoint restore <pre-turn-id> .` brings the old file back. It is `undo`'s provenance link that is missing.
- **Symlinks an agent creates or repoints are not reverted by `undo`**, for the same reason: creating a symlink is not a close-write, so no version is captured. `restore` reproduces symlink targets exactly.
- **Secrets are not recoverable by checkpoint.** That is the deliberate cost of never capturing them.
- **Writes outside the protected roots are not captured.** They are named in `status` rather than silently ignored, but they are not recoverable.
- **Nothing is captured while the daemon is not running.** checkpoint protects from the moment `protect` succeeds, and stops the moment it is stopped.
- **This is not a backup tool.** The store is local to the machine. It survives`rm -rf` of the project; it does not survive losing the disk.

### Secrets are never captured

Credential-shaped paths are skipped by default and recorded as named exceptions on the checkpoint, so you can see exactly what is not protected and why. The match is on names, not content, and is deliberately over-inclusive: skipping a harmless `server.key` costs a visible exception, while capturing a real key copies a credential into storage you did not choose.

| Class | Matched |
| --- | --- |
| Credential directories (whole subtree) | `.ssh`, `.aws`, `.gnupg`, `.docker` |
| Browser profiles (saved passwords, cookies, session tokens) | `.mozilla`, `.thunderbird`, `google-chrome`, `chromium`, `BraveSoftware`, `Microsoft Edge`, `vivaldi` |
| CLI token stores, only under `.config` | `gh`, `gcloud`, `op`, `doctl` |
| File names, anywhere | `.netrc`, `.pgpass`, `.npmrc`, `.pypirc`, `id_rsa`, `id_dsa`, `id_ecdsa`, `id_ed25519`, `credentials` |
| Extensions | `.pem`, `.key`, `.p12`, `.pfx`, `.jks`, `.keystore` |
| Environment files | `.env` and `.env.*`, but **not** `.envrc`, which is direnv config and ordinary project source |

The `secrets-never-captured` selftest writes a unique marker into a `.env`, then searches every file in the store (decompressing objects as it goes) to prove the marker is absent while a control marker from `README.md` is present.

---
