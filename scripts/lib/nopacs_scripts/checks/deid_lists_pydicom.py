"""Check a profile's text lists in de-identified files with pydicom.

Input: the path of a JSON file:
  {"terms": [<each term of the lists>],
   "files": [{"original": <path of a file as given to the engine>,
              "result": <path of the same file de-identified>,
              "cleaned": {<tag as 8 hex digits>: <the text the cleaned
                          element must hold>},
              "kept": [<tag as 8 hex digits of each top-level element the
                       engine kept unchanged>]}]}

For each pair, pydicom alone checks that:

  - no term stands as a word, in any letter case, in any text value of the
    result at any depth: not preceded or followed by a letter or a digit,
    the rule the engine cuts by, so a term inside a longer word is no
    finding;
  - each cleaned element holds exactly the expected text (trailing padding
    aside): the terms cut, every other character kept;
  - each element the engine kept is in the result with the value it had in
    the original (a sequence with as many items).

Exit status 0 when every pair holds and at least one kept element was
compared; 1 otherwise. A failure names the file, the element and what was
wrong, and never a term: it says which term by its position in the list.
"""
import json
import re

import pydicom

from nopacs_scripts.deid import element_text, walk


def term_patterns(terms):
    """A pattern per term that finds it standing as a word, in any case."""
    return [re.compile(r"(?<![^\W_])" + re.escape(t) + r"(?![^\W_])", re.IGNORECASE) for t in terms]


def standing_terms(ds, patterns):
    """(tag, term position) for each term standing in a text value of ds, at
    any depth; a value with no text form is read as UTF-8."""
    out = []
    for e in walk(ds):
        if e.VR == "SQ":
            continue
        text = element_text(e)
        if text is None and isinstance(e.value, (bytes, bytearray)):
            text = bytes(e.value).decode("utf-8", "replace")
        if not text:
            continue
        for i, p in enumerate(patterns):
            if p.search(text):
                out.append(("%08X" % int(e.tag), i + 1))
    return out


def cleaned_mismatches(result, cleaned):
    """The tags of cleaned whose element in result holds other text."""
    bad = []
    for tag_hex, want in cleaned.items():
        tag = int(tag_hex, 16)
        got = str(result[tag].value).rstrip(" \x00") if tag in result else None
        if got != want:
            bad.append(tag_hex)
    return bad


def kept_changes(original, result, kept):
    """(tag, what is wrong) for each kept element missing or changed in
    result, and how many were compared."""
    bad, compared = [], 0
    for tag_hex in kept:
        tag = int(tag_hex, 16)
        if tag not in original:
            continue
        compared += 1
        if tag not in result:
            bad.append((tag_hex, "is not in the result"))
            continue
        a, b = original[tag], result[tag]
        same = len(a.value) == len(b.value) if a.VR == "SQ" else a.value == b.value
        if not same:
            bad.append((tag_hex, "changed"))
    return bad, compared


def main(argv):
    with open(argv[0], encoding="utf-8") as f:
        manifest = json.load(f)
    patterns = term_patterns(manifest["terms"])
    failures = compared = 0
    for pair in manifest["files"]:
        name = pair["result"].rsplit("/", 1)[-1]
        original = pydicom.dcmread(pair["original"], force=True)
        result = pydicom.dcmread(pair["result"], force=True)
        for tag_hex, i in standing_terms(result, patterns):
            print("%s: term %d of the lists stands in %s" % (name, i, tag_hex))
            failures += 1
        for tag_hex in cleaned_mismatches(result, pair["cleaned"]):
            print("%s: %s holds other text than the cleaned value" % (name, tag_hex))
            failures += 1
        bad, n = kept_changes(original, result, pair["kept"])
        compared += n
        for tag_hex, what in bad:
            print("%s: %s was kept and %s" % (name, tag_hex, what))
            failures += 1
    print("%d files, %d kept elements compared, %d failures" % (len(manifest["files"]), compared, failures))
    return 1 if failures or compared == 0 else 0
