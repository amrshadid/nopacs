r"""Check value patterns and redact in de-identified files with pydicom.

Input: the path of a JSON file:
  {"redact": <a regular expression a rule cut out of every value it
              matched in, with no replacement>,
   "remove": <a regular expression a rule removed every element whose
              whole value it matched>,
   "files": [{"input": <path of a file as given to the engine>,
              "output": <path of the same file de-identified>}]}

For each pair, pydicom reads the input and the output. Every element of
VR SH, LO, UC, ST, LT, UT or PN of the input, at any depth of sequences,
is read for the two patterns, each value of a multi-valued element apart
(an ST, LT or UT value is one value):

  - an element of VR SH, LO, UC, ST, LT or UT holding a match of redact
    must be in the output with every match cut out, again until none is
    left, and nothing else changed (padding aside);
  - an element a value of which remove matches whole must not be in the
    output.

Then every text redact matched and every value remove matched must be
nowhere in the output: in no element's value as pydicom decodes it, at any
depth, and not in the raw bytes of the whole file.

A person's name (VR PN) is not checked for redact: the engine never keeps
part of one, and removes it unless the IOD requires it.

Where this script differs from the engine: Python's re finds the leftmost
match by its first alternative that matches, where the engine's search
finds the longest at the leftmost place; the patterns given must hold no
alternation whose alternatives begin alike (as MRN|MRN\d+), for which the
two would cut differently. Each value is compared with its padding
removed, leading spaces too for SH and LO, as the engine reads it.

Exit status 0 when every pair holds and at least one match was checked; 1
otherwise. A failure prints the input, the element and what was wrong (the
values are public test data and planted synthetic text).
"""
import json
import re

import pydicom

TEXT_VRS = {"SH", "LO", "UC", "ST", "LT", "UT", "PN"}
SINGLE = {"ST", "LT", "UT"}


def parts(elem):
    """Return an element's values as text, padding removed as PS3.5 6.2
    allows: trailing spaces and NULs, and leading spaces for SH and LO."""
    v = elem.value
    if v is None:
        return []
    if elem.VR in SINGLE:
        return [str(v).rstrip(" \x00")]
    values = v if isinstance(v, (list, pydicom.multival.MultiValue)) else [v]
    out = []
    for x in values:
        x = str(x).rstrip(" \x00")
        if elem.VR in ("SH", "LO"):
            x = x.lstrip(" ")
        out.append(x)
    return out


def cut(pattern, text):
    """Cut every match of pattern out of text, again until none is left."""
    while True:
        out = pattern.sub("", text)
        if out == text:
            return out
        text = out


def walk(ds, path=""):
    """Yield (path, element) for every element of ds at any depth."""
    for elem in ds:
        p = path + "%08X" % int(elem.tag)
        if elem.VR == "SQ":
            for i, item in enumerate(elem.value or []):
                yield from walk(item, p + "[%d]/" % i)
            continue
        yield p, elem


def check(spec):
    redact = re.compile(spec["redact"])
    remove = re.compile(spec["remove"])
    failures, checked = [], 0
    for pair in spec["files"]:
        src = pydicom.dcmread(pair["input"], force=True)
        out = pydicom.dcmread(pair["output"], force=True)
        after = {p: e for p, e in walk(out)}
        matched = set()
        for p, elem in walk(src):
            if elem.VR not in TEXT_VRS:
                continue
            values = parts(elem)
            whole = [v for v in values if v and remove.fullmatch(v)]
            if whole:
                checked += 1
                matched.update(whole)
                if p in after:
                    failures.append("%s: %s: matched whole and still present" % (pair["input"], p))
                continue
            found = [m.group(0) for v in values for m in redact.finditer(v)]
            if not found or elem.VR == "PN":
                continue
            checked += 1
            matched.update(found)
            want = [cut(redact, v) for v in values]
            if p not in after:
                failures.append("%s: %s: redacted element missing" % (pair["input"], p))
                continue
            got = [v.rstrip(" ") for v in parts(after[p])]
            if got != [v.rstrip(" ") for v in want]:
                failures.append("%s: %s: redacted to %r, want %r" % (pair["input"], p, got, want))
        raw = open(pair["output"], "rb").read()
        for text in sorted(matched):
            if text.encode("utf-8") in raw:
                failures.append("%s: %r is in the output's bytes" % (pair["input"], text))
            for p, elem in after.items():
                if elem.VR in TEXT_VRS or elem.VR in {"CS", "AE", "UR"}:
                    if any(text in v for v in parts(elem)):
                        failures.append("%s: %r is in %s of the output" % (pair["input"], text, p))
    return failures, checked


def main(argv):
    with open(argv[0], encoding="utf-8") as f:
        spec = json.load(f)
    failures, checked = check(spec)
    for line in failures:
        print(line)
    print("%d files, %d elements checked, %d failures" % (len(spec["files"]), checked, len(failures)))
    return 0 if checked and not failures else 1

