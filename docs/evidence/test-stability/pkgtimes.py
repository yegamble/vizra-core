import json, sys
for f in sys.argv[1:]:
    t = {}
    for l in open(f):
        try: e = json.loads(l)
        except Exception: continue
        if e.get("Test") is None and e.get("Action") in ("pass", "fail") and "Elapsed" in e:
            t[e["Package"].rsplit("/", 1)[-1]] = (e["Action"], e["Elapsed"])
    top = sorted(t.items(), key=lambda kv: -kv[1][1])[:4]
    print(f.rsplit("/", 1)[-1], " ".join("%s=%s:%.1fs" % (k, a, s) for k, (a, s) in top))
