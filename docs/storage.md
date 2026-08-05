# Storage and pruning

Where the session history lives, what it costs, and how it is bounded.

[Back to the README](../README.md)

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
