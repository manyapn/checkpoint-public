# checkpoint

**Version control built for long-running agent sessions.**

Git assumes a person decides when a unit of work is finished, and writes it down. That holds for humans. It breaks the moment an agent works on its own for hours.

- a usable timeline would mean committing CONSTANTLY, and the history becomes noise you have to clean up before anyone can read it
- git records an author per commit, not per change, so inside one working tree it cannot tell the agent's edits from the ones you made alongside them
- a file the agent created and deleted between two commits never existed, as far as git is concerned
- `git checkout .` to undo the agent throws away what you wrote in the same window

checkpoint keeps a second history underneath the one you publish. 

It commits itself, it knows who wrote each file, and it never touches your git history. 

| Coming from git | In checkpoint |
| --- | --- |
| `git commit`, if you remembered to | happens on its own, at every agent turn |
| `git log` | `checkpoint history` |
| `git checkout <sha> -- .` | `checkpoint restore <id>` |
| `git revert` | `checkpoint undo`, which reverts the agent and keeps you |
| `git stash`, decided in advance | nothing to decide: it was already recorded |
| `git gc` | `checkpoint prune` |
| *no equivalent* | `checkpoint recover`, for files that never lived to a commit |

```console
$ checkpoint history
#4   2026-08-04 04:10:16  Fully recoverable             run: ./agent_turn.sh
#3   2026-08-04 04:08:00  Recoverable with exceptions   run: ./agent_turn.sh
      ! .env (not captured: looks like credential material (security default))
#2   2026-08-04 04:07:45  Fully recoverable             manual
#1   2026-08-04 04:07:40  Fully recoverable             setup

$ checkpoint undo
undo of checkpoint 4: reverted 2, removed 0, skipped 0 for review
pre-undo checkpoint 5 saved (restore it to undo this undo)
```

**checkpoint is NOT a replacement for git.** No branches, no merges, no remotes, nothing here is meant to be published or reviewed. Git holds the history of your project. checkpoint holds the history of the session, which git was never built to hold. 

Every completed write under a protected directory is captured to a store that lives outside your project, so any checkpoint restores the tree byte-exact: 
- after a bad refactor
- after `rm -rf`
- ...and even for a file the agent created and deleted
inside a single turn, which was never in git at all

The harder half is coverage. **checkpoint never claims protection it does not have.** Every checkpoint carries a badge (Fully recoverable, Recoverable with exceptions, or Incomplete), every gap names the exact paths it could not cover, and a filesystem that cannot support the full guarantee tells you so when you run `checkpoint doctor`, not when you try to restore. 

Capture records the lineage of the process that made each write, so the revert can be scoped by AUTHOR rather than by whole checkpoint. `undo` reverts what the agent wrote and leaves the paragraph you typed alongside it untouched.

---

## Will this work on your machine?

| | |
| --- | --- |
| **OS** | Linux only |
| **Privilege** | `CAP_SYS_ADMIN` for the capture daemon (as `fanotify_init` requires it). In practice: run `protect`, `daemon`, `run` and `capture` under `sudo`. |
| **Kernel** | 5.1 or newer. The dirent change feed additionally needs 5.17 or newer. |
| **Filesystem** | **ext4 / xfs / btrfs**: full fidelity. **overlayfs**, which is what a default Docker container gives you, is **degraded**; see below. |
| **Store** | Lives outside the project (default `$XDG_DATA_HOME/checkpoint/<key>`), on any filesystem, so `rm -rf` of the project cannot destroy its history. |

**overlayfs note** 
The change feed is a filesystem-wide `fanotify` mark that reports directory-entry events. It is how checkpoint knows a file was *deleted* and by whom. overlayfs refuses that mark. On overlayfs, capture and restore still work, but:
- **deletions carry no provenance**, so `undo` will not undo an agent's delete.
  It says so and prints the exact `restore --only` command that brings the file
  back, rather than guessing who deleted it.
- **every checkpoint does a full tree scan** instead of scaling with the number
  of changes, which is slower on large trees.

If you work in a container, bind-mount the project from the host instead of working on the container's overlay layer.

`checkpoint doctor` answers this question for you:

```console
$ checkpoint doctor
ok    kernel                Linux 6.12.76-linuxkit is supported (needs 5.1+; change feed needs 5.17+)
ok    fanotify capability   fanotify armed and released successfully (a real group was created and marked)
ok    workspace             /srv/acme-api: 4 files, 209 bytes to protect
ok    workspace filesystem  ext2/ext3/ext4: change feed available (deletions are attributed; checkpoints scale with changes, not tree size)
ok    store location        /root/.local/share/checkpoint/acme-api-e2c2f8005ccf: will be created (/root/.local/share exists and is writable), out of the workspace tree
ok    store free space      446.5 GB free of 485.5 GB on the store filesystem (/root/.local/share)

checkpoint should work on this machine.
```

and on overlayfs, the same command:

```console
WARN  workspace filesystem  overlayfs: no dirent change feed here, because the kernel refuses a filesystem-wide mark. Capture and restore still work; what is lost is that deletions are not attributed (an agent-deleted file is not undone) and every checkpoint does a full tree scan (slower on large trees)
                            remedy: nothing to fix: this is a property of overlayfs. For delete attribution, keep the project on ext4/xfs/btrfs (in containers, bind-mount the project from the host instead of working on the container's overlay layer)

checkpoint will work here, with 1 limitation above (WARN).
```

---

## Quickstart

```sh
git clone https://github.com/manyapn/checkpoint-public
cd checkpoint-public
make build && sudo make install     # installs bin/checkpoint to /usr/local/bin
```

Everything below is a real transcript, on ext4, of the binary this repo builds.

**1. Check the machine, then start protection.** `protect` starts a detached daemon for the project and cuts a complete baseline checkpoint immediately.

```console
$ cd /srv/acme-api
$ sudo checkpoint doctor
...
checkpoint should work on this machine.

$ sudo checkpoint protect
protection started for /srv/acme-api: protected [log: /root/.local/share/checkpoint/acme-api-e2c2f8005ccf/daemon.log]

$ sudo checkpoint status
Protection: Protected
Root: /srv/acme-api
Store: /root/.local/share/checkpoint/acme-api-e2c2f8005ccf
Storage: 1.2 kB across 4 objects and 1 checkpoint (oldest just now)
Checkpoints: 1
Last complete checkpoint: 0s ago
Complete baseline: yes
Change feed: active (delete attribution + change-scaled checkpoints)
Active agent sessions: 0
Protecting since: 2026-08-04 03:26:49
```

**2. Wrap the agent.** `run` executes any command and asks the daemon to cut one checkpoint when it exits. checkpoint does not know or care what the process is: `claude`, `codex`, `aider` and a bash script are all wrapped identically. Here the "agent" refactors `server.py` and `routes.py`, deletes `scratch.md`, and **meanwhile a human appends a line to `NOTES.md` from a separate shell**.

```console
$ sudo checkpoint run -- ./agent_turn.sh
checkpoint: local file changes are recoverable; network requests, remote databases, deploys, emails, and other outside effects are NOT.
checkpoint 1 DURABLE (3 entries)

$ sudo checkpoint history
#1   2026-08-04 03:26:51  Fully recoverable             run: /srv/agent_turn.sh
#0   2026-08-04 03:26:49  Fully recoverable             setup
```

**3. Undo the agent.**

```console
$ sudo checkpoint undo
undo of checkpoint 1: reverted 3, removed 0, skipped 0 for review
pre-undo checkpoint 2 saved (restore it to undo this undo)
```

Both refactors are reverted, `scratch.md` is back byte-exact, and the line the human typed during the turn is still there:

```console
$ tail -1 NOTES.md
a line I typed myself while the agent was working
```

The undo itself became checkpoint 2, so it is undoable too.

**If you both edited the same file**, `undo` refuses and changes *nothing at all*, not even the files it could legally have reverted:

```console
$ sudo checkpoint undo
  needs review (you also changed it): NOTES.md
checkpoint: 1 file(s) need review; NOTHING was changed. Rerun with --save-both to revert the rest and keep each conflict's checkpoint version alongside

$ sudo checkpoint undo --save-both
undo of checkpoint 1: reverted 0, removed 1, skipped 1 for review
  needs review (you also changed it, left untouched): NOTES.md
  saved checkpoint version alongside: /srv/acme-api/NOTES.md.checkpoint-0
pre-undo checkpoint 2 saved (restore it to undo this undo)
```

Nothing is ever line-merged and nothing of yours is ever overwritten.

**4. If something destroys the project**, the store is out of tree, so it survives:

```sh
sudo rm -rf /srv/acme-api
sudo checkpoint restore --store /root/.local/share/checkpoint/acme-api-e2c2f8005ccf 0 /srv/acme-api
```

Restore reproduces content, permission bits, exec bits, symlink targets and empty directories.

**5. Files that existed only between checkpoints** are still recoverable, because capture is continuous and checkpoints are only labels over it:

```console
$ sudo checkpoint recover .
/srv/acme-api/.agent-tmp.json

$ sudo checkpoint recover --to /tmp/salvage .
recovered /srv/acme-api/.agent-tmp.json -> /tmp/salvage/.agent-tmp.json
```

---

## How it works

**Capture: `fanotify` close-write, with the file descriptor retained.** 
- The daemon holds a `fanotify` group marked on every protected root
- The moment a writable handle is *closed*, the kernel hands over a file descriptor for the file and checkpoint reads the content through *that* descriptor
- Reading through the retained fd rather than reopening the path is what makes a file created and deleted microseconds later still recoverable: the path is gone, the inode is not
- Only completed writes are captured; an open, half-written file is never recorded as "recoverable".

**Attribution: process lineage** 
- Each captured write records the writing pid; a birth-parent tracker resolves that pid's ancestry and a write is the agent's only if its lineage terminates at a registered agent root (what `checkpoint run` registers, or what an agent's turn-end hook registers)
- A human editing from a separate shell is never a descendant of the wrapped process, so their write is not the agent's *even if it lands in the middle of the agent's turn*
- This is the whole reason a mid-turn human edit survives `undo`, andnothing here depends on timestamps.

**Storage: content-addressed, out of tree.** 
- Contents are stored as git-compatible loose objects: `blob <len>\0<content>`, sha1-keyed, zlib-compressed, deduplicated, at `objects/<xx>/<rest>`.
- Identical content is stored once no matter how many checkpoints reference it, and the objects are readable by a real `git cat-file`
- The store lives outside the project by default, which is why deleting the project does not delete its history.

A checkpoint is a manifest over that content: a labelled point in a continuous stream, not a separate copy operation.

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

## Storage and pruning

Content is deduplicated by hash, so a checkpoint costs the bytes that actually changed. The whole quickstart above (a baseline, two agent turns, an undo and a transient file) cost about 5 kB:

```console
$ sudo checkpoint status
Storage: 5.2 kB across 8 objects and 4 checkpoints (oldest just now)
```

`prune` deletes unnamed checkpoints older than `--keep-days` (default 7), then reclaims every object no remaining checkpoint references:

```console
$ sudo checkpoint protect --stop
protection stopped for /srv/acme-api

$ sudo checkpoint prune --keep-days 7 --dry-run
nothing to prune
```

Three rules make this safe: **named checkpoints never expire** (`checkpoint save --name before-migration`), **the latest complete baseline always survives** because it is what `undo` and incremental checkpoints are computed against, and prune requires the daemon stopped so it never races capture. `--dry-run` reports exactly what a real pass would remove.

---

## Verify the claims on your own machine

Nothing above is worth believing on a page. `selftest` builds real scenarios against a real daemon in a throwaway directory and reports a verdict per guarantee, including refusing to claim a guarantee it could not test here. On ext4 all seven pass:

```console
$ sudo checkpoint selftest --work "$PWD"
...
PASS  protection-starts              `protect` confirmed standing protection and the daemon cut setup checkpoint #0 ...
PASS  write-captured-and-restorable  a 30-byte write was captured into checkpoint #1 and restored byte-exact after the file was deleted (notes/keep.md)
PASS  rm-rf-disaster-restore         `rm -rf` of the whole workspace, then `restore 0`, reproduced all 10 entries byte-exact (content, permission bits, exec bits, symlink targets and empty directories)
PASS  transient-salvage              tmp/transient.txt was created, closed and deleted without ever appearing in a checkpoint; `recover --to` returned its 46 bytes byte-exact
PASS  agent-undo-preserves-human     during one wrapped agent turn the agent edited agent.txt and a separate human process concurrently edited human.txt; `undo` reverted agent.txt to its baseline (checkpoint 3) and left human.txt untouched
PASS  agent-delete-undone            a wrapped agent deleted doomed.txt; `undo` recognised the deletion as the agent's (change feed active) and restored the file byte-exact from checkpoint 6
PASS  secrets-never-captured         a .env holding a unique marker was written before and during protection; the marker appears nowhere under .../stores/primary (27 store files searched, compressed objects decompressed and searched too), while the control marker from README.md was found in 1 ...

VERDICT: every guarantee held on this machine.
```

That last check also proves the search itself works. A control marker from this README is found in the store, so "not found" means absent rather than unsearched.

Pass `--work "$PWD"` so it tests the filesystem your projects actually live on rather than `/tmp`. On overlayfs the same run reports 6 passed and one SKIP, with the reason spelled out. That is a property of the environment, not a failure:

```console
SKIP  agent-delete-undone            not tested: the dirent change feed is unavailable on this workspace's filesystem (overlayfs), and delete attribution is feed-scoped ...
```

`selftest --json` emits the machine-readable report, including a full environment block (kernel, distro, arch, filesystem of both workspace and store, whether the feed is active, and the commit the binary was built from). Attach it to any bug report.

`make demo` runs a longer end-to-end story that asserts every step against real disk state; see [`demo/README.md`](demo/README.md). `make bench` scores recovery and overhead against a git-shadow baseline; see [`bench/README.md`](bench/README.md).

---

## Commands

| | |
| --- | --- |
| `doctor` | Will this work on THIS machine? Run it first. |
| `selftest` | Prove the guarantees hold here. |
| `protect` / `protect --stop` | Start or stop standing protection for a project. |
| `run -- <cmd>` | Run a command, then cut one checkpoint at its exit. |
| `save [--name L]` | Cut a checkpoint now. Named checkpoints survive pruning. |
| `undo [--only …] [--save-both]` | Revert the latest checkpoint's agent-only changes. |
| `status` / `history` | What is protected, what was covered, what was not. |
| `restore <id> <dir>` | Rebuild a project from a checkpoint. |
| `recover [--to DIR]` | List or extract files no checkpoint ever held. |
| `prune [--keep-days N]` | Expire old checkpoints and reclaim storage. |
| `ui` | Terminal UI over the same data. |
| `version` | Which commit this binary was built from. |

All of them take `--root` and `--store`; both default sensibly from the current directory. `checkpoint --help` has the full surface.

**Turn boundaries.** `save` is the source-agnostic boundary: an agent's turn-end hook (for example Claude Code's `Stop` hook) and a manual `checkpoint save` enter through exactly the same door, so wiring up a new agent is one hook that shells out to `checkpoint save`.

---

## Building from source

```sh
make build        # -> bin/checkpoint
make test         # full suite (fanotify tests need root)
make vet
sudo make install # PREFIX=/usr/local by default
make demo         # the self-asserting end-to-end story (needs root)
make bench        # recovery and overhead vs a git shadow (needs root)
```

Go 1.24 or newer. The only runtime dependency is a Linux kernel.

## AI-assisted development

This code was written with AI assistance. This is what I did in the development process to ensure my build was still correct + accurate (a thorough verification system):

- Model knowledge about `fanotify` edge cases is stale or wrong, and the kernel is the only authority. Every claim about watcher semantics had to be backed by a runnable program that exits 0 on a real kernel before it could be written down as fact.
- A gate script could not be edited to make it pass. If a gate looked wrong, the rule is to stop and argue for changing it. 
- Tests are written first, then the code. The tests look at the code as a black box. The gates grep for specific test functions, so a passing run with no tests is a failure.
- All errors were each  reproduced first, then fixed. Each keeps its test as a permanent guard in the spirit of test-driven development.
- Results were scored from the disk-state, as an agent reporting success is not evidence. See the demo for details on this (it asserts against real files, the benchmark fingerprints the tree, and `selftest` re-runs the guarantees on your machine)
- Findings were checked by independent agents prompted to refute them, and the product was tested black-box against the built binary by an agent with no knowledge of the implementation.

## License

MIT. See [LICENSE](LICENSE).
