package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// prog is the name this binary was invoked as. It ships as `checkpoint` but may
// be renamed by a packager, so every message that names the command reads it
// from argv rather than hardcoding one.
func prog() string {
	if len(os.Args) > 0 && os.Args[0] != "" {
		return filepath.Base(os.Args[0])
	}
	return "checkpoint"
}

func usage() {
	fmt.Fprintf(os.Stderr, `%[1]s: version control for long-running agent sessions

usage (flags must precede positional args):
  %[1]s create  [--store DIR] <project-dir>
  %[1]s restore [--store DIR] [--include-extra [--yes]] [--only rel,rel] <id> <target-dir>
  %[1]s capture [--store DIR] <workspace>            (linux; needs CAP_SYS_ADMIN)
  %[1]s recover [--store DIR] [--to DIR] <workspace>
  %[1]s daemon  [--store DIR] [--protect DIR,DIR] <root>   (linux; needs CAP_SYS_ADMIN)
  %[1]s protect [--store DIR] [--protect DIR,DIR] [--stop] [<root>]   (standing protection, detached)
  %[1]s doctor  [--root DIR] [--store DIR]        (will this work on THIS machine?)
  %[1]s selftest [--json]                        (prove the guarantees hold HERE)
  %[1]s run     [--root DIR] [--store DIR] -- <command...>
  %[1]s save    [--root DIR] [--store DIR] [--source LABEL] [--name LABEL]
  %[1]s undo    [--root DIR] [--store DIR] [--only rel,rel] [--save-both]
  %[1]s history [--root DIR] [--store DIR] [--json]
  %[1]s prune   [--root DIR] [--store DIR] [--keep-days N] [--dry-run] [--yes]
  %[1]s status  [--root DIR] [--store DIR] [--json]
  %[1]s ui      [--root DIR] [--store DIR]
  %[1]s version                          (which commit this binary was built from)
  %[1]s register-agent   [--root DIR] [--store DIR] --pid N
  %[1]s unregister-agent [--root DIR] [--store DIR] --pid N

typical use:
  %[1]s doctor                   will this work on this machine?
  %[1]s protect                  start recording this project's session history
  %[1]s run -- <agent command>   run an agent; its turn becomes a checkpoint
  %[1]s undo                     revert that turn's agent-only changes

coming from git:
  git log             -> %[1]s history
  git checkout <sha>  -> %[1]s restore <id>
  git revert          -> %[1]s undo      (reverts the agent, keeps your edits)
  git stash           -> nothing to do; the turn was already recorded
  git gc              -> %[1]s prune
  (no equivalent)     -> %[1]s recover   (files that never lived to a commit)

checkpoint is not a replacement for git: no branches, no merges, no remotes,
nothing to publish. Git holds your project's history; this holds the session's,
written automatically, with authorship per file, in a store outside the repo.

create/restore: checkpoint the whole project and restore it by id.
capture: continuously save every completed write, so a file created and
deleted before any checkpoint is still recoverable. recover: list (or extract
with --to) those transient files no checkpoint holds. daemon: run the always-on
protector for a root. run: execute a command and, when it exits, ask the daemon
to cut one checkpoint (settling briefly to absorb trailing writes). save: ask the
daemon to cut one checkpoint now. Save is the source-agnostic boundary, used both
by an agent-turn hook (e.g. Claude Code's Stop hook) and by hand; --name names the
checkpoint (named checkpoints survive pruning and always cut, even with no
changes). undo: revert the latest checkpoint's agent-only changes, preserving
human edits; snapshots the present first so the undo is itself undoable.
prune: delete unnamed checkpoints older than --keep-days (default 7), then
reclaim unreferenced content; named checkpoints and the latest durable baseline
always survive. Prune requires the daemon stopped.

The store lives outside the project (default: $XDG_DATA_HOME/checkpoint/<key>),
so deleting the project cannot destroy its checkpoints. Restoring in place uses
the same default store as create; use --store to point elsewhere.
`, prog())
}
