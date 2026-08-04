# The checkpoint benchmark

`bench/` answers two questions with numbers instead of adjectives:

1. **Does recovery actually work, repeatedly?** Four destructive scenarios are
   driven through the real binary and scored from real disk state, five rounds
   each by default.
2. **What does wrapping a command cost?** Wrapped versus unwrapped, on a
   write-churn workload and again on a realistic compile workload.

Three of the four recovery scenarios also run against a **git-shadow baseline**,
the strongest simple git strategy: a force-add commit at every turn boundary.
Recovery numbers mean little on their own, so the report carries the comparison
in the same file.

```sh
make bench     # builds bin/checkpoint and bin/bench, then runs as root
make accept    # scores results/bench.json against the thresholds
```

`make bench` needs root, for the same reason the daemon does: it starts real
daemons, and `fanotify_init` requires `CAP_SYS_ADMIN`.

## Knobs

| Variable | Default | Meaning |
|---|---|---|
| `BENCH_BASE` | a temp dir | where sandboxes are created. Point it at ext4, xfs or btrfs. |
| `BENCH_ROUNDS` | 5 | rounds per scenario |

`BENCH_BASE` matters. Delete provenance comes from the dirent change feed, which
overlayfs (what a default Docker container gives you) does not provide. There,
every `agent_delete_undo` round records `change feed unavailable on this
filesystem` in its `detail` and scores zero, so `make accept` fails. That is the
environment rather than a regression. Read `feed_active` in the report before
reading anything into that column, and put `BENCH_BASE` on ext4, xfs or btrfs if
you want a score that means something.

```sh
make bench BENCH_BASE=/mnt/ext4 BENCH_ROUNDS=10
```

The harness can also be run directly:

```sh
go build -o bin/bench ./bench
sudo ./bin/bench --bin bin/checkpoint --base /mnt/ext4 --rounds 5 --out results/bench.json
```

## The scenarios

| Scenario | What it does | Scored on |
|---|---|---|
| `rm_rf_restore` | an agent turn writes, then the whole workspace is deleted | the restored tree must fingerprint identically to the pre-delete tree, including symlinks and directories |
| `transient_salvage` | a file is created and deleted between checkpoints, so no checkpoint ever holds it | `recover --to` must return its exact bytes |
| `human_preservation` | the agent edits one file while the human edits another | `undo` must revert the agent's file and leave the human's byte-identical |
| `agent_delete_undo` | the agent deletes a pre-existing file | `undo` must bring it back byte-exact. Needs the change feed. |

The first three also run against the git shadow. Git restores a deleted tree
fine; it has no answer for a transient it never committed, and no provenance to
separate the agent's edit from yours, so those two columns come back at zero
with the reason in each row's `detail`.

## Why you can believe the numbers

Three rules, enforced in the code rather than promised here:

- **The expected post-state is computed, never authored.** Each scenario records
  the real pre-destruction bytes, either a fingerprint of the whole tree or the
  exact content it wrote, and compares against that. The harness cannot assert
  what it hopes to see.
- **A scenario scores `recovered: true` only after the destruction was observed
  on disk.** If a wrapper quietly swallows the workload, the scenario errors
  with `mutation not observed` rather than passing for free.
- **An errored round scores `recovered: false`, and the report is written
  either way.** A crashed round cannot vanish from the denominator.

For overhead, the timed script times *itself* into a nanosecond file rather than
being wall-clocked from outside. Capture is off the writer's path, so what a
working agent feels is the writer-visible cost; wall-clocking `run` on a burst
workload mostly measures the settle window instead. The trailing boundary cost
(drain, settle, cut) is reported separately as `boundary_ms`.

The realistic overhead measurement exists because the churn percentage has no
resolving power: at millisecond scale both sides are noise. It compiles a
generated C project instead, scaling the translation-unit count at runtime until
one unwrapped build takes at least 3 seconds, and records the achieved duration
next to the percentage so you can check that for yourself. Without `gcc` it is
skipped cleanly, leaving `overhead_realistic_pct` null and a note saying why.

## Thresholds

`bench/accept.sh` scores `results/bench.json`:

- the four recovery scenarios must be at 100%
- `us_per_write` must be 25 or less
- `overhead_pct`, `boundary_ms` and `routine_cut_ms` are reported, not gated

These are development thresholds for catching a regression between changes.
They are not a pre-registered benchmark, and the numbers they print are not a
citeable headline result.

## Layout

| File | Job |
|---|---|
| `main.go` | flags, the JSON report schema, the run loop, aggregation |
| `sandbox.go` | one throwaway workspace, store and daemon per round; the fingerprint comparison everything is scored with |
| `scenarios.go` | the four recovery scenarios |
| `gitshadow.go` | the git baseline, committed to an external git dir outside the workspace |
| `overhead.go` | churn overhead, boundary latency, routine checkpoint cost, realistic compile overhead |
| `accept.sh` | scores a report against the thresholds above |
