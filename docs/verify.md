# Verifying the claims on your own machine

None of the claims in this project ask to be taken on trust. This is how you check them yourself, in about a minute.

[Back to the README](../README.md)

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
