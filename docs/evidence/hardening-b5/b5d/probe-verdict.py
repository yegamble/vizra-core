#!/usr/bin/env python3
"""Judge one-reader-probe.py output (JSON file argv[1]); exit 1 on any problem. Used by demo.sh."""
import json, sys
d = json.load(open(sys.argv[1]))
bad = 0
for p in d["probes"]:
    ok = p["clean_ok"] and p["poisoned_ok"]
    bad += not ok
    print(("ok   " if ok else "BAD  ") + "POISON " + p["name"])
for s in d["source"]:
    bad += 1
    print("BAD  SOURCE " + s[:200])
for i in d["identity"]:
    ok = i["distinct_sequences"] == 1
    bad += not ok
    print(("ok   " if ok else "BAD  ") + "IDENTITY text %s: %d distinct sequence(s)" % (i["text"], i["distinct_sequences"]))
print("probe: %d probe(s), %d named non-makefile read(s), %d problem(s)" % (len(d["probes"]), d["named_reads"], bad))
sys.exit(1 if bad else 0)
