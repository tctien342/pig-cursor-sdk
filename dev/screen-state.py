#!/usr/bin/env python3
"""Replay a raw PTY capture through a terminal emulator and report the SCREEN.

Stripping ANSI and grepping proves bytes were emitted. It does not prove anything
was visible: a row can be painted and then erased, overwritten, or scrolled away.
This feeds the capture to pyte, which maintains a real screen grid, so the answer
comes from what a user would have seen.

    dev/screen-state.py PATTERN [file ...]     # default: /tmp/pi-bar-raw.log

Prints the frame count, whether PATTERN is on the final screen or on any frame,
and the rows around the last hit.
"""
import re
import sys

import pyte

DEFAULT = "/tmp/pi-bar-raw.log"
ROWS, COLS = 40, 110


def frames(path, rows=ROWS, cols=COLS):
    """Yield (frame_no, screen) after every chunk of output worth a look."""
    screen = pyte.Screen(cols, rows)
    screen.set_mode(pyte.modes.LNM)
    stream = pyte.Stream(screen)
    data = open(path, "rb").read().decode("utf-8", "replace")
    # Split on CSI block boundaries so each yield is a settled screen state rather
    # than a half-applied sequence.
    chunks = re.split(r"(?=\x1b\[2J|\x1b\[H)", data) or [data]
    for i, chunk in enumerate(chunks):
        if chunk:
            stream.feed(chunk)
        yield i, screen


def grid(screen):
    return ["".join(screen.buffer[y][x].data for x in range(screen.columns)).rstrip()
            for y in range(screen.lines)]


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    pattern = sys.argv[1]
    paths = sys.argv[2:] or [DEFAULT]
    rx = re.compile(pattern)
    exit_code = 1
    for path in paths:
        last = []
        on_any = False
        count = 0
        for _, screen in frames(path):
            count += 1
            last = grid(screen)
            if any(rx.search(l) for l in last):
                on_any = True
        final_hit = [i for i, l in enumerate(last) if rx.search(l)]
        print(f"== {path}: {count} screen states replayed at {COLS}x{ROWS}")
        print(f"   on final screen: {bool(final_hit)}   seen in any frame: {on_any}")
        for i in final_hit[:3]:
            lo, hi = max(0, i - 2), min(len(last), i + 3)
            for j in range(lo, hi):
                mark = ">" if j == i else " "
                print(f"   {mark} {last[j][:112]}")
        if final_hit:
            exit_code = 0
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
