#!/usr/bin/env python3
"""The code mutations for slice B5b, one per id. Applied in place by
docs/evidence/hardening-b1/mutate.sh, which aborts if the file's sha256 did not
move and verifies the byte-identical restore. Each needle must occur exactly once."""
import sys

GUARD = "scripts/make-integrity-guard.py"
MUTATIONS = {
    # R2-F1: the pre-make refusals, one name each.
    "C15": [(GUARD, '    ".IGNORE": ', '    ".IGNORE-disabled-by-C15": ')],
    "C15b": [(GUARD, '    ".IGNORE": ', '    ".IGNORE-disabled-by-C15b": '),
             (GUARD, '    if variables.get("__IGNORE__"):', '    if False:')],
    "C16": [(GUARD, '    ".DEFAULT": ', '    ".DEFAULT-disabled-by-C16": ')],
    "C17": [(GUARD, '    ".EXTRA_PREREQS": ', '    ".EXTRA_PREREQS-disabled-by-C17": ')],
    # R2-F2: the closure reaches only explicit, phony rules.
    "C18": [(GUARD, "            if _PATTERN_RULE_RE.match(line) and not _TARGET_SPECIFIC_RE.match(line):",
             "            if False:")],
    "C19": [(GUARD, "    if not_phony:", "    if False:")],
    "C20": [(GUARD, "                undefined.append(t)\n                g.fail(",
             "                undefined.append(t)\n                if False: g.fail(")],
    "C21": [(GUARD, "        if _SUBMAKE_RE.search(body):", "        if False:")],
}

for path, old, new in MUTATIONS[sys.argv[1]]:
    s = open(path).read()
    assert s.count(old) == 1, (sys.argv[1], old, s.count(old))
    open(path, "w").write(s.replace(old, new))
