p = "scripts/make-integrity-guard.py"
s = open(p).read()
old = "four `" + "\\" * 2 + "`-continued"
new = "four `" + "\\" + "`-continued"
assert s.count(old) == 1, s.count(old)
open(p, "w").write(s.replace(old, new))
