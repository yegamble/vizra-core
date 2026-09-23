"""Re-run the PR#9 verifier's ENTIRE evasion table against the redesigned control.

For each row: substitute it into a COPY of the real .github/ tree, assert the
digest moved, run the guard, and record RED (by which rule) or GREEN.
Nothing in the repository is modified: every mutation is applied to a copy.
"""
import hashlib, io, os, re, shutil, subprocess, sys, tempfile, yaml

# The repository root: this file lives at docs/evidence/hardening-b1/.
CORE = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
os.chdir(CORE)

ROWS = [
    # (id, description, kind, payload)
    ("B1",  "run: make -i ci",                                  "run",  "make -i ci"),
    ("B2",  "run: make SHELL=/usr/bin/true ci",                 "run",  "make SHELL=/usr/bin/true ci"),
    ("B3",  "run: make ci SHELL=/usr/bin/true (after target)",  "run",  "make ci SHELL=/usr/bin/true"),
    ("B4",  "run: make --ign ci (long abbreviation)",           "run",  "make --ign ci"),
    ("B5",  "run: make -srik ci (short cluster)",               "run",  "make -srik ci"),
    ("B6",  'run: bash -c "make -i ci"',                        "run",  'bash -c "make -i ci"'),
    ("B7",  "run: env MAKEFLAGS=-i make ci",                    "run",  "env MAKEFLAGS=-i make ci"),
    ("B8",  "step env: MAKEFLAGS: -i",                          "key",  ("env", {"MAKEFLAGS": "-i"})),
    ("B9",  "run: make -C build ci",                            "run",  "make -C build ci"),
    ("B10", "run: go build ./... && make --keep-going ci",      "run",  "go build ./... && make --keep-going ci"),
    ("A3",  'run: eval "make -i ci"',                           "run",  'eval "make -i ci"'),
    ("A4",  "run: echo ci | xargs make -i",                     "run",  "echo ci | xargs make -i"),
    ("A5",  "run: nice make -i ci",                             "run",  "nice make -i ci"),
    ("A6",  "run: time make -i ci",                             "run",  "time make -i ci"),
    ("A8",  "run: exec make -i ci",                             "run",  "exec make -i ci"),
    ("A9",  "here-doc feeding sh",                              "run",  "sh <<'EOS'\nmake -i ci\nEOS\n"),
    ("A10", "line continuation splitting -i",                   "run",  "make \\\n  -i \\\n  ci\n"),
    ("A12", "run: make ci -i (flag after target)",              "run",  "make ci -i"),
    ("A13", "run: make --ignore-errors=yes ci",                 "run",  "make --ignore-errors=yes ci"),
    ("A14", "run: make -ki ci",                                 "run",  "make -ki ci"),
    ("A15", "run: make -j4 -i ci",                              "run",  "make -j4 -i ci"),
    ("A28", "run: make -e ci",                                  "run",  "make -e ci"),
    ("A29", "run: make -f Makefile -i ci",                      "run",  "make -f Makefile -i ci"),
    ("A31", "untokenisable run: (unbalanced quote)",            "run",  'make ci "unterminated'),
    ("A32", "run: make -E bogus -i ci",                         "run",  "make -E bogus -i ci"),
    ("A34", "run: echo $(make -i ci)",                          "run",  "echo $(make -i ci)"),
    ("A23", "job continue-on-error: ${{ true }}",               "job",  ("continue-on-error", "${{ true }}")),
    ("A7",  "run: sudo -E make ci  (does NOT neuter)",          "run",  "sudo -E make ci"),
    ("A11", "run: make -- -i ci   (does NOT neuter)",           "run",  "make -- -i ci"),
    # ---- THE THIRTEEN that were GREEN and UNLISTED ----
    ("A16", "run: make -j -i ci",                               "run",  "make -j -i ci"),
    ("A17", "run: make -l -i ci",                               "run",  "make -l -i ci"),
    ("A18", "export MAKEFLAGS=-i above make ci",                "run",  "export MAKEFLAGS=-i\nmake ci\n"),
    ("A18b","export GNUMAKEFLAGS=-i above make ci",             "run",  "export GNUMAKEFLAGS=-i\nmake ci\n"),
    ("A19", "$GITHUB_ENV write by an earlier step",             "insert", 'echo \'MAKEFLAGS=-i\' >> "$GITHUB_ENV"'),
    ("A19b","$GITHUB_PATH write by an earlier step",            "insert", 'echo /tmp/sh >> "$GITHUB_PATH"'),
    ("A20", "step shell: bash -c '{0} || true'",                "key",  ("shell", "bash -c '{0} || true'")),
    ("A21", "step working-directory: build/other",              "key",  ("working-directory", "build/other")),
    ("A22", "step if: always() && false",                       "key",  ("if", "always() && false")),
    ("A24", "make() { :; } then make ci",                       "run",  "make() { :; }\nmake ci\n"),
    ("A25", "PATH shadow: stub make earlier on PATH",           "run",  "PATH=/tmp/sh:$PATH make ci"),
    ("A1",  "M=make then $M -i ci",                             "run",  "M=make\n$M -i ci\n"),
    ("A2",  "${MAKE:-make} -i ci",                              "run",  "${MAKE:-make} -i ci"),
    ("A33", "echo `make -i ci` (backticks)",                    "run",  "echo `make -i ci`"),
    ("A26", "wrapper script ./scripts/wrap.sh running make -i ci", "run", "./scripts/wrap.sh"),
    ("A27", "alias make='make -i' (did not neuter)",            "run",  "shopt -s expand_aliases\nalias make='make -i'\nmake ci\n"),
    # The honest counter-rows: things the control does NOT refuse, which the
    # residual list names. A wrapper script ADDED beside the pinned make step
    # (rather than replacing it) still runs; so does any other arbitrary step.
    ("R1",  "wrapper script ADDED early in the job (not replacing)",  "early", "./scripts/wrap.sh"),
    ("R2",  "an arbitrary EARLY step rewrites a test file on disk",    "early", "echo 'package obs' > internal/obs/obs_test.go"),
    ("R3",  "an arbitrary EARLY step writes MAKEFLAGS to $GITHUB_ENV", "early", 'echo \'MAKEFLAGS=-i\' >> "$GITHUB_ENV"'),
]

def sha(p): return hashlib.sha256(io.open(p,'rb').read()).hexdigest()

def apply(tmp, kind, payload):
    p = os.path.join(tmp, "workflows", "build-test.yml")
    text = io.open(p).read()
    before = sha(p)
    target = "      - name: make ci\n        run: make ci\n"
    assert text.count(target) == 1, "anchor text not unique"
    if kind == "run":
        body = payload if payload.endswith("\n") else payload + "\n"
        if "\n" in body.rstrip("\n"):
            new = "      - name: make ci\n        run: |\n" + "".join(
                "          " + l if l.strip() else "\n" for l in body.splitlines(keepends=True))
        else:
            new = f"      - name: make ci\n        run: {body.rstrip()}\n"
    elif kind == "key":
        k, v = payload
        rendered = (f"        {k}:\n" + "".join(f"          {a}: {b}\n" for a, b in v.items())
                    if isinstance(v, dict) else f"        {k}: {v}\n")
        new = "      - name: make ci\n" + rendered + "        run: make ci\n"
    elif kind == "early":
        # At the TOP of the job's steps — genuinely earlier, so adjacency is
        # untouched. This is the residual the docs name.
        marker = "    steps:\n      - name: Check out\n"
        assert text.count(marker) == 1
        text = text.replace(marker, marker.rstrip("\n") + "\n" + "      - name: early\n        run: "
                            + payload + "\n", 1)
        io.open(p, "w").write(text)
        return before, sha(p)
    elif kind == "insert":
        new = ("      - name: prepare\n        run: " + payload + "\n") + target
    elif kind == "job":
        k, v = payload
        text = text.replace("  build-test:\n    name: build-test\n",
                            f"  build-test:\n    name: build-test\n    {k}: {v}\n", 1)
        io.open(p, "w").write(text)
        return before, sha(p)
    io.open(p, "w").write(text.replace(target, new, 1))
    return before, sha(p)

results = []
for rid, desc, kind, payload in ROWS:
    tmp = tempfile.mkdtemp(prefix="vzb2ev.")
    try:
        shutil.copytree(".github/workflows", os.path.join(tmp, "workflows"))
        shutil.copy(".github/required-checks.txt", os.path.join(tmp, "required-checks.txt"))
        before, after = apply(tmp, kind, payload)
        if before == after:
            results.append((rid, desc, "ABORT", "mutation did not apply")); continue
        r = subprocess.run(["python3", "scripts/ci-required-guard.py",
                            "--workflows", os.path.join(tmp, "workflows"),
                            "--manifest", os.path.join(tmp, "required-checks.txt"),
                            "--skip-makefile"], capture_output=True, text=True)
        out = r.stdout + r.stderr
        if r.returncode == 0:
            results.append((rid, desc, "GREEN", "—")); continue
        rules = []
        if "not byte-equal to any entry" in out: rules.append("8b pin")
        if re.search(r"make step .* carries \[", out): rules.append("8b keys")
        if "not IMMEDIATELY preceded" in out: rules.append("8b adjacency")
        if "does not run required invocation" in out: rules.append("8c present")
        if "carries continue-on-error" in out: rules.append("4 coe")
        if "anchor step before" in out: rules.append("8 anchor keys")
        if "with NO make-integrity-guard" in out: rules.append("8 anchor missing")
        if "job-level env sets" in out or "workflow-level env sets" in out: rules.append("8b env")
        if "cannot be tokenised" in out: rules.append("8b untokenisable")
        if "direct test step" in out or "runs the unit suite" in out: rules.append("9 direct")
        results.append((rid, desc, "RED", ", ".join(rules) or "other"))
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

w = max(len(d) for _, d, _, _ in results)
print(f"| {'#':5s} | {'evasion':{w}s} | result | refused by |")
print(f"|{'-'*7}|{'-'*(w+2)}|--------|------------|")
for rid, desc, res, rule in results:
    print(f"| {rid:5s} | {desc:{w}s} | {res:6s} | {rule} |")
print()
print("GREEN rows:", [r[0] for r in results if r[2] == "GREEN"])
print("ABORT rows:", [r[0] for r in results if r[2] == "ABORT"])
