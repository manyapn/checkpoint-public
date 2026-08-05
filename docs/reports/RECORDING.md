# Terminal recording of the demo

This is the provenance record for the recording committed at `demo/demo.svg`.
It says what was run, on what, how the recording was made, and how it was
checked. Everything below came out of a run that actually happened on the
machine described here.

## What is committed

| File | What it is |
|---|---|
| `demo/demo.svg` | Animated SVG of the run, 149754 bytes. Renders inline in a GitHub README with no third party and no external requests. |
| `demo/demo.cast` | The asciicast v2 recording the SVG was rendered from, 14577 bytes. This is the raw capture. |
| `docs/reports/record_demo.py` | The harness that drove the recording. |
| `docs/reports/verify_recording.py` | The check that the SVG plays back the cast. |

```
sha256  a0896272fe76a695fccbc137536084832b66b84053c8ce4052e9b10e30f0e402  bin/checkpoint (the binary that ran)
sha256  2335c4e3cf3c3bbfa52456124bb55a299f346f54a3dfb7f52c82e56595794aae  demo/demo.cast
sha256  4009988fcb1df7d50bf7154c1f5d73547fcd8e14e97de805e4d1aac72b372039  demo/demo.svg
```

To put it in the README:

```markdown
![checkpoint demo](demo/demo.svg)
```

## What the recording shows

One run of `demo/run_demo.sh --ext4`, the self-asserting 8 step demo. It was not
edited, cut or retimed. All 8 steps passed:

```
STEP 1 OK  (227 ms)   the session history is open, and its first checkpoint holds the whole project
STEP 2 OK  (1601 ms)  the turn ended and the history committed itself: exactly one durable checkpoint
STEP 3 OK  (9 ms)     one checkpoint, two authors: the file you wrote is recorded as yours
STEP 4 OK  (16 ms)    the session log is navigable, and honest about the one write it does not hold
STEP 5 OK  (1637 ms)  the turn was reverted per file: the agent's undone, yours kept, shared files refused
STEP 6 OK  (432 ms)   the working tree was destroyed and rebuilt byte-exact from a history that outlived it
STEP 7 OK  (22 ms)    a file that lived only between checkpoints came back byte-exact
STEP 8 OK  (144 ms)   recording stopped cleanly, and the session history it wrote is still readable

PROVEN ON THIS MACHINE IN 7.0s
```

Steps 2 and 5 are dominated by a deliberate `sleep 1.2` inside the scripted
agent turn, which exists so the human's concurrent edit lands strictly inside
the turn. The recording runs 16.5 seconds end to end: 7.0 seconds of demo plus
the typing and a pause at the end so the closing summary can be read before the
animation loops.

The change feed is **active** in this recording. That is the full fidelity path,
where deletions carry an author and step 5 reverts the agent's delete
automatically. It is available because the demo put its sandbox on ext4 (see
below), not because of anything special about the machine.

## Environment

| | |
|---|---|
| Kernel | Linux 6.12.76-linuxkit |
| Architecture | aarch64 |
| CPUs | 10 (`nproc`) |
| OS | Debian GNU/Linux 12 (bookworm), in a container |
| Container root filesystem | `overlay` (overlayfs) |
| Filesystem the demo ran on | ext4, on a 256 MiB loopback image the demo created itself |
| Binary commit | `f07cd88398dfc8fa0039180df33e94db6dfa5051` |
| Working tree at build time | modified, but in `README.md`, `bench/README.md` and untracked `docs/` only. `git status --porcelain -- '*.go'` was empty, so the compiled code is exactly that commit. |
| Go | go1.24.13 linux/arm64 |
| Recorder | asciinema 2.2.0 |
| Renderer | svg-term-cli 2.1.1 on node v24.18.0 |
| Terminal geometry | 100 columns x 30 rows, `TERM=xterm-256color` |
| Run as | root (the demo needs `mkfs.ext4`, `mount -o loop` and fanotify) |

Overlayfs, which is what a default container gives you, **degrades this product**:
the change feed is unavailable there and deletions arrive with no author. That
is why the recording was made with `--ext4`, which makes the demo `dd` a 256 MiB
image, `mkfs.ext4` it, `mount -o loop` it, put its sandbox inside, and unmount
and delete it on the way out. The mount and the image do not outlive the run.

The store free space warning visible in the recording (`only 223.4 MB free of
241.1 MB`) is a true reading of that 256 MiB loopback image. It is the demo
being honest about a small disk, not a problem with the run.

## How it was recorded

```sh
# 1. build the binary the demo will use
go build -o bin/checkpoint ./cmd/checkpoint

# 2. record, as root
PS1='# ' TERM=xterm-256color python3 docs/reports/record_demo.py \
    --rows 30 --cols 100 --settle 1.5 --answer-queries --log /tmp/demo.raw \
    --send-when '# $::uname -srm; nproc
' \
    --send-when '# $::./demo/run_demo.sh --ext4
' \
    --send-when '6.0::PROVEN ON THIS MACHINE|DEMO FAILED::exit
' \
    -- asciinema rec --overwrite -c 'bash --norc --noprofile -i' demo/demo.cast

# 3. render to an animated SVG
svg-term --in demo/demo.cast --out demo/demo.svg \
    --window --width 100 --height 30 --padding 14

# 4. check the SVG really plays back the cast
python3 docs/reports/verify_recording.py
```

`record_demo.py` exists because asciinema 2.2.0 takes its geometry from the
terminal it is started on, and the session that made this recording had no
controlling terminal. The harness opens a pty of a known size, runs a real
interactive `bash` on it, and types the two commands. The commands are typed
into a real shell and the prompt in the recording is that shell's real prompt.

### The one thing the harness does beyond typing

It answers two terminal status queries, because a real terminal answers them and
`script(1)` and `asciinema` do not:

* `ESC ] 11 ; ? ST` (what is your background colour) is answered with
  `rgb:2828/2d2d/3535`, which is the background svg-term actually renders.
* `ESC [ 6 n` (where is the cursor) is answered with `ESC [ 1;1R`.

This matters a lot, and it is worth stating plainly. `checkpoint` links
`github.com/charmbracelet/lipgloss`, which pulls in
`github.com/muesli/termenv v0.15.2`. `termenv` asks the terminal for its
background colour to decide light mode from dark mode, and
`termenv_unix.go` sets `OSCTimeout = 5 * time.Second`. If nothing answers, every
affected command blocks for a full five seconds. Measured on this machine:

```
checkpoint doctor, stdout is a pty, nothing answering :  5.058 s
checkpoint doctor, stdout is a pipe                   :  0.013 s
```

Four commands in the demo are affected (`doctor`, `restore`, `recover`,
`protect --stop`), so an unanswered recording turns a 7 second demo into a 26 to
28 second one, all of it spent waiting on a query no one replied to. A real
terminal answers in about a millisecond and never shows this. Answering the
query is therefore what makes the recording faithful rather than what makes it
flattering: without it the recording would show a stall that no user would ever
see.

**This is a real bug in the product, not only in the recording.** Any terminal
that does not answer OSC 11 costs five seconds per affected command. Worth
fixing at the source rather than papering over in the harness.

## How the recording was verified

`docs/reports/verify_recording.py` checks the SVG against the cast without a
browser. svg-term lays a recording out as a horizontal strip of full screen
frames and steps through it with a CSS keyframe animation, so the frames can be
read straight out of the file: parse the keyframes to learn which frame is on
screen when, rebuild that frame's character grid from the SVG, and compare it
against an independent terminal emulation of the `.cast` (`pyte`).

Result on the committed files:

```
cast : 100x30, 85 output events, 16.494s
svg  : 86 frames, 34 keyframes, animation 16.494s

E. animation length equals recording length          16.494s == 16.494s  OK
   transcript: 179 non-blank lines of real terminal output
A. pre-scroll frames matching pyte row-for-row       10 frames  OK
B. frames that are a contiguous run of the transcript 34/34  OK
C. runs advance monotonically through the transcript  OK
D. final frame reaches the end of the transcript     lines 150-178 of 179  OK
   (svg-term's last frame omits the final write: 'exit')

VERIFIED: the SVG plays back the recording
```

Checks B, C and D compare content and ordering rather than absolute row numbers.
That is deliberate: the demo prints lines longer than 100 columns, and `pyte`
wraps at the right margin one row earlier than xterm.js does, which shifts whole
screens down by one row without changing a single character. Check A demands
exact row for row equality, but only for the frames shown before the first
full width row appears, where the two emulators cannot disagree.

Separately checked on `demo/demo.svg`:

* well formed XML, 1068 x 739.3, one `@keyframes` block;
* no `<script>`, no `<foreignObject>`, no `<image>`, no external `href`. It is
  self contained and safe to serve through GitHub's image proxy;
* the rendered text contains the real run, including `aarch64`,
  `./demo/run_demo.sh --ext4`, `STEP 1 OK` through `STEP 8 OK`,
  `Change feed: active`, `Limited protection`, `rm -rf` and
  `PROVEN ON THIS MACHINE IN 7.0s`;
* no escape sequence leaked into rendered text (no stray `]11;?`, `[1;1R` or
  `?2004`).

## Limits of this artifact

* It was not opened in a browser. There is no browser in the container that
  produced it, so "it renders in GitHub" rests on the file being a standard
  svg-term-cli animated SVG with no script and no external references, plus the
  frame by frame check above, and not on someone having looked at it. Look at it
  once before relying on it.
* One run on one machine. It is a demonstration that the demo passes, not a
  benchmark. For timings that mean something, use the benchmark harness.
* The absolute times depend on this container. The 5 second figures discussed
  above are a fixed timeout and do not vary with the machine.
