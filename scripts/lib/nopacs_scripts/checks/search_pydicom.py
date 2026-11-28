"""Check NoPACs' search answers against a reference filter over the files.

Input: the path of a JSON file
  {"files": [<paths of the DICOM files as they were received>],
   "queries": [{"level": "scan" | "session" | "subject",
                "query": <the condition text>,
                "keys": [<the rows the search answered, by key>],
                "count": <the total the search counted> (absent for none),
                "facet": <a field> (absent for none),
                "facet_counts": {value: rows} (with a facet)}]}

A key names a scan by its Series Instance UID, a session by its Study
Instance UID and a subject by its sessions' Study Instance UIDs, sorted and
joined with ",". Every file is read with pydicom and every query is parsed and
evaluated by nopacs_scripts.searchref, which is written from the condition
language's grammar and the standard and shares nothing with the archive; what
the search answered must equal what it computes: the same rows, the same
total, the same facet.

Exit status 0 when everything agrees; the differences are printed otherwise.
"""
import json

from nopacs_scripts import searchref


def main(argv):
    with open(argv[0]) as f:
        got = json.load(f)
    rows = searchref.build_rows(got["files"])
    problems = []
    answered = 0
    for q in got["queries"]:
        level, text = q["level"], q["query"]
        what = "%s search %r" % (level, text)
        matched = searchref.answer(rows, level, text)
        want = sorted(searchref.key_of(level, r) for r in matched)
        have = sorted(q["keys"])
        if have != want:
            problems.append("%s: the search answers %d rows %s, the reference finds %d rows %s" % (
                what, len(have), have, len(want), want))
        if "count" in q and q["count"] != len(want):
            problems.append("%s: the search counts %r, the reference %d" % (what, q["count"], len(want)))
        if q.get("facet"):
            reference = searchref.facet(matched, q["facet"])
            if q["facet_counts"] != reference:
                problems.append("%s: the facet of %s is %r, the reference %r" % (what, q["facet"], q["facet_counts"], reference))
        print("%-7s %3d rows  %s" % (level, len(want), text or "(every row)"))
        if want:
            answered += 1
    for p in problems:
        print("DIFFER", p)
    print("%d files, %d scans, %d sessions, %d subjects; %d queries (%d with rows) checked; %d differ" % (
        len(got["files"]), len(rows["scan"]), len(rows["session"]), len(rows["subject"]), len(got["queries"]), answered, len(problems)))
    return 1 if problems else 0
