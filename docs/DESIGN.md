# How checkpoint works

One page. Every name below is a real package or function; read it with the
code open.

## The problem in one sentence

When an AI agent edits a project for hours, git cannot tell the agent's
edits from yours inside one working tree, and a file the agent created and
deleted between commits never existed. checkpoint records every write with
its author, so the agent's work can be reverted while yours survives.

## The data flow

```
   kernel: "a file was just closed after writing" ──► watch.Writes
   kernel: "a name was created/deleted/renamed"   ──► watch.Changes
                                  │
                                  ▼
                        daemon.recorder.write()
                 ┌────────────────┼──────────────────┐
                 ▼                ▼                  ▼
        objects.Put(bytes)  lineage.Who(pid)   writelog.Append(entry)
        content by hash     agent|human|self   one line per write
                                  │
   turn ends / save / autosave ───┤
                                  ▼
                        daemon.server.cut()
                     snapshot.Scan or snapshot.Fold
                     snapshot.Commit  ──► checkpoints/N.json
                                  │
            history ─ status ─ restore ─ undo ─ recover
```

## The packages, in dependency order

| package | one job | the function to read first |
| --- | --- | --- |
| `internal/objects` | store bytes by SHA-1 in git's loose-object format | `Put` |
| `internal/writelog` | append one JSON line per write, survive a crash mid-line | `Open` (torn-tail repair) |
| `internal/snapshot` | record the whole tree, put it back, prune, find transients | `Scan`, `Restore`, `Prune` |
| `internal/lineage` | decide agent / human / self / unknown from the process tree | `Classify`, `Tracker.Lineage` |
| `internal/watch` | the two fanotify listeners, nothing else | `Writes.Drain`, `Changes.Drain` |
| `internal/daemon` | record continuously, cut checkpoints when asked | `server.cut`, `recorder.write` |
| `internal/undo` | plan and apply the author-scoped revert | `Plan` |
| `internal/doctor` | will this work here? probe, don't guess | `Run` |
| `internal/selftest` | prove the guarantees on this machine with the real binary | `Run` |
| `cmd/checkpoint` | the commands; thin | `common.go` then any command |

Nothing above imports anything below it in the table, except that `daemon`
and `undo` both sit on `snapshot`.

## Where the history lives

```
~/.local/share/checkpoint/<project>-<hash>/
  root                 which project this store belongs to
  objects/ab/cdef...   file contents, git loose-object format
  writes.jsonl         the write log: path, ref, mode, writer, time
  checkpoints/0.json   one manifest per checkpoint: every path -> entry
  daemon.sock/pid/log  the running daemon
```

The store is always outside the project. `snapshot.CheckStoreLocation`
refuses anything else, on real paths, so `rm -rf project` cannot take the
history with it.

## The five decisions worth defending

1. **Capture on close, through the kernel's descriptor.** fanotify's
   `FAN_CLOSE_WRITE` event carries an open fd to the file. Reading through
   it works even if the file was unlinked a microsecond earlier. This is why
   a file created and deleted inside one turn is still recoverable
   (`recover`). See `watch/writes_linux.go`, `daemon/recorder_linux.go`.

2. **Author by process lineage, not by time window.** A write is the
   agent's only if the writing pid descends from the process `checkpoint
   run` started. A human edit made during the agent's turn is still the
   human's. Short-lived writers (`rm`) can be gone before we ask about their
   parent, so `lineage.Tracker` keeps a two-second cache of /proc's parent
   table while an agent session is active. See `lineage/tracker_linux.go`.

3. **Checkpoints are whole trees, not diffs.** A manifest lists every path
   with its content hash, mode, and symlink target. Restore is a pure
   function of (manifest, target directory). Incremental cost comes from
   reusing hashes for unchanged files (size + mtime + ctime) and, with the
   change feed, folding only the changed paths (`snapshot.Fold`).

4. **Undo never merges.** `undo.Plan` groups the write log by path. Agent-only
   paths are reverted or removed. A path anyone else also wrote is a
   conflict: skipped, reported, and optionally saved alongside. Checkpoint's
   own restore writes are `self` and never count as either author. Mistakes
   can only under-revert.

5. **Honesty over coverage.** Every checkpoint carries a badge. Named
   exceptions list what it could not capture (credential files, unreadable
   entries). A kernel queue overflow marks the window `Incomplete`. `doctor`
   and `selftest` say what works on *this* machine before you rely on it.

## What is deliberately not here

- One protected folder per store. Protect two folders with two daemons.
- No operation journal for interrupted undo/restore: both cut a checkpoint
  of the present first, so the way back is `restore <that id>`.
- No power-loss guarantee. Process-crash consistency only (no fsync per
  write; `Sync` at every checkpoint).
- Not a backup: the store is on the same disk.
