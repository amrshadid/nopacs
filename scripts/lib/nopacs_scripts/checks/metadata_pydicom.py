"""Check NoPACs' metadata answers against pydicom.

Input: the path of a JSON file, a list of
  {"file": <path of a DICOM file as it was received>,
   "id": <SOP Instance UID>,
   "json": <the DICOM JSON text the Metadata service answered for it>,
   "omitted": <true when the answer left out bulk data (deflated files)>}

For each entry, the answer must equal pydicom's Dataset.to_json_dict() of the
same file, member for member, with these differences only, each checked here:

  * Data Set Trailing Padding (FFFC,FFFC) is not answered (PS3.5 7.1.3).
  * A member the answer holds as a BulkDataURI (PS3.18 F.2.2) must name, by
    its offset and length in the file, exactly the bytes pydicom holds for
    that value: its InlineBinary decoded, or its text.
  * Specific Character Set (0008,0005) is answered as ISO_IR 192 where
    pydicom keeps the file's own: go-dicom decodes every text value into
    UTF-8, which is what a JSON string holds (RFC 8259), and says so. The
    values themselves must still equal pydicom's, which are decoded too.
  * A member left out because the file is deflated (omitted) must be one
    pydicom holds as a value over 1 KiB (binary) or over 64 KiB.

Exit status 0 when every entry agrees; the differences are printed otherwise.
"""
import json
import re
from urllib.parse import parse_qs, urlsplit

import pydicom

from nopacs_scripts.dicom import their_bytes

BINARY = {"OB", "OD", "OF", "OL", "OV", "OW", "UN"}
URI = re.compile(r"^/tenants/[^/]+/instances/[^/]+/[^/]+/[^/]+/[^/?]+\?offset=\d+&length=\d+$")


def bulk_bytes(ref, data):
    if not URI.match(ref):
        return None
    q = parse_qs(urlsplit(ref).query)
    off, length = int(q["offset"][0]), int(q["length"][0])
    if off + length > len(data):
        return None
    return data[off:off + length]


def compare(ours, theirs, data, omitted, where, problems):
    theirs = {k: v for k, v in theirs.items() if not (where == "" and k == "FFFCFFFC")}
    for k in sorted(set(ours) | set(theirs)):
        at = where + k
        if k not in ours:
            if omitted and theirs[k]["vr"] in BINARY | {"UT", "LT", "ST", "UC", "UR"}:
                continue
            problems.append(f"{at}: pydicom has it, the answer does not")
            continue
        if k not in theirs:
            problems.append(f"{at}: the answer has it, pydicom does not")
            continue
        o, t = ours[k], theirs[k]
        if o.get("vr") != t.get("vr"):
            problems.append(f"{at}: vr {o.get('vr')} against {t.get('vr')}")
            continue
        if "BulkDataURI" in o:
            got = bulk_bytes(o["BulkDataURI"], data)
            if got is None:
                problems.append(f"{at}: BulkDataURI {o['BulkDataURI']!r} is not well formed or lies outside the file")
                continue
            want = their_bytes(t)
            if want is None:
                problems.append(f"{at}: pydicom holds no single value to compare the bytes at the URI with")
            elif k == "7FE00010" and t["vr"] in BINARY and got.startswith(b"\xfe\xff\x00\xe0"):
                # Encapsulated: pydicom holds the encoded value; the bytes at
                # the URI are those and the delimiter, as the file has them.
                if not got.startswith(want.rstrip(b"\x00")[:64]) or len(got) < len(want):
                    problems.append(f"{at}: the encapsulated bytes at the URI do not begin as pydicom's value does")
            elif got.rstrip(b"\x00 ") != want.rstrip(b"\x00 "):
                problems.append(f"{at}: the {len(got)} bytes at the URI are not pydicom's {len(want)}")
            continue
        if t["vr"] == "SQ":
            oi, ti = o.get("Value", []), t.get("Value", [])
            if len(oi) != len(ti):
                problems.append(f"{at}: {len(oi)} items against {len(ti)}")
                continue
            for i, (a, b) in enumerate(zip(oi, ti)):
                compare(a, b, data, omitted, f"{at}/{i}/", problems)
            continue
        if k == "00080005" and where == "" and o.get("Value") == ["ISO_IR 192"] and t.get("Value"):
            continue
        if o != t:
            problems.append(f"{at}: {json.dumps(o)[:120]} against {json.dumps(t)[:120]}")


def main(argv):
    with open(argv[0]) as f:
        entries = json.load(f)
    bad = 0
    for e in entries:
        with open(e["file"], "rb") as f:
            data = f.read()
        theirs = pydicom.dcmread(e["file"], force=True).to_json_dict()
        problems = []
        compare(json.loads(e["json"]), theirs, data, e.get("omitted", False), "", problems)
        status = "ok" if not problems else "DIFFERS"
        print(f"{status} {e['id']} ({len(json.loads(e['json']))} members, pydicom {pydicom.__version__})")
        for p in problems[:20]:
            print("    " + p)
        bad += bool(problems)
    print(f"{len(entries)} instances, {bad} differ")
    return 1 if bad else 0

