#!/usr/bin/env python3
"""Turn a raw PTY capture into something a human can read on stdout.

pig paints with cursor movement and box drawing, so a raw capture is mostly
escape sequences. This strips the sequences, collapses carriage returns, and
squashes runs of blank lines. It cannot rebuild the screen — overlapping paints
and cursor-relative writes collapse into one stream — which is enough to read
what a line said and to grep for a glyph.
"""
import re
import sys

data = sys.stdin.buffer.read().decode("utf-8", "replace")
# CSI sequences (colours, cursor movement, erase) and OSC sequences (titles, links).
data = re.sub(r"\x1b\[[0-9;?]*[A-Za-z@-~]", "", data)
data = re.sub(r"\x1b\][^\x07\x1b]*(\x07|\x1b\\)", "", data)
data = re.sub(r"\x1b[()][A-Z0-9]", "", data)
data = re.sub(r"\x1b[^[\]()][A-Za-z]?", "", data)
data = data.replace("\x07", "").replace("\x08", "")
data = data.replace("\r\n", "\n").replace("\r", "\n")

out, blank = [], 0
for line in (l.rstrip() for l in data.split("\n")):
    if not line.strip():
        blank += 1
        if blank > 1:
            continue
    else:
        blank = 0
    out.append(line)
sys.stdout.write("\n".join(out) + "\n")
