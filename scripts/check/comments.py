#!/usr/bin/env python3
"""Fail on text in the repository that points at something a reader cannot see.

A comment, a document or a script message must say its reason itself: the
constraint and the failure it prevents, or a published rule (a standard, an
RFC, a vendor's documentation, the API contract in api/proto). A pointer to a
note that is not in the repository explains nothing to the person reading it,
so this check refuses the forms such pointers take: a numbered decision such as
"D12", a design note or its parts, a review and its points, an issue's
acceptance points, a milestone name such as "M0", a numbered research
question, a Markdown file the repository does not have, and the names of tools
used to write the code.

It reads every file git tracks except gen/ (generated from api/proto, whose
comments it checks at the source) and go.sum, and the tracked paths
themselves, and prints file:line: reason: text for each hit (line 0 is the
path). Exit status 1 on any hit, 0 on none.

A new false positive is fixed by rewording the text, or, when the text is
right as it is, by one line in ALLOWED below with its reason. A rule is never
loosened to let one through.

This file is excluded from its own scan by path (SELF below): it necessarily
spells out every pattern it refuses.

Usage: comments.py [root]. root defaults to the current directory, which
must be inside a git work tree. Standard library only.
"""

import os
import re
import subprocess
import sys

SELF = "scripts/check/comments.py"

# Paths not scanned, by exact path or by prefix ending in "/".
SKIPPED = ("gen/", "go.sum", SELF)

# Each rule: a reason printed with a hit, and a regular expression. The
# decision, research and milestone rules are case-sensitive, because "D12",
# "R6" and "M0" are what a citation looks like, and lowercase "d12" or "m1" in
# a hex string or test data is not one; the others ignore case.
RULES = [
    # A numbered decision: "D12" as a word. The word boundary before D keeps
    # out hex strings and SQLSTATE codes such as 3D000 or %2D91, where D
    # follows a digit or a letter. This covers every citation form, "(D12)",
    # "decision D12", ", D12", "D12:", "guards D12", and a bare "D12 needs a
    # look" too; a real name of that shape, should one ever be needed, goes
    # in ALLOWED with its reason.
    ("decision reference", re.compile(r"\bD\d{1,3}\b|\bdecisions? [A-Z]?\d")),
    # A numbered research question, "R6", or the folder of such notes.
    ("research reference", re.compile(r"\bR\d{1,2}\b|\bresearch [A-Z]?\d|\bresearch/")),
    ("design note", re.compile(r"design note", re.I)),
    ("acceptance point", re.compile(r"\bacceptance (?:point|criteri|\d)", re.I)),
    # A review's findings, a numbered point or step of some list the reader
    # does not have, and a test named for a review.
    ("review reference", re.compile(
        r"\bof the review\b|\bthe review found\b|\bthe reviewer|\bin the review\b"
        r"|\bpoints? \d+[a-z]?\b|\bstep \d+[a-z]\b|\bTestReview",
        re.I)),
    ("guide reference", re.compile(r"\bthe guide\b", re.I)),
    # A milestone name as a word, "M0", and in lowercase as the end of a
    # hyphenated name, "prove-m0", as a file or a make target would carry it
    # (a bare lowercase "m1" is test data in places, so it is not refused).
    # "M1 Pro" and the like, the name of a processor, are let through by
    # ALLOWED, phrase by phrase.
    ("milestone name", re.compile(r"\bM[0-7]\b|\b[a-z]+-m[0-7]\b")),
    ("milestone", re.compile(r"\bmilestones?\b", re.I)),
    ("planning file", re.compile(r"\.claude\b|\bplans/|\bwork-\d", re.I)),
    ("decision by a person", re.compile(
        r"\bthe owner (?:decided|said|asked|wants|chose|confirmed|agreed|prefers)"
        r"|\bas decided\b|\bper the plan\b|\bthe brief\b|\bthe lead\b",
        re.I)),
    ("tool name", re.compile(r"\bclaude\b|\banthropic\b", re.I)),
]

# A Markdown file named in text must be one the repository has: any other is
# a note a reader cannot open. Checked against the tracked paths in main().
MARKDOWN = re.compile(r"\b[A-Za-z0-9_.-]+\.md\b")

# A path that names a review, a point or a finding instead of what it holds.
PATH_RULES = [
    ("file named for a review", re.compile(r"(?:^|/)[^/]*(?:review|finding|point\d)[^/]*$", re.I)),
] + RULES

# Exact text let through, one entry per line of the repository that a rule
# matches and that cites nothing:
#
#   ("path/to/file", "exact text on the line", "why it cites nothing"),
#
# for example a processor's name in a measured number:
#
#   ("cmd/nopacs-server/bench_test.go", "Apple M1 Pro", "a processor, not a milestone"),
#
# The text must cover the whole match, so an entry lets through that phrase
# on that file's lines and nothing else.
ALLOWED = [
]

# The leader of a comment or a quoted line, removed from the second of two
# lines before they are joined.
LEADER = re.compile(r"^\s*(?://+|#+|--+|\*+|;+|>+)?\s*")


def tracked_files(root):
    out = subprocess.run(
        ["git", "-C", root, "ls-files", "-z"],
        check=True, capture_output=True,
    ).stdout
    return [p for p in out.decode("utf-8", "replace").split("\0") if p]


def skipped(path):
    return any(path == s or (s.endswith("/") and path.startswith(s)) for s in SKIPPED)


def allowed(path, line, start, end):
    for apath, text, _reason in ALLOWED:
        if apath != path:
            continue
        i = line.find(text)
        while i >= 0:
            if i <= start and end <= i + len(text):
                return True
            i = line.find(text, i + 1)
    return False


def scan_line(path, number, line, rules, hits, markdown=None):
    for reason, rule in rules:
        for m in rule.finditer(line):
            if not allowed(path, line, m.start(), m.end()):
                hits.append(f"{path}:{number}: {reason}: {line.strip()}")
                break
    if markdown is None:
        return
    for m in MARKDOWN.finditer(line):
        if m.group(0) not in markdown and not allowed(path, line, m.start(), m.end()):
            hits.append(f"{path}:{number}: Markdown file not in the repository: {line.strip()}")
            break


def scan_file(path, text, markdown, hits):
    lines = text.splitlines()
    for number, line in enumerate(lines, 1):
        scan_line(path, number, line, RULES, hits, markdown)
    # A phrase broken across two comment lines, "design" at the end of one
    # and "note" at the start of the next, is joined and scanned again; a hit
    # counts only when it spans the join, since one within a line is already
    # reported.
    for number in range(1, len(lines)):
        a = lines[number - 1].rstrip()
        joined = a + " " + LEADER.sub("", lines[number])
        for reason, rule in RULES:
            for m in rule.finditer(joined):
                if m.start() < len(a) < m.end() and not allowed(path, joined, m.start(), m.end()):
                    hits.append(f"{path}:{number}: {reason} (across lines {number} and "
                                f"{number + 1}): {joined.strip()}")
                    break


def main():
    root = sys.argv[1] if len(sys.argv) > 1 else "."
    hits = []
    paths = tracked_files(root)
    markdown = {os.path.basename(p) for p in paths if p.endswith(".md")}
    markdown |= {p for p in paths if p.endswith(".md")}
    for path in paths:
        if skipped(path):
            continue
        scan_line(path, 0, path, PATH_RULES, hits)
        try:
            with open(os.path.join(root, path), "rb") as f:
                data = f.read()
        except FileNotFoundError:
            continue  # deleted in the work tree, not yet in the index
        if b"\0" in data[:8192]:
            continue  # binary
        scan_file(path, data.decode("utf-8", "replace"), markdown, hits)
    def where(hit):
        path, number, _ = hit.split(":", 2)
        return path, int(number)

    for h in sorted(hits, key=where):
        print(h)
    if hits:
        print(f"check-comments: {len(hits)} line(s) refer to something outside the "
              "repository; say the reason itself (see scripts/check/comments.py)",
              file=sys.stderr)
        return 1
    print("check-comments: no reference to anything outside the repository")
    return 0


if __name__ == "__main__":
    sys.exit(main())
