"""makefile_pin — the ONE reader of .github/pinned-makefiles.yml.

Imported by scripts/make-integrity-guard.py (the anchor, which enforces the pin
at runtime and refuses to invoke make on anything it rejects) and by
scripts/ci-required-guard.py check 11 (the static half). Both call verify_pin(),
so the two can never disagree about what is refused (sweep B5 fix round 1,
PR#10 VERIFY FINDING 3: check 11 used to accept a GNUmakefile beside the
Makefile and a symlinked Makefile that every anchor refused).

Standard library only, and it starts no process: the anchor must be able to run
this before make, and before anything else is trusted.

WHAT IT DECIDES, WITHOUT RUNNING ANY MAKEFILE
---------------------------------------------
  1. The pin has its one accepted shape (PIN_HEADER_RE / PIN_ENTRY_RE), is
     non-empty, and pins `Makefile`.
  2. Every pinned file is a REGULAR file (not a symlink, FIFO or device) whose
     sha256 is the pinned one.
  3. What make will read (static_read_set): the root `Makefile` — a
     `GNUmakefile` or `makefile` beside it would be read INSTEAD and is refused
     — plus every literal include/-include/sinclude/load path in the pinned
     bytes, transitively. A computed or out-of-repository name, and
     `$(eval …)` / `$(guile …)`, are refused. That set must equal the pin
     exactly: no unpinned file, no stale entry.

(3) is sound ONLY because it reads bytes whose digest already matched the pin:
bytes a reviewer approved together with it. It is not a model of make's parser
and is no defence against hostile text — a changed byte never reaches it.

What this module CANNOT decide is whether make would REMAKE a pinned file from
something nobody pinned (make's builtin implicit rules need no makefile line).
The anchor asks make that, with one `make -q` naming every pinned file.
"""

from __future__ import annotations

import hashlib
import os
import re
import stat
from dataclasses import dataclass, field
from pathlib import Path

PIN_FILE = Path(".github") / "pinned-makefiles.yml"

# The pin file's ONLY accepted shape: a YAML subset. Anything else — a quoted
# key, a tab, a flow mapping, a second document — is refused.
PIN_HEADER_RE = re.compile(r"^makefiles:[ \t]*$")
PIN_ENTRY_RE = re.compile(r"^  ([A-Za-z0-9_][A-Za-z0-9._/-]*): ([0-9a-f]{64})[ \t]*$")

# With no `-f`, GNU make reads the FIRST of these that exists in the working
# directory (manual §3.2). A `GNUmakefile` beside the Makefile would be read
# INSTEAD of it, so its mere presence is refused.
DEFAULT_MAKEFILE_NAMES = ("GNUmakefile", "makefile", "Makefile")

# The directives that make GNU make read (or load) another file.
_READ_DIRECTIVE_RE = re.compile(r"^[ \t]*(-include|sinclude|include|-load|load)(?:[ \t]+(.*))?$")

# Functions that can MANUFACTURE a directive at parse time: `$(eval include x)`,
# and guile's gmk-eval. A static reading cannot see what they will produce.
_MANUFACTURES_DIRECTIVES_RE = re.compile(r"\$[({](eval|guile)[\s)}]")

# Characters that make a directive's file name something make computes.
_COMPUTED_NAME_CHARS = set("$*?[%~`\\")

# Informational only: the parse-time execution the REVIEWED bytes contain.
_PARSE_TIME_EXEC_RE = re.compile(r"\$[({]shell[\s)}]|!=")


class PinError(Exception):
    """The pin file itself is unusable. `kind` is one of: missing, not-a-file,
    unreadable, encoding, header, shape, path, duplicate, empty."""

    def __init__(self, kind: str, message: str) -> None:
        super().__init__(message)
        self.kind = kind


@dataclass
class Problem:
    """One reason make must not be run. `kind` is one of: pin, no-makefile,
    absent, not-regular, unreadable, mismatch, encoding, default-name, eval,
    computed, outside, unpinned, stale."""

    kind: str
    file: str
    message: str
    got: str = ""
    want: str = ""
    pin_kind: str = ""


@dataclass
class PinResult:
    pins: dict = field(default_factory=dict)
    order: list = field(default_factory=list)      # what make will read, in order
    digests: dict = field(default_factory=dict)    # file -> sha256, for every regular pinned file
    problems: list = field(default_factory=list)
    sites: list = field(default_factory=list)      # reviewed parse-time execution, informational

    @property
    def ok(self) -> bool:
        return not self.problems


def load_makefile_pin(pin_path: Path) -> dict:
    """Parse the pin. Every deviation from the shape raises PinError."""
    label = PIN_FILE
    if pin_path.is_dir():
        raise PinError("not-a-file", f"{label} is a directory, not a file.")
    try:
        raw = pin_path.read_bytes()
    except FileNotFoundError:
        raise PinError("missing", f"{label} does not exist. It is the list of Makefile bytes make may read; "
                                  f"without it nothing says which bytes were reviewed.")
    except OSError as err:
        raise PinError("unreadable", f"{label} cannot be read: {err}")
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError:
        raise PinError("encoding", f"{label} is not UTF-8")
    entries: dict = {}
    header = False
    for n, line in enumerate(text.split("\n"), 1):
        if not line.strip() or line.startswith("#"):
            continue
        if not header:
            if not PIN_HEADER_RE.match(line):
                raise PinError("header", f"{label}:{n}: expected `makefiles:` as the first entry, found {line!r}")
            header = True
            continue
        m = PIN_ENTRY_RE.match(line)
        if not m:
            raise PinError("shape", f"{label}:{n}: not a `  <path>: <64 lowercase hex sha256>` entry: {line!r}")
        name, digest = m.group(1), m.group(2)
        if name != os.path.normpath(name) or name.startswith("../") or name == "..":
            raise PinError("path", f"{label}:{n}: {name!r} is not a normalised path inside the repository")
        if name in entries:
            raise PinError("duplicate", f"{label}:{n}: {name!r} is pinned twice")
        entries[name] = digest
    if not header or not entries:
        raise PinError("empty", f"{label} pins no file. An empty pin would let make read anything.")
    return entries


def logical_lines(text: str):
    """(first line number, joined line) with backslash continuations joined and comments cut."""
    lines = text.split("\n")
    i = 0
    while i < len(lines):
        first, line = i + 1, lines[i]
        # An ODD number of trailing backslashes continues the line.
        while line.endswith("\\") and (len(line) - len(line.rstrip("\\"))) % 2 == 1 and i + 1 < len(lines):
            i += 1
            line = line[:-1] + " " + lines[i].lstrip()
        # A `#` starts a comment unless escaped. Cutting it can only REMOVE a
        # directive argument, never add one, and the bytes are pinned anyway.
        out, j = [], 0
        while j < len(line):
            c = line[j]
            if c == "\\" and j + 1 < len(line):
                out.append(line[j:j + 2])
                j += 2
                continue
            if c == "#":
                break
            out.append(c)
            j += 1
        yield first, "".join(out)
        i += 1


def static_read_set(root: Path, texts: dict):
    """What make WILL read, determined without running a line of any makefile.

    Returns (ordered list of files, list of Problem). Sound ONLY over bytes whose
    sha256 already matched the pin. See the module docstring.

      1. The root makefile: with no `-f`, the first of GNUmakefile, makefile,
         Makefile in the working directory. Only `Makefile` may exist (exact
         directory-entry names, so a case-insensitive disk cannot hide one).
      2. MAKEFILES from the environment would be read before it — the anchor
         refuses it and drops it from every process it starts (not this module's job).
      3. Every include/-include/sinclude/load/-load in a file already in the
         set, transitively. Each name must be a literal repository-relative
         path; the caller requires it pinned and present — make searches -I
         and /usr/include-style directories only for a name NOT found relative
         to the working directory, so a pinned, present file is the one opened.
      4. No `$(eval …)` / `$(guile …)` anywhere in the set, comments and
         recipes included (a `#` is not a comment inside a recipe, and recipes
         are expanded under -n).
    """
    problems: list = []
    try:
        present = set(os.listdir(root))
    except OSError as err:
        return [], [Problem("default-name", ".", f"cannot list {root}: {err}")]
    for name in DEFAULT_MAKEFILE_NAMES[:-1]:
        if name in present:
            problems.append(Problem("default-name", name,
                                    f"{name} exists beside the Makefile. With no -f, GNU make reads {name} INSTEAD "
                                    f"of Makefile, so the pinned Makefile would not be what runs."))
    order: list = []
    queue = ["Makefile"]
    while queue:
        rel = queue.pop(0)
        if rel in order:
            continue
        order.append(rel)
        text = texts.get(rel)
        if text is None:
            continue  # unpinned or unreadable — reported by the caller
        for n, line in enumerate(text.split("\n"), 1):
            if _MANUFACTURES_DIRECTIVES_RE.search(line):
                problems.append(Problem("eval", rel,
                                        f"{rel}:{n} calls $(eval …) or $(guile …), which can manufacture an include "
                                        f"the static reading cannot see: `{line.strip()[:120]}`"))
        for n, line in logical_lines(text):
            m = _READ_DIRECTIVE_RE.match(line)
            if not m:
                continue
            directive, args = m.group(1), (m.group(2) or "").strip()
            for word in args.split():
                if directive.endswith("load"):
                    word = word.split("(", 1)[0]
                if _COMPUTED_NAME_CHARS & set(word):
                    problems.append(Problem("computed", rel,
                                            f"{rel}:{n} `{directive} {word}` names a file make COMPUTES; which file "
                                            f"it reads cannot be determined without running the makefile."))
                    continue
                norm = os.path.normpath(word)
                if os.path.isabs(word) or norm == ".." or norm.startswith("../"):
                    problems.append(Problem("outside", rel,
                                            f"{rel}:{n} `{directive} {word}` reads a file outside the repository."))
                    continue
                queue.append(norm)
    return order, problems


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def verify_pin(root: Path, pin_path: Path = None) -> PinResult:
    """Everything this module decides, for the tree at `root`. Reads files; starts no process."""
    root = Path(root)
    pin_path = root / PIN_FILE if pin_path is None else Path(pin_path)
    r = PinResult()
    try:
        r.pins = load_makefile_pin(pin_path)
    except PinError as err:
        r.problems.append(Problem("pin", str(PIN_FILE), str(err), pin_kind=err.kind))
        return r
    if "Makefile" not in r.pins:
        r.problems.append(Problem("no-makefile", "Makefile",
                                  f"{PIN_FILE} does not pin `Makefile`, the file make reads first."))
        return r

    texts: dict = {}
    for rel in sorted(r.pins):
        path = root / rel
        try:
            st = os.lstat(path)
        except FileNotFoundError:
            r.problems.append(Problem("absent", rel, f"{rel} is pinned in {PIN_FILE} but does not exist.",
                                      want=r.pins[rel]))
            continue
        if not stat.S_ISREG(st.st_mode):
            r.problems.append(Problem("not-regular", rel,
                                      f"{rel} is not a regular file (a symlink, FIFO or device is refused): make "
                                      f"would read whatever it points at, which is not what was digested."))
            continue
        try:
            data = path.read_bytes()
        except OSError as err:
            r.problems.append(Problem("unreadable", rel,
                                      f"{rel} is pinned but cannot be read ({err.strerror or err}); its digest "
                                      f"cannot be checked, so make is not run on it."))
            continue
        r.digests[rel] = sha256(data)
        if r.digests[rel] != r.pins[rel]:
            r.problems.append(Problem("mismatch", rel,
                                      f"{rel}: sha256 {r.digests[rel]} does not match {PIN_FILE} ({r.pins[rel]}).",
                                      got=r.digests[rel], want=r.pins[rel]))
            continue
        try:
            texts[rel] = data.decode("utf-8")
        except UnicodeDecodeError:
            r.problems.append(Problem("encoding", rel,
                                      f"{rel} is not UTF-8 text; the files make may read cannot be determined from it."))

    r.order, problems = static_read_set(root, texts)
    r.problems.extend(problems)
    for f in r.order:
        if f not in r.pins:
            r.problems.append(Problem("unpinned", f, f"make would read {f}, which has no entry in {PIN_FILE}."))
    stale = sorted(set(r.pins) - set(r.order))
    if stale and not r.problems:
        r.problems.append(Problem("stale", ", ".join(stale),
                                  f"{PIN_FILE} pins {', '.join(stale)}, which make would NOT read from the pinned "
                                  f"bytes."))
    if not r.problems:
        for rel in r.order:
            for n, line in logical_lines(texts[rel]):
                if _PARSE_TIME_EXEC_RE.search(line):
                    r.sites.append(f"{rel}:{n} `{line.strip()[:90]}`")
    return r
