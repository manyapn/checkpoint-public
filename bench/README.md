# The checkpoint benchmark

`bench/` answers three questions with numbers instead of adjectives:

1. **Does recovery actually work, repeatedly?** Four destructive scenarios are
   driven through the real binary and scored from real disk state, five rounds
   each by default, so a default run is 4 scenarios x 5 rounds = 20 scored
   rounds (plus 3 x 5 git-shadow rounds).
2. **What does wrapping a command cost?** Wrapped versus unwrapped, on a
   write-churn workload and again on a realistic compile workload.
3. **How long does rolling back take?** `checkpoint undo` timed end to end
   against a stated workload, reported with its sample count and spread.

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

## Rollback latency

`undo_ms` is the median wall time of `checkpoint undo`, from fork/exec of the
binary until it exits. Nothing is subtracted: process start, store open, plan
build, the pre-undo checkpoint it cuts and every file written back are all
inside the number. A sample counts only if undo exited 0 *and* every file the
turn changed was verified back to its seeded bytes, and the report carries the
achieved sample count rather than the intended one.

The workload travels with the number, because rollback cost scales with the
size of the turn being reverted: the default is 20 files reverted inside a
500-file tree, median of 9 samples, with `min_ms` and `max_ms` for the spread.
`startup_floor_ms` is the same binary run as `checkpoint version`, which touches
no store at all; it is the exec cost every undo also pays. A quoted rollback
median close to that floor is a measurement of process launch, not of rollback.

Quote `undo_ms` only together with the workload, the sample count and the
filesystem. On its own it is not a result.

## Overhead

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
- `overhead_pct`, `boundary_ms`, `routine_cut_ms` and `undo_ms` are reported,
  not gated

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
| `overhead.go` | churn overhead, boundary latency, routine checkpoint cost, realistic compile overhead, rollback latency |
| `accept.sh` | scores a report against the thresholds above |
