# The checkpoint benchmark

`bench/` answers three questions about the session history with numbers instead
of adjectives:

1. **Can the history give the state back, repeatedly?** Four scenarios take a
   state away and ask the history for it, driven through the real binary and
   scored from real disk state, five rounds each by default, so a default run is
   4 scenarios x 5 rounds = 20 scored rounds (plus 3 x 5 git-shadow rounds).
2. **What does it cost to record a session?** Wrapped versus unwrapped, on a
   write-churn workload and again on a realistic compile workload, plus the cost
   of the checkpoint cut at each turn boundary.
3. **How long does it take to go back?** `checkpoint undo` timed end to end
   against a stated workload, reported with its sample count and spread.

Three of the four scenarios also run against a **git-shadow baseline**: git
driven as hard as a simple strategy can drive it, with a force-add commit at
every turn boundary. It is there because the interesting question is not whether
a state can be recovered in the abstract, but which of the two histories still
holds it. The comparison ships in the same report rather than in a sentence
here.

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

`BENCH_BASE` matters. A delete reaches the history with an author only through
the dirent change feed, which overlayfs (what a default Docker container gives
you) does not provide. There, every `agent_delete_undo` round records `change
feed unavailable on this filesystem` in its `detail` and scores zero, so `make
accept` fails. That is the environment rather than a regression. Read
`feed_active` in the report before reading anything into that column, and put
`BENCH_BASE` on ext4, xfs or btrfs if you want a score that means something.

```sh
make bench BENCH_BASE=/mnt/ext4 BENCH_ROUNDS=10
```

The harness can also be run directly:

```sh
go build -o bin/bench ./bench
sudo ./bin/bench --bin bin/checkpoint --base /mnt/ext4 --rounds 5 --out results/bench.json
```

## The scenarios

Each one asks the history for a state that is no longer on disk.

| Scenario | What it does | Scored on |
|---|---|---|
| `rm_rf_restore` | an agent turn writes, then the whole workspace is deleted | the restored tree must fingerprint identically to the pre-delete tree, including symlinks and directories |
| `transient_salvage` | a file is created and deleted between checkpoints, so no checkpoint ever held it | `recover --to` must return its exact bytes |
| `human_preservation` | the agent edits one file while the human edits another | `undo` must revert the agent's file and leave the human's byte-identical |
| `agent_delete_undo` | the agent deletes a pre-existing file | `undo` must bring it back byte-exact. Needs the change feed. |

The first three also run against the git shadow, and the split is the point.
Git checks a deleted tree back out fine, which is what it is for. It has no
answer for a state it never committed, and no authorship below the commit to
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

## How long it takes to go back

`undo_ms` is the median wall time of `checkpoint undo`, from fork/exec of the
binary until it exits. Nothing is subtracted: process start, store open, plan
build, the checkpoint it cuts before reverting and every file written back are
all inside the number. A sample counts only if undo exited 0 *and* every file
the turn changed was verified back to its seeded bytes, and the report carries
the achieved sample count rather than the intended one.

The workload travels with the number, because going back costs in proportion to
the size of the turn being reverted: the default is 20 files reverted inside a
500-file tree, median of 9 samples, with `min_ms` and `max_ms` for the spread.
`startup_floor_ms` is the same binary run as `checkpoint version`, which touches
no store at all; it is the exec cost every undo also pays. A quoted median close
to that floor is a measurement of process launch, not of the revert.

Quote `undo_ms` only together with the workload, the sample count and the
filesystem. On its own it is not a result.

## What it costs to record a session

Recording is what you pay all the time, so it is measured where it is felt. The
timed script times *itself* into a nanosecond file rather than being wall-clocked
from outside, because capture is off the writer's path: what a working agent
feels is the writer-visible cost, and wall-clocking `run` on a burst workload
mostly measures the settle window instead. `us_per_write` is that cost per file
written. The cost of the commit at the end of a turn (drain, settle, cut) is
reported separately as `boundary_ms`, and `routine_cut_ms` is the cost of a cut
in the middle of a session.

The realistic overhead measurement exists because the churn percentage has no
resolving power: at millisecond scale both sides are noise. It compiles a
generated C project instead, scaling the translation-unit count at runtime until
one unwrapped build takes at least 3 seconds, and records the achieved duration
next to the percentage so you can check that for yourself. Without `gcc` it is
skipped cleanly, leaving `overhead_realistic_pct` null and a note saying why.

## Thresholds

`bench/accept.sh` scores `results/bench.json`:

- the four scenarios must be at 100%
- `us_per_write` must be 25 or less
- `overhead_pct`, `boundary_ms`, `routine_cut_ms` and `undo_ms` are reported,
  not gated

### The 25 us per write threshold is a tripwire, not a measurement

Say it plainly, because it is the easiest number in this repo to misread:

> **25 us per write is a regression gate. It is not a performance claim, it is
> not a measured result, and it must not be quoted as one.** It means "if this
> jumps, someone put work on the writer's path". A passing run says the writer
> path did not get worse. It does not say recording costs 25 us, or 5 us, or
> any other number.

The same goes for the 5% bar on `overhead_realistic_pct`, which `accept.sh`
prints as OK or EXCEEDED and does not gate at all.

There is measured evidence for why this matters, committed in
[`docs/reports/`](../docs/reports/). Five identical runs on one machine, one
commit, no code change between them, returned `us_per_write` of -12.74, -9.25,
-0.42, +0.30 and +87.22, and the gate passed four times and failed once. Three
of the five came back negative, which is not a speedup: the wrapped and
unwrapped sides of the churn workload differ by a few tens of milliseconds
spread over 2000 writes, so a millisecond of scheduler noise moves the result by
0.5 us per write. On that machine the honest statement about recording cost is
"below the noise floor of the measurement", not a figure.

A threshold a stationary system crosses one run in five is coarser than its own
measurement. That is fine for a tripwire and disqualifying for a headline.

If you want a number to quote, quote the recovery columns (they are counts of
rounds, not timings), or run the benchmark on hardware you control and publish
the spread the way `docs/reports/README.md` does.

These are development thresholds for catching a regression between changes.
They are not a pre-registered benchmark, and the numbers they print are not a
citeable headline result.

## Layout

| File | Job |
|---|---|
| `main.go` | flags, the JSON report schema, the run loop, aggregation |
| `sandbox.go` | one throwaway workspace, store and daemon per round; the fingerprint comparison everything is scored with |
| `scenarios.go` | the four scenarios |
| `gitshadow.go` | the git baseline, committed to an external git dir outside the workspace |
| `overhead.go` | recording cost, boundary latency, routine checkpoint cost, realistic compile overhead, and how long it takes to go back |
| `accept.sh` | scores a report against the thresholds above |
