# checkpoint

Version control for long-running AI agent sessions.

When an agent edits your project for hours, git cannot tell its edits from
yours inside one working tree, and a file the agent created and deleted
between commits never existed. checkpoint keeps a second history underneath
git: it records every file write as it happens, remembers who made it, and
cuts a checkpoint at the end of every agent turn.

- `undo` puts back only what the agent changed and leaves your edits alone.
  If you both changed a file it refuses and says so; it never merges.
- `restore` rebuilds the project byte-exact, modes and symlinks included,
  even after `rm -rf`. The history lives outside the project.
- `recover` returns files that were created and deleted inside one turn and
  never reached a checkpoint.

Linux only (it uses fanotify), needs root or `CAP_SYS_ADMIN`.

## Try it

```sh
git clone https://github.com/manyapn/checkpoint-public && cd checkpoint-public
make build && sudo make install
cd ~/my-project
sudo checkpoint doctor          # will this work on this machine and filesystem?
sudo checkpoint protect         # start recording; cuts a first checkpoint now
sudo checkpoint run -- claude   # or any agent; its turn becomes a checkpoint
checkpoint history
sudo checkpoint undo            # revert that turn's agent-only changes
sudo checkpoint protect --stop
```

`sudo checkpoint selftest --verbose` proves every guarantee on your own
machine by driving the real binary through seven scenarios and checking
bytes on disk:

```
STEP 1 protection-starts            PASS
STEP 2 write-captured-and-restorable PASS
STEP 3 rm-rf-disaster-restore       PASS
STEP 4 transient-salvage            PASS
STEP 5 agent-undo-preserves-human   PASS
STEP 6 agent-delete-undone          PASS
STEP 7 secrets-never-captured       PASS

VERDICT: every guarantee that could be tested held on this machine.
```

## Running it from a Mac

The product needs a Linux kernel, so the kernel-dependent tests run in a
privileged Docker container with a loopback ext4 filesystem. Docker Desktop
is the only setup.

```sh
make test          # everything that needs no kernel, on this machine, in seconds
make linux-test    # the whole suite in the container
make demo          # build in the container and run the selftest as a narrated story
```

`scripts/linux.sh <command>` runs any command the same way. One thing to
know: a folder shared from macOS into Docker Desktop accepts fanotify
watches and then never delivers an event. `doctor` detects this and fails;
the scripts put test workspaces on the ext4 mount instead.

## How it works

Read [docs/DESIGN.md](docs/DESIGN.md), one page with the diagram and the
five decisions worth defending. [docs/INTERVIEW.md](docs/INTERVIEW.md) is
the study guide: which file to read in which order and the questions you
will be asked.

```
cmd/checkpoint       the commands (thin)
cmd/checkpoint-ui    the terminal UI, a separate binary
internal/objects     file contents by hash, in git's loose-object format
internal/writelog    one line per write: path, content ref, author
internal/snapshot    whole-tree checkpoints: scan, fold, restore, prune, salvage
internal/lineage     agent | human | self | unknown, from the process tree
internal/watch       the two fanotify listeners
internal/daemon      records continuously, cuts checkpoints when asked
internal/undo        the author-scoped revert
internal/doctor      will it work here? probe, do not guess
internal/selftest    the seven scenarios, against the real binary
```

## Commands

| | |
| --- | --- |
| `doctor` | probe the kernel, fanotify, the filesystem and the store location |
| `protect [--stop] [DIR]` | start or stop standing protection |
| `run -- <cmd>` | run a command as the agent; one checkpoint when it exits |
| `save [--name L]` | cut a checkpoint now; named ones never expire |
| `undo [--only a,b] [--save-both]` | revert the latest turn's agent-only changes |
| `history [--json]`, `status [--json]` | what is recorded, what was missed |
| `restore [--only a,b] ID [DIR]` | rebuild from a checkpoint, in place by default |
| `recover [--to DIR]` | files no checkpoint holds |
| `prune [--keep-days N] [--yes]` | expire old checkpoints, reclaim space (daemon stopped) |
| `ui`, `selftest`, `version` | |

All commands take `--root DIR` (default: the current directory) and
`--store DIR` (default: `~/.local/share/checkpoint/<project>-<hash>`). A
store inside the project is refused.

## What is not guaranteed

- No power-loss durability: process-crash consistency only.
- `mmap` writes without a close are not captured.
- Edits done as write-temp-then-rename (`sed -i`, some editors) show up as a
  new file under the temp name; `restore` has the old content, `undo` cannot
  link the two.
- On filesystems without a change feed (overlayfs), deletions have no
  author, so `undo` lists them instead of reverting them.
- Credential-shaped files (`.env`, `*.pem`, `.ssh/`, ...) are never captured
  and are listed as exceptions on every checkpoint.
- One protected folder per store. Protect two folders with two daemons.
- It is not a backup; the store is on the same disk.

## History and AI assistance

This is a from-scratch rewrite of the earlier version in this repository's
history, with the same commands and guarantees in about a third of the code.
Two features were dropped on purpose: extra protected folders
(`--protect DIR,DIR`) and the interrupted-operation journal, since `undo` and
`restore` already cut a checkpoint of the present before touching anything.

The code was written with AI assistance. Nothing about kernel behavior was
taken from the model: every fanotify claim is backed by a test that runs on
a real kernel, and `selftest` judges by bytes on disk, not by what any
program reports about itself.

MIT license.
