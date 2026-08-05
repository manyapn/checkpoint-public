# Measured evidence

Everything in this directory came out of a real run on one machine on
2026-08-04. Nothing here is hand written, estimated, or copied from a previous
run. The machine, the filesystem and the binary commit are recorded in
[`env.txt`](env.txt) so the numbers can be argued with.

The one sentence version: **recovery is 100/100 rounds on ext4 and the recording
cost is smaller than this machine's measurement noise, so the honest statement
about overhead is "not measurable here", not a number.**

## The files

| File | What it is |
|---|---|
| [`env.txt`](env.txt) | the machine, toolchain, binary commit and both filesystems under test |
| [`selftest-ext4.txt`](selftest-ext4.txt) | `checkpoint selftest` on loopback ext4, human readable: 7 checks, 7 passed |
| [`selftest-ext4.json`](selftest-ext4.json) | the same command with `--json`, a separate run of the same binary |
| [`selftest-overlayfs.txt`](selftest-overlayfs.txt) | the same selftest on overlayfs: 6 passed, 1 SKIP, and the SKIP explains itself |
| [`bench-ext4.json`](bench-ext4.json) | the benchmark report quoted below. Byte identical to `bench-ext4-run5.json` |
| `bench-ext4-run1.json` .. `bench-ext4-run5.json` | all five benchmark runs, kept so the spread is visible rather than summarised away |
| [`bench-overlayfs.json`](bench-overlayfs.json) | one benchmark run on overlayfs, which fails acceptance on purpose |
| [`accept-output.txt`](accept-output.txt) | `bench/accept.sh` scored against all seven reports above, verbatim |

## The machine

Short version, full version in `env.txt`:

- Linux 6.12.76-linuxkit, aarch64, 10 CPUs, 8 GB RAM, Debian 12 in a Docker container, running as root
- Go 1.24.13, gcc 12.2.0
- checkpoint commit `f07cd88398dfc8fa0039180df33e94db6dfa5051`, built from a clean checkout so the version stamp carries no "MODIFIED working tree" mark
- good case filesystem: ext4 on a 4 GB loopback image mounted at `/mnt/ckext4`
- degraded case filesystem: the container's overlayfs root, via `/tmp`

**These are single machine results.** One arm64 container, one loopback ext4, one
afternoon. They say what this product did here. They are not a claim about your
hardware, your kernel, your filesystem or your workload, and the timing numbers
in particular should be assumed to be different on anything else.

## What was measured

### 1. The guarantees hold on ext4 (`selftest-ext4.txt`)

7 checks, 7 passed, 0 failed, 0 skipped, in 6.9 seconds. Each check drives the
installed binary and scores from bytes on disk: protection starts and cuts a
baseline checkpoint, a write is captured and restored byte exact, `rm -rf` of the
whole workspace is restored across all 10 entries including symlinks, permission
bits and empty directories, a file that existed only between checkpoints is
salvaged, `undo` reverts the agent's file while leaving a concurrent human edit
untouched, an agent's delete is recognised as the agent's and undone, and a `.env`
marker is proven absent from all 27 store files while a control marker from
`README.md` is proven present in one of them.

### 2. The same binary on overlayfs is honest about it (`selftest-overlayfs.txt`)

6 passed, 0 failed, 1 skipped. The skip is `agent-delete-undone`, and the report
states why in the report itself: the dirent change feed is unavailable on
overlayfs, delete attribution is feed scoped, so `undo` cannot know an agent
deleted a file there. Restore by checkpoint still brings the file back, which the
`rm -rf` check covers. This file is committed because a product that quietly
passes a check it never ran is worth less than one that says which guarantee it
could not test.

### 3. Recovery: 100 of 100 scored rounds on ext4

Five benchmark runs, 5 rounds per scenario per run, so 25 rounds per scenario.

| Scenario | checkpoint | git shadow baseline |
|---|---|---|
| `rm_rf_restore` | 25 / 25 | 25 / 25 |
| `transient_salvage` | 25 / 25 | 0 / 25 (`no commit ever saw the transient`) |
| `human_preservation` | 25 / 25 | 0 / 25 (`git has no provenance: reverting the agent reverts the human too`) |
| `agent_delete_undo` | 25 / 25 | not applicable |

The git shadow is git driven as hard as a simple strategy can drive it, with a
force add commit at every turn boundary. The quoted failure strings are the
`detail` fields in the reports, not a paraphrase. Git recovers a deleted tree
perfectly, which is what it is for.

On overlayfs the same benchmark scores `agent_delete_undo` at 0%, with
`change feed unavailable on this filesystem (delete provenance off)` in every
round's `detail`, and `bench/accept.sh` fails. See `bench-overlayfs.json` and the
last block of `accept-output.txt`. That is the environment, not a regression, and
it is exactly what `bench/README.md` says will happen.

### 4. Rollback latency: about 23 ms, and most of what it is not

`undo_ms` is `checkpoint undo` timed end to end, fork/exec until exit, with
nothing subtracted. The workload it was measured against travels with it:
**20 files reverted inside a 500 file tree**, median of **9 accepted samples** per
run, where a sample counts only if undo exited 0 and every file was verified back
to its seeded bytes.

| Run | median ms | min ms | max ms | startup floor ms |
|---|---|---|---|---|
| 1 | 25.338 | 19.952 | 923.134 | 1.594 |
| 2 | 22.823 | 19.748 | 38.508 | 2.013 |
| 3 | 22.086 | 20.247 | 31.537 | 5.882 |
| 4 | 23.923 | 20.618 | 31.025 | 1.665 |
| 5 | 23.186 | 21.670 | 88.541 | 1.544 |

Five run medians spanning 22.1 to 25.3 ms is the most stable thing in this
directory. The tails are not stable: run 1 has a 923 ms sample and run 5 an 88 ms
one, so quote the median with its spread or do not quote it. The startup floor is
the same binary running `checkpoint version`, which touches no store, so roughly
1.5 to 6 ms of every undo above is process launch.

### 5. Recording cost: below this machine's noise floor

This is the number a reader will want, and it is the number this machine cannot
give. The churn workload is **2000 small writes**, timed by the script itself
(median of 3 unwrapped runs against median of 3 wrapped runs), because capture is
off the writer's path.

| Run | unwrapped ms | wrapped ms | us per write | realistic compile overhead |
|---|---|---|---|---|
| 1 | 98 | 79 | -9.25 | +10.66% (165 TUs, 3126 ms unwrapped) |
| 2 | 65 | 66 | +0.30 | +4.78% (204 TUs, 3380 ms) |
| 3 | 98 | 73 | -12.74 | -22.63% (204 TUs, 3910 ms) |
| 4 | 52 | 226 | +87.22 | -31.54% (95 TUs, 2103 ms) |
| 5 | 80 | 79 | -0.42 | +4.14% (171 TUs, 2678 ms) |

Read that honestly. The unwrapped side alone moved between 52 and 98 ms across
five identical runs, and the whole wrapped versus unwrapped difference is a few
tens of milliseconds spread over 2000 writes, so one millisecond of scheduler
noise is worth 0.5 us per write. Three of five runs came back **negative**, which
is not a speedup, it is the measurement telling you the signal is under the noise.
Run 4 is the opposite tail: 226 ms wrapped against 52 ms unwrapped.

The realistic compile overhead is no better. The harness scales the translation
unit count at runtime until one unwrapped build takes at least 3 seconds, so the
workload itself differed between runs (95 to 204 TUs), and the results range from
-31.5% to +10.7%. A negative build overhead is, again, noise, not a result.

What can be said from this data: **on this machine, recording a session costs less
than the run to run variation of the workload being measured, on both a 2000 write
churn burst and a gcc build.** What cannot be said: a microseconds per write
figure. Ten shared vCPUs in a VM backed container is not a measurement rig.

Turn boundary work is more legible, because it is larger than the noise:
`boundary_ms` (drain, settle, cut at the end of a wrapped command, median of 3)
came in at 1183, 1179, 1219, 1616 and 1188 ms, and `routine_cut_ms` (a checkpoint
mid session on a warm daemon with 5 changed files, median of 3, fixed 250 ms quiet
window subtracted) at 32, 27, 34, 41 and 34 ms.

## The 25 us per write threshold is a tripwire, not a result

`bench/accept.sh` fails a report whose `us_per_write` exceeds 25. **That number is
a regression gate. It is not a measured performance claim, and it must never be
quoted as one.** Read it as "if this jumps, someone put work on the writer's
path", nothing more.

This directory contains the evidence for why that distinction matters. Across
five identical runs on the same machine and the same commit, `us_per_write` came
back as -12.74, -9.25, -0.42, +0.30 and +87.22. The gate passed four times and
failed once (`accept-output.txt`, run 4), with no code change between them. A
threshold that a stationary system crosses one run in five is coarser than its own
measurement, which is exactly what you want from a tripwire and exactly what
disqualifies it as a headline.

The same applies to the 5% bar on the realistic compile overhead, which
`accept.sh` prints but does not gate: it printed EXCEEDED on run 1 (+10.66%) and
OK on run 3 (-22.63%), and the second of those is more obviously meaningless than
the first.

## Reproducing this

You need Linux, root or `CAP_SYS_ADMIN` (fanotify), and a filesystem that is not
overlayfs. Everything below is what was actually run, in order.

```sh
# 1. a filesystem that is not the container's overlay
dd if=/dev/zero of=/var/ckbench/ext4.img bs=1M count=4096
mkfs.ext4 -F /var/ckbench/ext4.img
mkdir -p /mnt/ckext4
mount -o loop /var/ckbench/ext4.img /mnt/ckext4

# 2. the binary under test, from a clean checkout so `version` is unambiguous
git clone <repo> /tmp/cp-clean && cd /tmp/cp-clean
git checkout f07cd88
mkdir -p /tmp/ckreport
go build -o /tmp/ckreport/ck    ./cmd/checkpoint
go build -o /tmp/ckreport/bench ./bench
/tmp/ckreport/ck version   # must print the commit with no MODIFIED mark

# 3. selftest, on ext4 and then on overlayfs
cd /mnt/ckext4
/tmp/ckreport/ck selftest        --work /mnt/ckext4 > selftest-ext4.txt
/tmp/ckreport/ck selftest --json --work /mnt/ckext4 > selftest-ext4.json
mkdir -p /tmp/ckoverlay && cd /tmp/ckoverlay
/tmp/ckreport/ck selftest        --work /tmp/ckoverlay > selftest-overlayfs.txt

# 4. the benchmark, five times, keeping every report
cd /tmp/cp-clean
for i in 1 2 3 4 5; do
  /tmp/ckreport/bench --bin /tmp/ckreport/ck --base /mnt/ckext4 \
    --rounds 5 --out bench-ext4-run$i.json
done
/tmp/ckreport/bench --bin /tmp/ckreport/ck --base /tmp/ckoverlay \
  --rounds 5 --out bench-overlayfs.json

# 5. score them
for f in bench-ext4-run*.json bench-overlayfs.json; do bash bench/accept.sh $f; done

# 6. put the machine back
umount /mnt/ckext4 && rmdir /mnt/ckext4 && rm -f /var/ckbench/ext4.img
```

`make bench BENCH_BASE=/mnt/ckext4 BENCH_ROUNDS=5` does step 4 in one line, with
`sudo` and `results/bench.json` handled for you. The direct invocation is used
here only because `results/` is gitignored, and these reports were meant to be
committed.

The selftest text and JSON reports come from **two separate runs**, because the
CLI emits one format per run. They are the same command against the same binary a
few seconds apart, so the store paths and durations inside them differ, and the
verdicts do not.

## Why `bench-ext4.json` is run 5

Keeping one run out of five invites the question of which one, so: run 5 is the
**median run on every reported metric**, not the best one.

| Metric | run values, sorted | median | run 5 |
|---|---|---|---|
| `us_per_write` | -12.74, -9.25, **-0.42**, 0.30, 87.22 | -0.42 | -0.42 |
| `overhead_realistic_pct` | -31.54, -22.63, **4.14**, 4.78, 10.66 | 4.14 | 4.14 |
| `undo_ms` | 22.09, 22.82, **23.19**, 23.92, 25.34 | 23.19 | 23.19 |
| `boundary_ms` | 1179, 1183, **1188**, 1219, 1616 | 1188 | 1188 |
| `routine_cut_ms` | 27, 32, **34**, 34, 41 | 34 | 34 |

All five runs are committed anyway, so the choice changes nothing that a reader
cannot check. Run 4, the one that fails acceptance, is in here too.
