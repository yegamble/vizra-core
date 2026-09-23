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

  4. THE ALLOWLIST GRAMMAR (grammar_problems; queue 2p, core B5d, ported from
     vizra-search scripts/makegate.py at 4810048): every logical line of every
     pinned file whose digest matched is one of five shapes (see the block
     comment above grammar_problems), or it is refused by line number. It is
     the PRIMARY pre-make control; `include` is outside it, so (3) can only
     ever accept the root Makefile — (3) stays as defence in depth.
  5. ONE line reader (makefile_lines, cached per text): the grammar, (3), the
     parse-time sites, and every reading in the anchor and ci-required-guard
     consume the same logical-line sequence (scripts/testdata/one-reader-probe.py).

(3) is sound ONLY because it reads bytes whose digest already matched the pin:
bytes a reviewer approved together with it. It is not a model of make's parser
and is no defence against hostile text — a changed byte never reaches it.

What this module CANNOT decide is whether make would REMAKE a pinned file from
something nobody pinned (make's builtin implicit rules need no makefile line).
The anchor asks make that, with one `make -q` naming every pinned file.
"""

from __future__ import annotations

import functools
import hashlib
import os
import re
import stat
import unicodedata
from dataclasses import dataclass, field
from pathlib import Path
from typing import NamedTuple, Optional

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
    grammar: list = field(default_factory=list)    # grammar_problems for every readable pinned file
    texts: dict = field(default_factory=dict)      # file -> decoded text (decode_makefile)

    @property
    def ok(self) -> bool:
        return not self.problems and not self.grammar


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


# ------------------------------------------------------------ the one line reader ---
#
# Ported from vizra-search `scripts/makegate.py` at 4810048 (search #5, VERIFIED): decode_makefile,
# makefile_lines, keeps_rule_open, recipe_lines, _strip_comment. ONE reading of a makefile's bytes serves
# EVERY check in this repository that reads makefile text (queue 2p, core B5d): the grammar and the static
# read set here; prerequisite_closure, check_text and check_environment_overrides in the anchor;
# load_makefile_env_names and check_makefile_selection in ci-required-guard. Two readers that split lines
# differently disagree on which recipe a TAB line belongs to (search PR #5 closing VERIFY, FINDING 14); a
# single reader cannot. scripts/testdata/one-reader-probe.py checks POISON (rewriting the text inside
# makefile_lines changes every named reader's verdict), SOURCE (no reader and no other place in the three
# files splits or reads makefile text itself, except the NAMED reads of other inputs) and IDENTITY (every
# reader of one text receives the same sequence object).


class LogicalLine(NamedTuple):
    """One logical line of a makefile, as makefile_lines() reads it."""

    n: int                      # number of its first physical line (1-based)
    segments: tuple             # the physical lines it was joined from, exactly as decoded
    tab: bool                   # its first physical line starts with TAB (a recipe line while a rule is open)
    raw: str                    # the physical lines joined as make joins them, comments included
    code: str                   # non-TAB: raw before a make comment; TAB: raw (make hands `#` to the shell)
    comment_at: int             # non-TAB: index in raw of the `#` that starts a make comment; else -1
    comment_why: Optional[str]  # non-TAB: why that comment boundary is refused (a `#` inside `$(…)`), or None
    tail: int                   # index in raw where the text of its LAST physical line starts


def decode_makefile(data: bytes) -> str:
    """THE decoding of makefile bytes: strict UTF-8, NO newline translation (a CR stays a CR)."""
    return data.decode("utf-8")


def read_makefile_text(path) -> str:
    return decode_makefile(Path(path).read_bytes())


def _continued(physical: str) -> bool:
    """make continues a line that ends in an ODD number of backslashes immediately before the newline."""
    return (len(physical) - len(physical.rstrip("\\"))) % 2 == 1


def _strip_comment(line: str):
    """(text before an unescaped `#` at expansion depth 0, problem or None)."""
    depth, i = 0, 0
    while i < len(line):
        c = line[i]
        if c == "\\" and i + 1 < len(line):
            i += 2
            continue
        if c == "$" and i + 1 < len(line) and line[i + 1] in "({":
            depth += 1
            i += 2
            continue
        if c in ")}" and depth:
            depth -= 1
        elif c == "#":
            if depth:
                return line[:i], "a `#` inside `$(…)`, where whether it starts a comment is not left to the make version"
            return line[:i], None
        i += 1
    return line, None


@functools.lru_cache(maxsize=64)
def makefile_lines(text: str) -> tuple:
    """THE line reader: `text` split on LF only, backslash-newline joined as make joins it.

    A TAB line (a recipe line) keeps its continuation lines raw, minus the one leading TAB make removes; any
    other line joins its continuation with one space and the continuation's leading spaces/TABs removed. The
    result is cached per text, so every reader of the same text in one program receives the SAME sequence.
    """
    phys = text.split("\n")
    out = []
    i = 0
    while i < len(phys):
        first = i
        raw = phys[i]
        tab = raw.startswith("\t")
        tail = 0
        while _continued(phys[i]) and i + 1 < len(phys):
            i += 1
            nxt = phys[i]
            part = (nxt[1:] if nxt.startswith("\t") else nxt) if tab else nxt.lstrip(" \t")
            raw = raw[:-1] + " "
            tail = len(raw)
            raw += part
        if tab:
            code, at, why = raw, -1, None
        else:
            code, why = _strip_comment(raw)
            at = len(code) if len(code) < len(raw) else -1
        out.append(LogicalLine(first + 1, tuple(phys[first:i + 1]), tab, raw, code, at, why, tail))
        i += 1
    return tuple(out)


def read_makefile_lines(path) -> tuple:
    """makefile_lines() over read_makefile_text(path): what every reader that holds a PATH calls."""
    return makefile_lines(read_makefile_text(path))


def keeps_rule_open(rec: LogicalLine) -> bool:
    """A blank line (an EMPTY line) or a comment line (`#` in column 0) leaves an open rule's recipe open."""
    return not rec.tab and (rec.raw == "" or rec.raw.startswith("#"))


def recipe_lines(lines, start: int) -> list:
    """The recipe of the rule on lines[start - 1]: (line number, text after its TAB, stripped) for every TAB
    line from lines[start] on. Blank and comment lines keep the recipe open; any other line closes it —
    exactly as grammar_problems decides which rule a TAB line belongs to."""
    recipe = []
    for rec in lines[start:]:
        if rec.tab:
            recipe.append((rec.n, rec.raw[1:].strip()))
        elif not keeps_rule_open(rec):
            break
    return recipe


# ------------------------------------------------------ the Makefile grammar ---
#
# THE PRIMARY pre-make control for what the reviewed bytes may SAY (queue 2p, core B5d; ported from
# vizra-search makegate.py grammar_problems at 4810048). An ALLOWLIST of line shapes, default-deny: every
# round of review found another spelling a denylist missed, because make's grammar is unbounded; this
# repository's Makefile uses a tiny part of it, so that part is all a pinned makefile may use. It reads the
# ONE logical-line sequence (makefile_lines) every later reader reads too. First, BY NAME, whatever the
# shape: a CR anywhere, a NUL or any other control character except TAB, an invisible format character or
# non-ASCII whitespace (_forbidden_char); a line of only spaces/TABs (blank means EMPTY: make reads a
# TAB-only line in a rule as an empty recipe line); and a comment that ends in an unescaped backslash, which
# make continues onto the next line. Then every logical line must be exactly one of:
#
#   BLANK/COMMENT  empty, or a comment line with `#` in column 0 (keeps_rule_open), or a comment after an
#                  assignment or rule (`#` outside any `$(…)`/`${…}`; text after it is ignored);
#   ASSIGNMENT     `NAME op value`: NAME a literal `[A-Za-z_][A-Za-z0-9_]*` that is not one of
#                  DIRECTIVE_KEYWORDS, or one of ASSIGNABLE_SPECIALS, op one of `:=` `?=` `=`, at the start of
#                  the line; the value may use only `$$`, `$(NAME)`/`${NAME}` references and `$(shell …)`
#                  (whose text may use the same references); every other `$` form — a function, a
#                  substitution reference, `$X`, a computed name — is refused;
#   PHONY          `.PHONY: name …` with literal names;
#   RULE           `name: prerequisite …` at the start of the line: ONE literal target (not starting with
#                  `.`, so no special target, suffix or pattern rule; not a directive keyword), one `:`,
#                  literal prerequisite words; no `;`, `$`, `%`, `|`, `=`, second `:`, `::` or `&:`;
#   RECIPE         a TAB line (with its backslash-continued lines, read RAW: make hands `#` in a recipe to the
#                  shell) while a RULE is open — empty and comment lines between recipe lines keep it open,
#                  any other line closes it (keeps_rule_open; recipe_lines makes the same decision for every
#                  reader). Its text may use only `$$` and `$(NAME)`/`${NAME}` references, and not `$(MAKE)`.
#
# Anything else — a conditional, include, define, export, override, private, vpath, undefine, load, an inline
# `;` recipe, several targets, a special target other than .PHONY, `+=`/`!=`/`::=`, leading whitespace, a TAB
# line outside a rule, a `#` inside `$(…)` — is refused by line number. The by-name refusals in the anchor
# (check_text, REFUSED_TOKENS, …) stay as a SECOND, more specific diagnosis, and the pin, the remake probe
# and the post-make database checks stay as defence in depth.
#
# Core's differences from search's grammar, all NARROWER or equal: none. The same shapes; a recipe body that
# BEGINS with `$(NAME)` (core's `@$(GO) vet $(PKGS)`) is allowed by the grammar in both repositories — search
# refuses it by name, core resolves it from make's own database after make (check_expanded_prefixes).
ASSIGNABLE_SPECIALS = (".SHELLFLAGS", ".DEFAULT_GOAL")
_G_NAME = r"(?:[A-Za-z_][A-Za-z0-9_]*|\.SHELLFLAGS|\.DEFAULT_GOAL)"
_G_ASSIGN_RE = re.compile(r"^(" + _G_NAME + r")[ \t]*(:=|\?=|=)(.*)$")
_G_WORD = r"[A-Za-z0-9_][A-Za-z0-9_./-]*"
_G_PHONY_RE = re.compile(r"^\.PHONY[ \t]*:((?:[ \t]+" + _G_WORD + r")+)[ \t]*$")
_G_RULE_RE = re.compile(r"^(" + _G_WORD + r")[ \t]*:((?:[ \t]+" + _G_WORD + r")*)[ \t]*$")
_G_IDENT_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")

# GNU Make's directive words: none may be an assigned NAME or a rule target, because whether make reads
# `ifdef := 1` as an assignment or as the directive has differed between make versions.
DIRECTIVE_KEYWORDS = frozenset({
    "ifdef", "ifndef", "ifeq", "ifneq", "else", "endif", "include", "-include", "sinclude", "define", "endef",
    "export", "unexport", "override", "private", "undefine", "vpath", "load", "-load",
})
_G_LEAD_RE = re.compile(r"^([^\s:=?+!#]+)[ \t]*(:::=|::=|:=|\?=|\+=|!=|=|&?::?)?")


def _dollar_problems(text: str, allow_shell: bool) -> list:
    """Every `$` use in `text` that is not `$$`, a literal `$(NAME)`/`${NAME}` reference, or (when
    allow_shell) `$(shell …)` whose own text passes the same check without shell."""
    out, i = [], 0
    while i < len(text):
        if text[i] != "$":
            i += 1
            continue
        nxt = text[i + 1] if i + 1 < len(text) else ""
        if nxt == "$":
            i += 2
            continue
        if nxt not in "({":
            out.append(f"`{text[i:i + 2]}` (only `$$`, `$(NAME)` and `${{NAME}}` are allowed)")
            i += 2
            continue
        close = ")" if nxt == "(" else "}"
        depth, j = 1, i + 2
        while j < len(text) and depth:
            if text[j] == nxt:
                depth += 1
            elif text[j] == close:
                depth -= 1
            j += 1
        if depth:
            out.append(f"`{text[i:i + 30]}` (an unterminated expansion)")
            break
        inner = text[i + 2:j - 1]
        if _G_IDENT_RE.match(inner):
            if inner == "MAKE":
                out.append("`$(MAKE)`")
        elif allow_shell and nxt == "(" and re.match(r"shell[ \t]", inner):
            out += _dollar_problems(inner[6:], allow_shell=False)
        else:
            out.append(f"`{text[i:j][:60]}` (a function, a substitution reference or a computed name)")
        i = j
    return out


def _forbidden_char(ch: str):
    """Why `ch` may not appear in a pinned makefile, or None. Only TAB and LF among control characters."""
    if ch == "\t" or " " <= ch <= "~":
        return None
    if ch == "\r":
        return ("a carriage return (CR, U+000D): make drops a CR before a newline but keeps it elsewhere, and a "
                "text reader with universal newlines breaks the line there")
    if ch == "\0":
        return "a NUL byte (U+0000): make does not read the rest of that line"
    cat = unicodedata.category(ch)
    if cat == "Cc":
        return f"a control character (U+{ord(ch):04X})"
    if cat == "Cf":
        return f"an invisible format character (U+{ord(ch):04X}), which a reviewer cannot see"
    if ch.isspace():
        return f"a non-ASCII whitespace character (U+{ord(ch):04X}), which make does not read as whitespace"
    return None


def grammar_problems(rel: str, text: str) -> list:
    """Default-deny: every logical line (makefile_lines) must be one of the shapes in the block comment above."""
    out: list = []
    in_rule = False
    shapes = {"blank/comment": 0, "assignment": 0, "phony": 0, "rule": 0, "recipe": 0}

    def refuse(n: int, line: str, why: str) -> None:
        out.append(f"{rel}:{n} is outside the Makefile grammar this anchor allows ({why}): `{line.strip()[:100]}`. "
                   f"Every line must be empty, a comment, `NAME := | ?= | = value`, `.PHONY: names`, a "
                   f"single-target rule `name: prerequisites`, or a TAB recipe line of a rule.")

    for rec in makefile_lines(text):
        n, line = rec.n, rec.raw
        bad = [(rec.n + k, seg, why) for k, seg in enumerate(rec.segments)
               for why in [next(filter(None, map(_forbidden_char, seg)), None)] if why]
        if bad:
            for k, seg, why in bad:
                refuse(k, repr(seg)[1:-1], why)
            in_rule = False
            continue
        if line and not line.strip(" \t"):
            refuse(n, repr(line), "a line of only spaces or TABs; a blank line must be empty, because make "
                                  "reads a TAB-only line inside a rule as an empty recipe line")
            in_rule = False
            continue
        if rec.tab:
            if not in_rule:
                refuse(n, line, "a TAB line outside a rule")
                continue
            shapes["recipe"] += 1
            for d in _dollar_problems(line, allow_shell=False):
                refuse(n, line, f"a recipe line using {d}")
            continue
        if rec.comment_why:
            refuse(n, line, rec.comment_why)
            in_rule = False
            continue
        if 0 <= rec.comment_at < rec.tail:
            refuse(n, line, "a comment continued onto the next line by a trailing backslash; make reads that next "
                            "line as part of the comment")
            in_rule = False
            continue
        if keeps_rule_open(rec):
            shapes["blank/comment"] += 1  # an empty line, or a comment line (`#` in column 0)
            continue
        body = rec.code
        in_rule = False
        lead = _G_LEAD_RE.match(body)
        if lead and lead.group(1) in DIRECTIVE_KEYWORDS and lead.group(2):
            kind = "a rule target" if lead.group(2).lstrip("&") in (":", "::") else "a variable name"
            refuse(n, line, f"a directive keyword (`{lead.group(1)}`) as {kind}; make may read the line as that "
                            f"directive")
            continue
        m = _G_ASSIGN_RE.match(body)
        if m:
            shapes["assignment"] += 1
            for d in _dollar_problems(m.group(3), allow_shell=True):
                refuse(n, line, f"an assignment value using {d}")
            continue
        if (_G_PHONY_RE.match(body) or _G_RULE_RE.match(body)) and len(rec.segments) > 1:
            # Core B5d, stricter than search's grammar (search refuses this by name, after it): a rule or
            # `.PHONY:` line is ONE physical line, so its line number is where every word of it is.
            refuse(n, line, "a rule line continued with a backslash; write the rule on one line")
            continue
        if _G_PHONY_RE.match(body):
            shapes["phony"] += 1
            continue
        if _G_RULE_RE.match(body):
            shapes["rule"] += 1
            in_rule = True
            continue
        words = body.split()
        first = words[0] if words else ""
        if first in ("ifeq", "ifneq", "ifdef", "ifndef", "else", "endif"):
            why = "a conditional directive"
        elif first in ("include", "-include", "sinclude", "load", "-load"):
            why = f"the `{first}` directive"
        elif first in ("define", "endef", "undefine", "export", "unexport", "override", "private", "vpath"):
            why = f"the `{first}` directive"
        elif body[:1] in (" ", "\t"):
            why = "a line that starts with whitespace"
        elif ";" in body:
            why = "an inline `;` recipe or a `;` outside a recipe"
        elif "$" in body:
            why = "an expansion outside an assignment value or a recipe"
        else:
            why = "not one of the allowed shapes"
        refuse(n, line, why)
    grammar_problems.last_shapes = shapes
    return out


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
        lines = makefile_lines(text)
        # Searched in the RAW logical lines, comments and recipes included: a `#` is not a comment inside a
        # recipe, and a recipe is expanded under `-n` too.
        for rec in lines:
            if _MANUFACTURES_DIRECTIVES_RE.search(rec.raw):
                problems.append(Problem("eval", rel,
                                        f"{rel}:{rec.n} calls $(eval …) or $(guile …), which can manufacture an "
                                        f"include the static reading cannot see: `{rec.raw.strip()[:120]}`"))
        for rec in lines:
            n = rec.n
            m = _READ_DIRECTIVE_RE.match(rec.code)
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
            texts[rel] = decode_makefile(data)
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
    # THE GRAMMAR (queue 2p): every pinned makefile whose bytes were read, line by line. Kept apart from
    # r.problems so a caller can still run its by-name reading as the second diagnosis; r.ok covers both.
    for rel in sorted(texts):
        r.grammar.extend(Problem("grammar", rel, msg) for msg in grammar_problems(rel, texts[rel]))
    r.texts = dict(texts)
    if not r.problems:
        for rel in r.order:
            for rec in makefile_lines(texts[rel]):
                if _PARSE_TIME_EXEC_RE.search(rec.code):
                    r.sites.append(f"{rel}:{rec.n} `{rec.code.strip()[:90]}`")
    return r
