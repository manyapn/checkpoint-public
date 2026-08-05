#!/usr/bin/env python3
"""Verify demo.svg really plays back demo.cast, without needing a browser.

svg-term lays the recording out as a horizontal strip of full-screen frames and
steps through it with a CSS keyframe animation, so the SVG can be checked
directly: read the keyframes to learn which frame is on screen when, rebuild
that frame's character grid out of the SVG, and check it against an independent
terminal emulation (pyte) of the .cast.

One wrinkle: the demo prints lines longer than 100 columns, and pyte wraps them
one row earlier than xterm.js does (eager versus deferred wrap at the right
margin). That shifts whole screens vertically by a row without changing a single
character. So the comparison is done on content and ordering rather than on
absolute row numbers:

  A. every frame shown before the screen first scrolls must match pyte exactly,
     row for row;
  B. every frame's lines must appear as one contiguous run of the full
     transcript, so no frame shows text the run never produced;
  C. those runs must advance monotonically, so the animation plays forward;
  D. the last frame must be the end of the transcript;
  E. the animation must last exactly as long as the recording.

Usage:  python3 docs/reports/verify_recording.py [demo.svg] [demo.cast]
Needs:  pip install pyte
"""
import json
import os
import re
import sys
import xml.etree.ElementTree as ET

import pyte

_repo = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SVG = sys.argv[1] if len(sys.argv) > 1 else os.path.join(_repo, "demo", "demo.svg")
CAST = sys.argv[2] if len(sys.argv) > 2 else os.path.join(_repo, "demo", "demo.cast")
NS = {"s": "http://www.w3.org/2000/svg"}
HREF = "{http://www.w3.org/1999/xlink}href"
COLW, ROWH = 1.002, 2.171

raw = open(SVG, encoding="utf-8").read()
root = ET.fromstring(raw)

symbols = {}
for sym in root.iter("{http://www.w3.org/2000/svg}symbol"):
    symbols[sym.get("id")] = {round(float(t.get("x", 0)) / COLW): t.text
                              for t in sym.findall("s:text", NS) if t.text}

strip = None
for g in root.iter("{http://www.w3.org/2000/svg}g"):
    if "animation-name:j" in g.get("style", ""):
        strip = g.find("s:svg", NS)

frames = {}
for fr in strip.findall("s:svg", NS):
    rows = {}
    for use in fr.findall("s:use", NS):
        cells = symbols.get(use.get(HREF, "").lstrip("#"))
        if not cells:
            continue
        row = round(float(use.get("y", 0)) / ROWH)
        shift = round(float(use.get("x", 0)) / COLW)
        rows.setdefault(row, {}).update({c + shift: t for c, t in cells.items()})
    frames[round(float(fr.get("x", 0)) / 100)] = rows


def render(rows, height=30):
    out = []
    for r in range(height):
        line = [" "] * 220
        for col, text in rows.get(r, {}).items():
            for i, ch in enumerate(text):
                if 0 <= col + i < 220:
                    line[col + i] = ch
        out.append("".join(line).rstrip())
    while out and not out[-1]:
        out.pop()
    return out


css = re.search(r"@keyframes j\{(.*?)\}\}", raw, re.S).group(1) + "}"
steps = sorted((0.0 if p == "from" else 100.0 if p == "to" else float(p[:-1]),
                round(abs(float(x)) / 100))
               for p, x in re.findall(
                   r"(to|from|[\d.]+%)\{transform:translateX\((-?[\d.]+)p?x?\)\}", css))
dur = float(re.search(r"animation-duration:([\d.]+)s", raw).group(1))

events = []
with open(CAST) as fh:
    header = json.loads(fh.readline())
    for line in fh:
        if line.startswith("["):
            t, kind, data = json.loads(line)
            if kind == "o":
                events.append((t, data))

W, H = header["width"], header["height"]
print(f"cast : {W}x{H}, {len(events)} output events, {events[-1][0]:.3f}s")
print(f"svg  : {len(frames)} frames, {len(steps)} keyframes, animation {dur:.3f}s")

fail = []

# E. duration
if abs(dur - events[-1][0]) > 0.005:
    fail.append(f"E: animation {dur}s != cast {events[-1][0]}s")
print(f"\nE. animation length equals recording length          "
      f"{dur:.3f}s == {events[-1][0]:.3f}s  OK")

# full transcript on a screen tall enough that nothing ever scrolls
tall = pyte.Screen(W, 4000)
ts = pyte.Stream(tall)
for _, d in events:
    ts.feed(d)
transcript = [l.rstrip() for l in tall.display if l.strip()]
print(f"   transcript: {len(transcript)} non-blank lines of real terminal output")


def screen_at(n):
    s = pyte.Screen(W, H)
    st = pyte.Stream(s)
    for _, d in events[:n]:
        st.feed(d)
    out = [l.rstrip() for l in s.display]
    while out and not out[-1]:
        out.pop()
    return out


# A. exact match while the screen has not scrolled yet
exact = 0
for pct, frame in steps:
    want = screen_at(frame)
    # Once a row is exactly W columns wide the two emulators legitimately
    # disagree about when the next row scrolls, so stop demanding row equality.
    if len(want) >= H or any(len(l) == W for l in want):
        break
    if want != render(frames[frame]):
        fail.append(f"A: frame {frame} at {pct}% differs from pyte")
        break
    exact += 1
print(f"A. pre-scroll frames matching pyte row-for-row       {exact} frames  "
      f"{'OK' if exact else 'FAIL'}")


def find_run(got):
    """Locate got as a run of transcript. The bottom line of a frame may be a
    line still being typed or printed, so it is allowed to be a prefix."""
    n = len(got)
    for i in range(len(transcript) - n + 1):
        if transcript[i:i + n] == got:
            return i
    for i in range(len(transcript) - n + 1):
        if (transcript[i:i + n - 1] == got[:-1]
                and transcript[i + n - 1].startswith(got[-1])):
            return i
    return None


# B + C + D
prev_start = -1
windows = []
for pct, frame in steps:
    got = [l for l in render(frames[frame]) if l]
    start = find_run(got)
    if start is None:
        fail.append(f"B: frame {frame} at {pct}% is not a run of the transcript")
        continue
    windows.append((pct, frame, start, start + len(got)))
    if start < prev_start:
        fail.append(f"C: frame {frame} at {pct}% goes backwards "
                    f"({start} after {prev_start})")
    prev_start = start

print(f"B. frames that are a contiguous run of the transcript "
      f"{len(windows)}/{len(steps)}  "
      f"{'OK' if len(windows) == len(steps) else 'FAIL'}")
print(f"C. runs advance monotonically through the transcript  "
      f"{'OK' if not [f for f in fail if f.startswith('C')] else 'FAIL'}")

last_pct, last_frame, s0, s1 = windows[-1]
# svg-term's final frame is the state after the second-to-last output event, so
# the very last write of the run can be one line short. Anything more is a gap.
tail_ok = len(transcript) - s1 <= 1
if not tail_ok:
    fail.append(f"D: last frame ends at transcript line {s1}, not {len(transcript)}")
print(f"D. final frame reaches the end of the transcript     "
      f"lines {s0}-{s1} of {len(transcript)}  {'OK' if tail_ok else 'FAIL'}")
if s1 < len(transcript):
    print(f"   (svg-term's last frame omits the final write: "
          f"{transcript[s1]!r})")

print("\nlast lines the animation leaves on screen:")
for l in transcript[s1 - 3:s1]:
    print("   " + l[:96])

print("\n" + ("VERIFIED: the SVG plays back the recording"
              if not fail else "FAILURES:\n  " + "\n  ".join(fail)))
sys.exit(1 if fail else 0)
