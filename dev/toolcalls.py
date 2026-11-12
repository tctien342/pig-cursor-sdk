#!/usr/bin/env python3
"""Summarise host tool-call parking from pig-cursor-sdk's agent.log.

Reads a log slice (one run's worth) and prints, per call id, how many times the
call was parked and how many times its result was delivered. Exactly-once means
one of each.

  python3 dev/toolcalls.py ~/.pig/agent/pig-cursor-sdk/agent.log
"""
import collections
import re
import sys

PARK = re.compile(r'msg="host tool call parked".*?tool=(\S+) .*?id=\\?"([^"\\]+)')
DELIVER = re.compile(r'msg="host tool result delivered".*?tool=(\S+) .*?id=\\?"([^"\\]+)')

path = sys.argv[1]
parked = collections.Counter()
delivered = collections.Counter()
tools = {}
for line in open(path):
    m = PARK.search(line)
    if m:
        tools[m.group(2)] = m.group(1)
        parked[m.group(2)] += 1
        continue
    m = DELIVER.search(line)
    if m:
        delivered[m.group(2)] += 1

if not parked and not delivered:
    print("     (no host tool calls in this slice)")
    raise SystemExit(0)

print(f"     {'tool':<10}{'call id tail':<18}{'parked':>7}{'delivered':>11}")
for cid in sorted(set(parked) | set(delivered)):
    tail = cid.split("\n")[-1][-14:]  # ids carry a newline; keep the tail readable
    print(f"     {tools.get(cid, '?'):<10}{tail:<18}{parked[cid]:>7}{delivered[cid]:>11}")

bad = [c for c in parked if parked[c] != 1 or delivered[c] != 1]
extra = [c for c in delivered if delivered[c] > 1]
if not bad and not extra:
    print(f"     RESULT: PASS — {len(parked)} host call(s), each parked once and answered once")
    raise SystemExit(0)
print(f"     RESULT: FAIL — {len(bad)} call(s) not parked-and-answered exactly once: "
      + ", ".join(c.split('\n')[-1][-14:] for c in bad))
raise SystemExit(1)
