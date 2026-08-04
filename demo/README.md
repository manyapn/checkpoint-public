# The checkpoint demo

`demo/run_demo.sh` is the 8-step story from the main README, executed against
the real binary and **asserted at every step**. It is meant to be run by *you*,
on *your* machine, so that the claims stop being marketing and start being
observations about your kernel and your filesystem.

```sh
sudo ./demo/run_demo.sh
```

`make demo` from the repo root does the same thing, building the binary first.
Either way it takes about 4 seconds and prints `STEP n OK` lines with timings,
then a summary of what it proved.

## What it does to your machine

Nothing that survives it:

- everything happens inside one `mktemp -d` sandbox with its own project, its
  own store and its own daemon on its own socket;
- it never reads or writes your real store under `~/.local/share/checkpoint`,
  and never touches a project of yours;
- it stops the daemon and deletes the sandbox on exit, including when it fails;
- the only thing it may write outside the sandbox is `bin/checkpoint`, if that
  binary is missing and it has to build it.

## Requirements

| Need | Why | If missing |
|---|---|---|
| Linux | the product is fanotify-based | it cannot run |
| root / `CAP_SYS_ADMIN` | `fanotify_init` requires it | step 1 fails at `doctor`, with the remedy printed |
| `bash`, `coreutils`, `diff` | the assertions | nothing to substitute |
| Go 1.24+ | only to build `bin/checkpoint` if it is missing | pass `--bin /path/to/checkpoint` instead |

## Options

```
--base DIR   put the sandbox under DIR (default: $TMPDIR)
--ext4       provision a loopback ext4 image and run inside it (needs root +
             mkfs.ext4 + a free loop device). Use this to see the change-feed
             behaviour when your working filesystem is overlayfs. It fails
             loudly rather than silently falling back.
--keep       leave the sandbox on disk and print its path
--bin PATH   use an already-installed checkpoint (also: CHECKPOINT_BIN=...)
```

## What each step proves

Each row is an assertion made against disk state, not a line of narration. If
the assertion does not hold, the script prints `STEP n FAILED` and exits
nonzero. It cannot print a success it did not observe.

| Step | Claim | How it is checked |
|---|---|---|
| 1 | protection starts, with a complete baseline | `doctor` exits 0; `status` reads `Protected` **and** `Complete baseline: yes`; the change-feed mode is recorded for later steps |
| 2 | a wrapped agent turn is captured and ends in exactly one durable checkpoint | the refactor, the delete and the outside write are all confirmed on disk; the run output contains one `checkpoint N DURABLE` |
| 3 | a human edit landed *during* the agent's turn | the agent prints its own start/end timestamps; the human writer records its own; the human's is asserted to fall strictly between them. The human process is a child of the demo, never of `checkpoint run`, which is exactly how provenance tells them apart |
| 4 | the state report is honest about what it did **not** cover | the agent wrote outside every protected folder, so `status` must read `Limited protection` and must name that exact path; the newest `history` row must carry a recovery badge |
| 5 | undo reverts the agent and nothing else | both refactored files are back to their pre-turn content; `NOTES.md` is byte-identical to before the undo **and** still differs from its pre-turn hash, so the human's line survived; a pre-undo checkpoint id was printed |
| 5 | an agent-deleted file comes back | with the change feed: asserted restored **byte-exact** by `undo`. Without it: asserted **not** restored, asserted that `undo` said deletions are unattributable here, and then the exact `restore --only` command `undo` printed is executed and its result checked byte-exact |
| 5b | a file you both edited is refused, never merged | `undo` must exit **nonzero**, name the file, say `NOTHING was changed`, and leave every file untouched, including the ones it could legally have reverted. Then `undo --save-both` reverts the agent-only file, leaves the live conflicted file byte-identical, and writes the checkpoint version alongside as `NOTES.md.checkpoint-<id>` |
| 6 | `rm -rf` of the whole project is survivable | a fingerprint of every path (kind, permission bits, symlink target, SHA-256) is taken, the project is genuinely `rm -rf`'d, restored, and the fingerprints must `diff` clean, plus explicit exec-bit, symlink and empty-directory checks |
| 7 | a file created *and* deleted inside a turn is still recoverable | `recover` must list it (no checkpoint ever held it), `recover --to` must extract it, and the bytes must match what the agent wrote |
| 8 | it stops cleanly | `protect --stop`, then `status` must read `Not protected`; the store size is printed |

### Is the harness itself trustworthy?

It was checked against two lying binaries:

```sh
./demo/run_demo.sh --bin /bin/true     # STEP 1 FAILED: not Protected after protect
./demo/run_demo.sh --bin ./fake-undo   # STEP 5 FAILED: the agent's refactor was NOT reverted
```

where `fake-undo` is a wrapper that prints a plausible `undo of checkpoint 1:
reverted 9, removed 0, skipped 0 for review` and changes nothing. Both are
caught. A green run is not "the script printed OK".

---

## What this demo does **not** prove

This is the part worth reading twice.

1. **It does not prove anything about a machine other than the one it ran on.**
   fanotify behaviour differs by kernel, filesystem, container runtime and
   security policy. A green run here says: *on this kernel, on this filesystem,
   today*. That is the entire reason it is shipped as a script you run rather
   than as a recording you watch.
2. **The "agent" is a bash script, not a real LLM agent.** checkpoint attributes
   by process lineage, so a wrapped `claude`, `codex` or `aider` is captured
   identically, but this demo does not exercise any agent's turn-end hook and so
   does not prove those integrations work.
3. **It does not measure overhead or scale.** The project is 5 files. Nothing
   here says what a 50k-file monorepo, a `node_modules` install, or a long build
   costs. That is what `make bench` is for, and the numbers it produces carry
   their own honesty note in [`bench/README.md`](../bench/README.md).
4. **It does not test power loss.** checkpoint is consistent against `kill -9`
   and daemon crashes, which the test suite covers. It is not consistent against
   the host losing power mid-write, and this demo pulls neither plug.
5. **It does not prove the guarantee holds for writes it never made.** No mmap
   path, no 20k-file write storm (queue overflow), no unreadable files, no
   fifos, no credential-file skipping. Those are covered by `go test ./...`, not
   by this script.
6. **It never claims the change feed works when it does not.** On overlayfs,
   which is what a default Docker container gives you, step 5 asserts the
   *degraded* behaviour instead: undo must refuse to guess who deleted the file,
   must say so, and the manual remedy it prints must work. The closing summary
   states which of the two paths actually ran.

### Known gaps this demo deliberately steps around

Both were re-verified while writing this demo, on loopback ext4, with the
current binary. They are real, and the demo does not pretend they are fixed:

- **Edits made by write-temp-then-rename are not reverted by `undo`.** That
  includes `sed -i` and the atomic saves many editors and formatters do. The
  captured version is recorded under the *temporary* path, so `undo` sees an
  unknown new file and never connects it to the destination. The agent in this
  demo therefore edits files **in place**, which is what agent edit tools do,
  rather than via `sed -i`. Repro:

  ```sh
  printf 'B-original\n' > b.txt
  checkpoint protect
  checkpoint run -- bash -c 'sed -i s/B-original/B-agent/ b.txt'
  checkpoint undo          # says "reverted 0, removed 1"; b.txt is still B-agent
  ```

  The content is **not lost**: `checkpoint restore <pre-turn-id> .` still brings
  the old file back. It is `undo`'s provenance link that is missing.
- **A symlink the agent repoints is not restored.** Creating a symlink is not a
  close-write, so there is no captured version. On ext4 the observed result of
  `undo` is that the agent's new symlink is *removed* rather than the old target
  restored. That is recoverable from the pre-undo checkpoint, but it is not what
  you would expect. The demo's symlink is only exercised through
  restore-byte-exactness (step 6), which does work.

---

## If it fails on your machine

That is a useful result, and the point of shipping the script. To make the
report actionable:

```sh
./demo/run_demo.sh --keep 2>&1 | tee /tmp/checkpoint-demo.log
```

and send back:

1. the whole log (it contains the failing `STEP n FAILED` line and every
   command that ran, verbatim);
2. `checkpoint doctor` output, and `uname -a`;
3. `findmnt -no FSTYPE,SOURCE -T <the sandbox path>`, because filesystem type is
   the single most common cause of a behaviour difference;
4. the sandbox `--keep` left behind: its `store/daemon.log` is the daemon's own
   account of what it saw.

Anything the demo asserts is a promise the product makes in the
[main README](../README.md). A failure is a broken promise, not a misconfigured
demo, so report it as one.
