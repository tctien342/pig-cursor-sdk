"""Summarise host tool-call parking from pig-cursor-sdk's agent.log.

Reads a log slice (one run's worth) and prints, per call id, how many times the
call was parked and how many times its result was delivered. Exactly-once means
one of each.

  python3 dev/toolcalls.py ~/.pig/agent/pig-cursor-sdk/agent.log
  ./dev/run-edge.sh                       # uses it on one run's slice
"""
import collections
import re
import sys

# slog writes key=value, quoting only when the value needs it.
FIELD = re.compile(r'(?:^|\s)(\w+)=(?:"((?:[^"\\]|\\.)*)"|(\S+))')


def fields(line):
    out = {}
    for key, quoted, bare in FIELD.findall(line):
        out[key] = quoted if quoted else bare
    return out


def events(path):
    for line in open(path):
        if "host tool call parked" in line:
            kind = "park"
        elif "host tool result delivered" in line:
            kind = "deliver"
        else:
            continue
        f = fields(line)
        if f.get("id"):
            yield kind, f.get("tool", "?"), f["id"]


path = sys.argv[1]
parked = collections.Counter()
delivered = collections.Counter()
tools = {}
for kind, tool, cid in events(path):
    tools[cid] = tool
    (parked if kind == "park" else delivered)[cid] += 1

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
