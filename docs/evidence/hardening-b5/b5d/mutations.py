#!/usr/bin/env python3
"""The code mutations for queue 2p (core B5d), one per id. Applied in place by
docs/evidence/hardening-b1/mutate.sh, which aborts if the file's sha256 did not
move and verifies the byte-identical restore. Each needle must occur exactly once."""
import sys

PIN = "scripts/makefile_pin.py"
GUARD = "scripts/make-integrity-guard.py"
CRG = "scripts/ci-required-guard.py"
MUTATIONS = {
    # The grammar removed at its source: verify_pin computes no grammar problems (both the anchor and check 11).
    "C33": [(PIN, '        r.grammar.extend(Problem("grammar", rel, msg) for msg in grammar_problems(rel, texts[rel]))',
             "        pass")],
    # Check 11 no longer reports the grammar problems verify_pin computed.
    "C34": [(CRG, "    for p in r.grammar:\n        g.fail(p.message, \"The workflow anchor",
             "    for p in []:\n        g.fail(p.message, \"The workflow anchor")],
    # The anchor no longer reports the grammar problems verify_pin computed (it still skips the ok lines).
    "C35": [(GUARD, "    for p in r.grammar:\n        g.fail(p.message, \"make is only run",
             "    for p in []:\n        g.fail(p.message, \"make is only run")],
    # IDENTITY: the one reader's cache removed, so readers of one text get different sequences.
    "C36": [(PIN, "@functools.lru_cache(maxsize=64)\ndef makefile_lines(", "def makefile_lines(")],
    # A SECOND reader: ci-required-guard's env-name reading splits the file itself.
    "C37": [(CRG, "        for rec in lines:\n            m = _ENV_TAKEN_ASSIGN_RE.match(rec.raw)",
             "        for raw in makefile.read_text().split(\"\\n\"):\n            m = _ENV_TAKEN_ASSIGN_RE.match(raw)")],
    # Core's narrowing: a backslash-continued rule or .PHONY line accepted again.
    "C38": [(PIN, "        if (_G_PHONY_RE.match(body) or _G_RULE_RE.match(body)) and len(rec.segments) > 1:",
             "        if False:")],
    # C39 and C40 are C33 and C37 read through the Go tests.
    "C39": None,
    "C40": None,
    "C41": None,
}
MUTATIONS["C39"] = MUTATIONS["C33"]
MUTATIONS["C40"] = MUTATIONS["C37"]
MUTATIONS["C41"] = MUTATIONS["C33"]

for path, old, new in MUTATIONS[sys.argv[1]]:
    s = open(path).read()
    assert s.count(old) == 1, (sys.argv[1], old, s.count(old))
    open(path, "w").write(s.replace(old, new))
