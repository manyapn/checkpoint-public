#!/usr/bin/env python3
"""Run a command on a pty of a chosen size, typing input when the output says to.

Exists because asciinema 2.2.0 takes its geometry from the terminal it is
started on, and this session has no controlling terminal. Nothing here changes
what the recorded program does: it gives it a real tty of a known size and
types the same characters a human would type, at the same points a human would.

  --send-when 'REGEX::TEXT'   type TEXT once REGEX matches the output so far
                              and the output has then been quiet for --settle
"""
import argparse
import fcntl
import os
import pty
import re
import select
import signal
import struct
import sys
import termios
import time

ap = argparse.ArgumentParser()
ap.add_argument("--rows", type=int, default=24)
ap.add_argument("--cols", type=int, default=80)
ap.add_argument("--send-when", action="append", default=[], dest="sends")
ap.add_argument("--settle", type=float, default=0.7)
ap.add_argument("--deadline", type=float, default=600.0)
ap.add_argument("--log", default=None)
ap.add_argument("--answer-queries", action="store_true",
                help="reply to OSC 10/11 and DSR 6n the way a real terminal does")
ap.add_argument("--bg", default="2828/2d2d/3535")
ap.add_argument("--fg", default="b9b9/c0c0/cbcb")
ap.add_argument("cmd", nargs=argparse.REMAINDER)
args = ap.parse_args()

cmd = args.cmd[1:] if args.cmd and args.cmd[0] == "--" else args.cmd
if not cmd:
    sys.exit("no command")

pending = []
for spec in args.sends:
    parts = spec.split("::")
    settle = args.settle
    if len(parts) == 3:                              # SECONDS::REGEX::TEXT
        settle, pat, text = float(parts[0]), parts[1], parts[2]
    else:
        pat, text = parts[0], parts[1]
    pending.append([re.compile(pat), text, None, settle])


def dbg(msg):
    sys.stderr.write("[ptyrun %7.2fs] %s\n" % (time.time() - start, msg))
    sys.stderr.flush()


start = time.time()
pid, fd = pty.fork()
if pid == 0:
    os.execvp(cmd[0], cmd)

fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", args.rows, args.cols, 0, 0))

log = open(args.log, "wb") if args.log else None
seen = ""          # decoded output, used only for pattern matching
last = time.time()
eof = False

# A real terminal answers these; script(1) and asciinema do not, so anything
# built on termenv blocks for its full 5s OSCTimeout on every invocation. The
# values reported here are the ones the finished SVG actually renders with.
QUERIES = [
    (b"\x1b]11;?\x1b\\", ("\x1b]11;rgb:%s\x1b\\" % args.bg).encode()),
    (b"\x1b]10;?\x1b\\", ("\x1b]10;rgb:%s\x1b\\" % args.fg).encode()),
    (b"\x1b[6n",         b"\x1b[1;1R"),
]
qbuf = b""
answered = 0


def answer_queries(chunk):
    """Reply to any terminal status queries contained in chunk."""
    global qbuf, answered
    qbuf += chunk
    while True:
        hit = None
        for probe, reply in QUERIES:
            i = qbuf.find(probe)
            if i >= 0 and (hit is None or i < hit[0]):
                hit = (i, len(probe), reply, probe)
        if hit is None:
            break
        os.write(fd, hit[2])
        answered += 1
        dbg("answered terminal query %r with %r" % (hit[3], hit[2]))
        qbuf = qbuf[hit[0] + hit[1]:]
    qbuf = qbuf[-64:]

while True:
    now = time.time()
    if now - start > args.deadline:
        dbg("DEADLINE reached, terminating child")
        os.kill(pid, signal.SIGTERM)
        break

    r, _, _ = select.select([fd], [], [], 0.2)
    if r:
        try:
            data = os.read(fd, 65536)
        except OSError as e:
            dbg("read error %s (child closed the pty)" % e)
            eof = True
            break
        if not data:
            dbg("EOF on pty")
            eof = True
            break
        last = time.time()
        os.write(1, data)
        if log:
            log.write(data)
            log.flush()
        if args.answer_queries:
            answer_queries(data)
        seen += data.decode("utf-8", "replace")
        seen = seen[-65536:]

    now = time.time()
    if pending:
        head = pending[0]
        if head[2] is None and head[0].search(seen):
            head[2] = now
            dbg("matched %r" % head[0].pattern)
        if head[2] is not None and now - last >= head[3]:
            dbg("typing %r" % head[1])
            os.write(fd, head[1].encode())
            pending.pop(0)
            seen = ""
            last = time.time()
    elif now - last > 30:
        dbg("no output for 30s and nothing left to type, terminating child")
        os.kill(pid, signal.SIGTERM)
        break

if not eof:
    # drain whatever the child emits on its way out
    deadline = time.time() + 10
    while time.time() < deadline:
        r, _, _ = select.select([fd], [], [], 0.2)
        if not r:
            continue
        try:
            data = os.read(fd, 65536)
        except OSError:
            break
        if not data:
            break
        os.write(1, data)
        if log:
            log.write(data)
            log.flush()

os.close(fd)
if log:
    log.close()
_, status = os.waitpid(pid, 0)
dbg("child exited, status %d, pending sends left: %d, queries answered: %d"
    % (status, len(pending), answered))
sys.exit(os.waitstatus_to_exitcode(status))
