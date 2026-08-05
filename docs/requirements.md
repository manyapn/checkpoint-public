# Requirements and filesystem support

What checkpoint needs from a machine, and exactly what it loses when the machine cannot provide it.

[Back to the README](../README.md)

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
