#!/usr/bin/env python3
"""Code mutations for the test-stability slice (sentinel S-0016 / S-0001), one per id. Applied in
place by docs/evidence/hardening-b1/mutate.sh, which aborts if the file's sha256 did not move and
verifies the byte-identical restore. Each needle must occur exactly once."""
import sys

MUTATIONS = {
    # The fixtures package's TestMain back to main's shape: no testtmp root.
    "T1": [("internal/fixtures/fixtures_test.go", '\tos.Exit(testtmp.Run(m, "fixtures"))\n',
            '\t_ = testtmp.Run\n\tos.Exit(m.Run())\n')],
    # The integration package's TestMain without the testtmp root.
    "T2": [("internal/integration/main_test.go", '\tos.Exit(testtmp.Run(m, "integration"))\n',
            '\t_ = testtmp.Run\n\tos.Exit(m.Run())\n')],
    # Sweep treats every owner as alive: a killed run's root is never removed.
    "T3": [("internal/testtmp/testtmp.go", "\t\tif err != nil || pid <= 0 || alive(pid) {\n",
            "\t\tif err != nil || pid <= 0 || alive(pid) || true {\n")],
    # alive() treats EPERM as dead: another user's live process's root would be swept.
    # (`_ = errors.Is` keeps the file compiling: its red is a TEST failure, not a build failure.)
    "T4": [("internal/testtmp/testtmp.go", "\treturn err == nil || !errors.Is(err, syscall.ESRCH)\n",
            "\t_ = errors.Is\n\treturn err == nil\n")],
}

for path, old, new in MUTATIONS[sys.argv[1]]:
    s = open(path).read()
    assert s.count(old) == 1, (sys.argv[1], old, s.count(old))
    open(path, "w").write(s.replace(old, new))
