# Command reference

Every command, its flags, and what it does.

[Back to the README](../README.md)

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
