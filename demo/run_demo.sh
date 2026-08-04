#!/usr/bin/env bash
#
# The checkpoint demo: an 8-step story that asserts itself.
#
# Every step verifies its own outcome against the filesystem and exits nonzero
# if the outcome is not the one it claims. Nothing here is narrated: if you see
# "STEP n OK", the assertion printed under it ran and passed on THIS machine.
#
# It runs entirely inside a throwaway sandbox with its own project, its own
# store and its own daemon. It never touches your real projects, your real
# store under ~/.local/share/checkpoint, or anything else you own.
#
#   demo/run_demo.sh                  # run on this machine's filesystem
#   demo/run_demo.sh --ext4           # provision a loopback ext4 image first
#   demo/run_demo.sh --base /mnt/ext4 # put the sandbox somewhere specific
#   demo/run_demo.sh --keep           # leave the sandbox behind to poke at
#   demo/run_demo.sh --bin /usr/local/bin/checkpoint
#
# See demo/README.md for what each step proves and what it does NOT.

# No `pipefail`: several checks are `... | head -1 | grep -q`, where the head
# closes the pipe early and SIGPIPEs the producer. Where a pipeline's FIRST
# command is the one under test (the checkpoint invocations piped into tee),
# its status is read explicitly from PIPESTATUS.
set -eu

# ---------------------------------------------------------------- arguments --
BASE="${TMPDIR:-/tmp}"
BIN="${CHECKPOINT_BIN:-}"
KEEP=0
EXT4=0
while [ $# -gt 0 ]; do
  case "$1" in
    --base) BASE="${2:?--base needs a directory}"; shift 2 ;;
    --bin)  BIN="${2:?--bin needs a path}";        shift 2 ;;
    --ext4) EXT4=1; shift ;;
    --keep) KEEP=1; shift ;;
    -h|--help) sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option: $1 (try --help)" >&2; exit 2 ;;
  esac
done

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# ------------------------------------------------------------------- output --
BOLD=""; DIM=""; RESET=""
if [ -t 1 ]; then BOLD=$'\033[1m'; DIM=$'\033[2m'; RESET=$'\033[0m'; fi

now_ms() { date +%s%3N; }
T_ALL=$(now_ms)
T_STEP=$T_ALL

step() { T_STEP=$(now_ms); printf '\n%s=== STEP %s: %s%s\n' "$BOLD" "$1" "$2" "$RESET"; }
part() { printf '\n%s--- %s%s\n' "$BOLD" "$1" "$RESET"; }
ok()   { printf '%sSTEP %s OK%s  (%s ms): %s\n' "$BOLD" "$1" "$RESET" "$(( $(now_ms) - T_STEP ))" "$2"; }
die()  { printf '\nSTEP %s FAILED: %s\n' "$1" "$2" >&2; exit 1; }
note() { printf '  note: %s\n' "$1"; }
sub()  { printf '  - %s\n' "$1"; }

# cp_: echo the command the way a user would type it, then run the real binary.
# The echo goes to STDERR on purpose: `X="$(cp_ status ...)"` must capture the
# command's own output and nothing else.
cp_() { printf '%s$ checkpoint %s%s\n' "$DIM" "$*" "$RESET" >&2; "$BIN" "$@"; }

# ------------------------------------------------------------------ cleanup --
SB=""; PROJ=""; STORE=""; MNT=""; WORK=""
cleanup() {
  code=$?
  set +e
  cd /                       # never hold a cwd inside the sandbox (umount/rm -rf)
  if [ -n "$STORE" ] && [ -d "$STORE" ]; then
    "$BIN" protect --store "$STORE" --stop "$PROJ" >/dev/null 2>&1
  fi
  if [ "$KEEP" = 1 ]; then
    [ -n "$WORK" ] && printf '\nsandbox kept: %s\n' "$WORK"
  else
    [ -n "$MNT" ] && umount "$MNT" >/dev/null 2>&1
    [ -n "$WORK" ] && rm -rf "$WORK"
  fi
  if [ "$code" != 0 ]; then
    printf '\n%sDEMO FAILED (exit %s)%s. The failure is real; nothing above it was faked.\n' \
      "$BOLD" "$code" "$RESET" >&2
  fi
  exit "$code"
}
trap cleanup EXIT

# --------------------------------------------------------------- the binary --
if [ -z "$BIN" ]; then
  BIN="$REPO/bin/checkpoint"
  if [ ! -x "$BIN" ]; then
    printf 'building %s ...\n' "$BIN"
    command -v go >/dev/null 2>&1 || {
      echo "no bin/checkpoint and no go toolchain: build it, or pass --bin /path/to/checkpoint" >&2; exit 1; }
    (cd "$REPO" && go build -o bin/checkpoint ./cmd/checkpoint) \
      || { echo "build failed: fix the build, or pass --bin /path/to/an/installed/checkpoint" >&2; exit 1; }
  fi
fi
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 1; }
printf 'binary:  %s\n' "$BIN"
printf 'version: %s\n' "$("$BIN" version 2>/dev/null | head -1 || echo unknown)"

# -------------------------------------------------------------- the sandbox --
mkdir -p "$BASE" 2>/dev/null || true
WORK="$(mktemp -d "${BASE%/}/checkpoint-demo.XXXXXX")"

if [ "$EXT4" = 1 ]; then
  # An explicit request gets a hard failure, never a silent fallback onto the
  # filesystem the user just asked us not to use.
  printf 'provisioning a loopback ext4 image (needs root + mkfs.ext4 + mount) ...\n'
  IMG="$WORK/ext4.img"; MNT="$WORK/mnt"; mkdir -p "$MNT"
  dd if=/dev/zero of="$IMG" bs=1M count=256 status=none \
    || { echo "--ext4: dd failed" >&2; exit 1; }
  mkfs.ext4 -q "$IMG" || { echo "--ext4: mkfs.ext4 failed (not installed?)" >&2; exit 1; }
  mount -o loop "$IMG" "$MNT" \
    || { MNT=""; echo "--ext4: mount failed (needs root/CAP_SYS_ADMIN and a free loop device)" >&2; exit 1; }
  SB="$MNT/sandbox"
else
  SB="$WORK/sandbox"
fi

PROJ="$SB/acme-api"       # the "project": the protected root
STORE="$SB/store"         # the out-of-tree store (deliberately NOT in the project)
export OUTSIDE="$SB/outside"   # a folder nobody protected; the agent writes there
SALVAGE="$SB/salvage"
mkdir -p "$PROJ" "$STORE" "$OUTSIDE" "$SALVAGE"

# The daemon socket lives in the store, and sockaddr_un caps the path near 108
# bytes. Fail now, with the remedy, rather than after a readiness timeout.
if [ "${#STORE}" -gt 80 ]; then
  echo "store path too long for a unix socket ($STORE); rerun with --base /tmp" >&2; exit 1
fi

# ------------------------------------------------------------------ helpers --
# fingerprint: everything a byte-exact restore has to reproduce, namely the set
# of paths, each one's kind, permission bits, symlink target and content hash.
fingerprint() {
  ( cd "$1" && find . -mindepth 1 | LC_ALL=C sort | while IFS= read -r p; do
      if   [ -L "$p" ]; then printf 'L %s -> %s\n' "$p" "$(readlink "$p")"
      elif [ -d "$p" ]; then printf 'D %s %s\n' "$p" "$(stat -c %a "$p")"
      else printf 'F %s %s %s\n' "$p" "$(stat -c %a "$p")" "$(sha256sum "$p" | cut -d' ' -f1)"
      fi
    done )
}
sha() { sha256sum "$1" | cut -d' ' -f1; }
has() { grep -qF -- "$2" "$1"; }

# ============================================================================ #
step 1 "a real project, protection started"

cat > "$PROJ/server.py" <<'PYEOF'
from routes import legacy_route

def handle(req):
    return legacy_route(req)
PYEOF
cat > "$PROJ/routes.py" <<'PYEOF'
def legacy_route(req):
    return "v1: " + req
PYEOF
cat > "$PROJ/NOTES.md" <<'MDEOF'
# my notes
things I am in the middle of writing
MDEOF
cat > "$PROJ/scratch.md" <<'MDEOF'
scratch file the agent will decide to delete
MDEOF
mkdir -p "$PROJ/bin" "$PROJ/logs"                   # logs/ stays empty on purpose
printf '#!/bin/sh\necho build\n' > "$PROJ/bin/build.sh"
chmod 755 "$PROJ/bin/build.sh"                      # an exec bit that must survive
ln -sf ../routes.py "$PROJ/bin/routes.py"           # a symlink that must survive
printf 'retries=1\n' > "$OUTSIDE/app.conf"          # outside every protected folder

sub "project: $PROJ ($(find "$PROJ" -type f | wc -l) files, an empty dir, an exec bit, a symlink)"
sub "store:   $STORE (outside the project, which is the whole point)"

cp_ doctor --root "$PROJ" --store "$STORE" || die 1 "doctor reported a FATAL problem on this machine"

cp_ protect --store "$STORE" "$PROJ" >/dev/null || die 1 "protect failed"
STATUS="$(cp_ status --root "$PROJ" --store "$STORE")" || die 1 "status failed"
printf '%s\n' "$STATUS"
printf '%s' "$STATUS" | grep -q '^Protection: Protected' || die 1 "not Protected after protect"
printf '%s' "$STATUS" | grep -q '^Complete baseline: yes' || die 1 "no complete baseline after setup"

FEED=unavailable
if printf '%s' "$STATUS" | grep -q '^Change feed: active'; then FEED=active; fi
if [ "$FEED" = active ]; then
  note "change feed ACTIVE: deletions carry provenance on this filesystem"
else
  note "change feed UNAVAILABLE here: overlayfs, which is what a default Docker container gives you."
  note "STEP 5 shows exactly what that costs, on this machine, instead of pretending otherwise."
fi
ok 1 "protection is live, with one complete baseline checkpoint"

# ============================================================================ #
step 2 "an agent turn (wrapped): refactor two files, delete a third, write outside"

cat > "$SB/agent_turn.sh" <<'AGENTEOF'
#!/usr/bin/env bash
# A scripted stand-in for a coding agent. checkpoint neither knows nor cares
# what this process is: it wraps `claude`, `codex`, `aider` or a bash script
# identically. It is scripted so this demo is deterministic and re-runnable.
set -eu
edit() { local c; c="$(cat "$1")"; printf '%s\n' "${c//legacy_route/handle_v2}" > "$1"; }

echo "AGENT_START $(date +%s%3N)"
edit server.py                                    # "refactor" file 1
edit routes.py                                    # "refactor" file 2
rm scratch.md                                     # delete file 3, via plain bash
printf '{"generated":true}\n' > .agent-tmp.json   # a transient: created ...
sleep 1.2                                         # <- the human's edit lands here
rm -f .agent-tmp.json                             # ... and deleted, same turn
printf 'retries=5\n' > "$OUTSIDE/app.conf"        # a write OUTSIDE the project
echo "AGENT_END $(date +%s%3N)"
AGENTEOF
chmod +x "$SB/agent_turn.sh"

NOTES="$PROJ/NOTES.md"
PRE_SCRATCH_SHA="$(sha "$PROJ/scratch.md")"
PRE_NOTES_SHA="$(sha "$NOTES")"

# The human: editing a DIFFERENT file DURING the agent's turn, from outside the
# agent's process tree (a child of this script, never of `checkpoint run`).
( sleep 0.5
  printf 'a line I typed myself while the agent was working\n' >> "$NOTES"
  date +%s%3N > "$SB/human.ts"
  sleep 0.2 ) &
HUMAN_JOB=$!

cd "$PROJ"
set +e
cp_ run --root "$PROJ" --store "$STORE" -- "$SB/agent_turn.sh" 2>&1 | tee "$SB/run.out"
RUN_RC=${PIPESTATUS[0]}
set -e
wait "$HUMAN_JOB"
[ "$RUN_RC" = 0 ] || die 2 "checkpoint run exited $RUN_RC"

grep -q 'handle_v2' "$PROJ/server.py" && grep -q 'handle_v2' "$PROJ/routes.py" \
  || die 2 "the agent's refactor did not land"
[ ! -e "$PROJ/scratch.md" ] || die 2 "the agent's delete did not land"
has "$OUTSIDE/app.conf" 'retries=5' || die 2 "the agent's outside-the-project write did not land"
grep -q 'checkpoint [0-9]* DURABLE' "$SB/run.out" \
  || die 2 "no durable checkpoint was cut at the turn boundary"
sub "two files refactored, scratch.md deleted, one transient created+deleted, one write outside"
ok 2 "the agent turn ran wrapped and ended in exactly one durable checkpoint"

# ============================================================================ #
step 3 "MEANWHILE: the human edited a different file, mid-turn"

A_START="$(sed -n 's/^AGENT_START \([0-9]*\)$/\1/p' "$SB/run.out" | head -1)"
A_END="$(sed -n 's/^AGENT_END \([0-9]*\)$/\1/p' "$SB/run.out" | head -1)"
H_TS="$(cat "$SB/human.ts" 2>/dev/null || echo 0)"
[ -n "$A_START" ] && [ -n "$A_END" ] || die 3 "the agent turn did not report its own start/end times"
has "$NOTES" 'a line I typed myself' || die 3 "the human edit is missing from NOTES.md"
{ [ "$H_TS" -gt "$A_START" ] && [ "$H_TS" -lt "$A_END" ]; } \
  || die 3 "the human edit did not land DURING the agent turn (agent $A_START..$A_END, human $H_TS)"
sub "agent turn spanned ${A_START}..${A_END}; the human write landed at ${H_TS}, strictly inside it"
sub "the human process was never a descendant of \`checkpoint run\`, which is how they are told apart"
ok 3 "a human edit landed inside the agent's turn, in a file the agent never touched"

# ============================================================================ #
step 4 "the honest state: status + history"

STATUS="$(cp_ status --root "$PROJ" --store "$STORE")" || die 4 "status failed"
printf '%s\n' "$STATUS"
HIST="$(cp_ history --root "$PROJ" --store "$STORE")" || die 4 "history failed"
printf '%s\n' "$HIST"

printf '%s' "$HIST" | head -1 | grep -q 'agent_turn.sh' \
  || die 4 "the newest checkpoint is not the agent turn"
printf '%s' "$HIST" | head -1 | grep -qi 'recoverable' \
  || die 4 "the agent-turn checkpoint carries no recovery badge"
# The agent wrote outside every protected folder. That write is NOT captured,
# and checkpoint has to say so rather than quietly grading itself Protected.
printf '%s' "$STATUS" | grep -q '^Protection: Limited protection' \
  || die 4 "an uncaptured agent write outside the project did not downgrade protection"
printf '%s' "$STATUS" | grep -qF "$OUTSIDE/app.conf" \
  || die 4 "status does not name the uncaptured path $OUTSIDE/app.conf"
sub "protection is Limited, and status names the exact path it could not cover"
sub "an unrecoverable write is surfaced as a downgrade, never graded as covered"
ok 4 "status and history report what was covered AND what was not"

# ============================================================================ #
step 5 "undo: the agent's changes go, yours stay"

BASE_ID="$(printf '%s' "$HIST" | sed -n 's/^#\([0-9]*\) .*setup.*/\1/p' | head -1)"
[ -n "$BASE_ID" ] || BASE_ID=0
NOTES_BEFORE_UNDO="$(sha "$NOTES")"

set +e
cp_ undo --root "$PROJ" --store "$STORE" 2>&1 | tee "$SB/undo.out"
UNDO_RC=${PIPESTATUS[0]}
set -e
[ "$UNDO_RC" = 0 ] || die 5 "undo exited $UNDO_RC"

grep -q 'legacy_route' "$PROJ/server.py" && grep -q 'legacy_route' "$PROJ/routes.py" \
  || die 5 "the agent's refactor was NOT reverted"
if grep -q 'handle_v2' "$PROJ/server.py"; then die 5 "server.py still holds the agent's refactor"; fi
sub "both refactored files reverted, whole-file"

# The differentiator: the concurrent human edit must be bit-for-bit untouched.
[ "$(sha "$NOTES")" = "$NOTES_BEFORE_UNDO" ] \
  || die 5 "THE HUMAN'S FILE WAS MODIFIED BY UNDO: the core promise is broken"
[ "$(sha "$NOTES")" != "$PRE_NOTES_SHA" ] \
  || die 5 "NOTES.md matches its pre-turn hash: the human edit was reverted"
has "$NOTES" 'a line I typed myself' || die 5 "the human's line is gone"
sub "NOTES.md byte-identical to before the undo: the human edit survived"

# The agent-deleted file. This is the one behavior the change feed gates.
if [ "$FEED" = active ]; then
  [ -f "$PROJ/scratch.md" ] || die 5 "the agent-deleted file was not restored (the change feed is active here, so it should have been)"
  [ "$(sha "$PROJ/scratch.md")" = "$PRE_SCRATCH_SHA" ] || die 5 "the restored file is not byte-exact"
  sub "the agent-deleted file was restored byte-exact (delete provenance, via the change feed)"
else
  if [ -e "$PROJ/scratch.md" ]; then
    die 5 "scratch.md reappeared, but this filesystem cannot attribute deletions, so undo must not guess"
  fi
  grep -q 'cannot attribute deletions' "$SB/undo.out" \
    || die 5 "no change feed here, but undo did not say that deletions are unattributable"
  note "DEGRADED, HONESTLY: with no change feed, checkpoint cannot prove whether the agent"
  note "or you deleted scratch.md, so it refuses to guess. It still HAS the file, says so, and"
  note "prints the command that brings it back. Running that exact command now:"
  REMEDY_ID="$(sed -n 's/.*restore --only "[^"]*" \([0-9]*\) .*/\1/p' "$SB/undo.out" | head -1)"
  [ -n "$REMEDY_ID" ] || REMEDY_ID="$BASE_ID"
  cp_ restore --only scratch.md --store "$STORE" "$REMEDY_ID" "$PROJ" \
    || die 5 "the remedy undo printed does not work"
  [ "$(sha "$PROJ/scratch.md")" = "$PRE_SCRATCH_SHA" ] \
    || die 5 "the remedy restored something other than the original bytes"
  sub "the deleted file came back byte-exact via the documented manual remedy"
fi

grep -q 'pre-undo checkpoint [0-9]* saved' "$SB/undo.out" || die 5 "the undo is not itself undoable"
sub "a pre-undo checkpoint was cut first, so this undo is itself undoable"
note "known gap: edits made by write-temp-then-rename (\`sed -i\`, and the atomic"
note "saves some editors do) are NOT reverted by undo today. See demo/README.md."

part "5b: and when you BOTH edited the same file"

cat > "$SB/agent_turn2.sh" <<'AGENTEOF'
#!/usr/bin/env bash
set -eu
printf 'a line the AGENT appended to your notes\n' >> NOTES.md   # <- you touch this too
mkdir -p drafts && printf 'draft the agent wrote\n' > drafts/plan.md
sleep 1.2
AGENTEOF
chmod +x "$SB/agent_turn2.sh"

( sleep 0.5; printf 'and a line I typed into the same file\n' >> "$NOTES"; sleep 0.2 ) &
HUMAN_JOB=$!
set +e
cp_ run --root "$PROJ" --store "$STORE" -- "$SB/agent_turn2.sh" >"$SB/run2.out" 2>&1
RUN_RC=$?
set -e
wait "$HUMAN_JOB"
[ "$RUN_RC" = 0 ] || die 5 "the second agent turn exited $RUN_RC: $(cat "$SB/run2.out")"
CONFLICT_SHA="$(sha "$NOTES")"
has "$NOTES" 'a line the AGENT appended' || die 5 "the agent's conflicting write did not land"
has "$NOTES" 'and a line I typed into the same file' || die 5 "the human's conflicting write did not land"

set +e
cp_ undo --root "$PROJ" --store "$STORE" >"$SB/undo2.out" 2>&1
UNDO_RC=$?
set -e
cat "$SB/undo2.out"
[ "$UNDO_RC" != 0 ] || die 5 "undo SUCCEEDED over an unresolved conflict; it must refuse"
grep -q 'needs review' "$SB/undo2.out" || die 5 "the conflict was not reported by name"
grep -q 'NOTHING was changed' "$SB/undo2.out" || die 5 "undo did not state that it aborted before mutating"
[ "$(sha "$NOTES")" = "$CONFLICT_SHA" ] || die 5 "a refused undo modified the conflicted file"
[ -f "$PROJ/drafts/plan.md" ] || die 5 "a refused undo mutated other files before aborting"
sub "undo exited $UNDO_RC and changed nothing at all, not even the files it could have reverted"

set +e
cp_ undo --save-both --root "$PROJ" --store "$STORE" 2>&1 | tee "$SB/undo3.out"
UNDO_RC=${PIPESTATUS[0]}
set -e
[ "$UNDO_RC" = 0 ] || die 5 "undo --save-both exited $UNDO_RC"
[ ! -e "$PROJ/drafts/plan.md" ] || die 5 "--save-both did not revert the agent-only file"
[ "$(sha "$NOTES")" = "$CONFLICT_SHA" ] || die 5 "--save-both modified the live conflicted file"
SIB="$(ls "$PROJ"/NOTES.md.checkpoint-* 2>/dev/null | head -1 || true)"
[ -n "$SIB" ] || die 5 "--save-both did not write the checkpoint version alongside"
has "$SIB" 'things I am in the middle of writing' || die 5 "the saved sibling is not the checkpoint version"
if grep -q 'a line the AGENT appended' "$SIB"; then die 5 "the saved sibling contains the agent's line"; fi
sub "your file untouched; the checkpoint version written alongside as $(basename "$SIB")"
sub "nothing was line-merged, and nothing of yours was overwritten"
ok 5 "agent changes reverted, human edits untouched, conflicts refused before any mutation"

# ============================================================================ #
step 6 "the disaster: rm -rf the whole project, then restore it byte-exact"

SAVE_OUT="$(cp_ save --root "$PROJ" --store "$STORE" --name before-disaster)" || die 6 "save failed"
printf '%s\n' "$SAVE_OUT"
SAVE_ID="$(printf '%s' "$SAVE_OUT" | sed -n 's/^checkpoint \([0-9]*\).*/\1/p')"
[ -n "$SAVE_ID" ] || die 6 "could not read the checkpoint id from: $SAVE_OUT"

fingerprint "$PROJ" > "$SB/fp.before"
sub "recorded $(wc -l < "$SB/fp.before") paths with modes, symlink targets and content hashes"

cd "$SB"
printf '%s$ rm -rf %s%s\n' "$DIM" "$PROJ" "$RESET"
rm -rf "$PROJ"
[ ! -e "$PROJ" ] || die 6 "the project is somehow still there"
sub "the project directory no longer exists"

T_RESTORE=$(now_ms)
cp_ restore --store "$STORE" "$SAVE_ID" "$PROJ" || die 6 "restore failed"
RESTORE_MS=$(( $(now_ms) - T_RESTORE ))
fingerprint "$PROJ" > "$SB/fp.after"
diff -u "$SB/fp.before" "$SB/fp.after" > "$SB/fp.diff" \
  || die 6 "restore was NOT byte-exact:
$(cat "$SB/fp.diff")"
[ -x "$PROJ/bin/build.sh" ] || die 6 "exec bit lost"
[ -L "$PROJ/bin/routes.py" ] || die 6 "symlink lost"
[ -d "$PROJ/logs" ] || die 6 "empty directory lost"
sub "every path, mode, exec bit, symlink target and byte identical (${RESTORE_MS} ms)"
ok 6 "rm -rf survived: the project was rebuilt byte-exact from the out-of-tree store"

# ============================================================================ #
step 7 "the transient: a file created and deleted inside the turn"

REC="$(cp_ recover --store "$STORE" "$PROJ")" || die 7 "recover failed"
printf '%s\n' "$REC"
printf '%s' "$REC" | grep -q '\.agent-tmp\.json' \
  || die 7 "the transient file is not offered for recovery (no checkpoint ever held it)"
cp_ recover --store "$STORE" --to "$SALVAGE" "$PROJ" || die 7 "recover --to failed"
[ -f "$SALVAGE/.agent-tmp.json" ] || die 7 "the transient was not extracted"
[ "$(cat "$SALVAGE/.agent-tmp.json")" = '{"generated":true}' ] \
  || die 7 "the recovered transient is not byte-exact: $(cat "$SALVAGE/.agent-tmp.json")"
sub "no checkpoint ever contained this file: capture is continuous, and checkpoints are only labels over it"
ok 7 "a file that existed only between checkpoints was recovered byte-exact"

# ============================================================================ #
step 8 "cleanup"

cp_ protect --store "$STORE" --stop "$PROJ" || die 8 "protect --stop failed"
STATUS="$(cp_ status --root "$PROJ" --store "$STORE")" || die 8 "status failed after stop"
printf '%s\n' "$STATUS"
printf '%s' "$STATUS" | grep -q '^Protection: Not protected' \
  || die 8 "protection did not stop (there is no Paused state; stopped reads Not protected)"
sub "daemon stopped and socket removed; the store is still on disk and still restorable"
STORE_BYTES="$(du -sh "$STORE" 2>/dev/null | cut -f1)"
sub "store size for this entire demo: ${STORE_BYTES:-unknown}"
if [ "$KEEP" = 0 ]; then sub "sandbox (project, store, daemon) about to be deleted; your machine is unchanged"; fi
ok 8 "protection stopped cleanly"

TOTAL_MS=$(( $(now_ms) - T_ALL ))
printf '\n%s────────────────────────────────────────────────────────────────────────%s\n' "$BOLD" "$RESET"
printf '%sPROVEN ON THIS MACHINE IN %s.%ss%s. Every line below was asserted against disk state:\n' \
  "$BOLD" "$((TOTAL_MS/1000))" "$(( (TOTAL_MS%1000)/100 ))" "$RESET"
printf '  an agent refactored 2 files, deleted 1, created and deleted a transient, and wrote\n'
printf '  outside the project, while a human edited a file alongside it. checkpoint reverted\n'
printf '  every agent change and nothing of the human'\''s; refused to touch a file they both\n'
printf '  edited (and kept both versions on request); rebuilt the project byte-exact after\n'
printf '  rm -rf; recovered a file no checkpoint ever held; and named every path it could not\n'
printf '  cover.\n'
if [ "$FEED" = active ]; then
  printf '  Change feed ACTIVE: agent-deleted files were restored by undo automatically.\n'
else
  printf '  Change feed UNAVAILABLE on this filesystem: automatic delete attribution was NOT\n'
  printf '  demonstrated (it cannot be here); the documented manual remedy was run instead.\n'
fi
printf '%s────────────────────────────────────────────────────────────────────────%s\n' "$BOLD" "$RESET"
