#!/usr/bin/env python3
"""spawn-recorder — run make-integrity-guard.py IN-PROCESS and record every process it starts.

Test harness only (driven by scripts/scripts_test.go). It answers two questions
the guard's own output cannot be trusted to answer about itself:

  * Was make invoked? `subprocess.Popen` is replaced by a subclass that records
    argv and the environment BEFORE starting the real process, so every child
    the guard starts through the subprocess module is seen — `subprocess.run`
    looks `Popen` up in the module at call time. The guard starts processes no
    other way; TestTheSpawnRecorderSeesEveryWayTheGuardStartsAProcess asserts
    that of its source.
  * What environment did each child get? The recorded environment is the one
    handed to execve: the `env=` argument, or os.environ when none was given.

Usage:
    spawn-recorder.py RECORD.json [guard args...]
        runs the guard's main() with those args; writes the record; exits with
        the guard's exit code.
    spawn-recorder.py --child-env RECORD.json
        starts one real child (`env`) with the guard's clean_env() and records
        the variable names that child PRINTED — what a subprocess can see.
"""

from __future__ import annotations

import importlib.util
import json
import os
import subprocess
import sys
from pathlib import Path

GUARD = Path(__file__).resolve().parent.parent / "make-integrity-guard.py"


def load_guard():
    spec = importlib.util.spec_from_file_location("make_integrity_guard", GUARD)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def main() -> int:
    if len(sys.argv) >= 3 and sys.argv[1] == "--child-env":
        record = sys.argv[2]
        guard = load_guard()
        proc = subprocess.run(["env"], env=guard.clean_env(), capture_output=True, text=True, check=True)
        names = sorted({line.split("=", 1)[0] for line in proc.stdout.splitlines() if "=" in line})
        values = [line.split("=", 1)[1] for line in proc.stdout.splitlines() if "=" in line]
        Path(record).write_text(json.dumps({"child_env_names": names, "child_env_values": values}))
        return 0

    record, guard_args = sys.argv[1], sys.argv[2:]
    calls = []
    real_popen = subprocess.Popen

    class RecordingPopen(real_popen):  # type: ignore[misc, valid-type]
        def __init__(self, args, *a, **kw):
            env = kw.get("env")
            env = dict(os.environ if env is None else env)
            argv = [args] if isinstance(args, (str, bytes)) else [str(x) for x in args]
            calls.append({"argv": argv, "env_names": sorted(env), "env_values": sorted(env.values())})
            super().__init__(args, *a, **kw)

    subprocess.Popen = RecordingPopen  # type: ignore[misc]
    guard = load_guard()
    sys.argv = [str(GUARD), *guard_args]
    code = 1
    try:
        code = guard.main()
    finally:
        Path(record).write_text(json.dumps({"calls": calls, "exit": code,
                                            "make_invocations_counted": guard.MAKE_INVOCATIONS}))
    return code


if __name__ == "__main__":
    raise SystemExit(main())
