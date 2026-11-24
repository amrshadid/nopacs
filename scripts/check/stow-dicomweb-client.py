"""Check a tenant's STOW-RS service with dicomweb-client, hashlib and pydicom.

Usage:
  stow-dicomweb-client.py store <DICOMweb root URL> <study UID> <other> <file> [<file> ...]
  stow-dicomweb-client.py compare <storage root> <file> [<file> ...]

The key is read from the environment variable NOPACS_STOW_KEY, never from
the command line, and sent as a bearer token.

store, with dicomweb-client, a client that is not NoPACs':

  * stores every file in one request into the study the UID names
    (DICOMwebClient.store_instances, PS3.18 10.5) and checks the answer's
    Referenced SOP Sequence (0008,1199) lists each file's SOP Class and
    SOP Instance UID once, each with a Retrieve URL (0008,1190) that ends
    with the instance's WADO-RS path, and that no part failed;
  * stores the file <other>, of another study, into the same study and
    checks the request is refused with 409 and that the answer's Failed
    SOP Sequence (0008,1198) lists it with Failure Reason A900H (PS3.18
    Table I.2-2).

compare, once the archive has archived what store sent: for each file, the
bytes dicomweb-client sent (pydicom.dcmwrite of the file read with pydicom,
which is how store_instances encodes a data set) must be archived exactly,
once, under the storage root's data/ tree, with the size and SHA-256 its
scan's manifest (meta/.../manifest.json) lists, checked with hashlib; and
pydicom must read the archived file as the same data set as the file.

Exit status 0 when everything agrees; each difference is printed.
"""
import glob
import hashlib
import io
import json
import os
import sys

import pydicom
import requests
from dicomweb_client.api import DICOMwebClient

REFERENCED = "00081199"
FAILED = "00081198"
CLASS = "00081150"
INSTANCE = "00081155"
URL = "00081190"
REASON = "00081197"
DOES_NOT_MATCH = 0xA900


def value(item, tag):
    """The first value of an element of a DICOM JSON object, or None."""
    v = item.get(tag, {}).get("Value") or [None]
    return v[0]


def sent_bytes(path):
    """The bytes dicomweb-client sends for a file: pydicom's encoding of
    the data set it reads from it."""
    ds = pydicom.dcmread(path)
    b = io.BytesIO()
    pydicom.dcmwrite(b, ds)
    return b.getvalue()


def store(root, study, other, files):
    key = os.environ.get("NOPACS_STOW_KEY", "")
    if not key:
        return ["NOPACS_STOW_KEY is not set"]
    client = DICOMwebClient(url=root, headers={"Authorization": "Bearer " + key})
    errors = []
    datasets = [pydicom.dcmread(f) for f in files]
    answer = client.store_instances(datasets=datasets, study_instance_uid=study)
    answer = answer.to_json_dict() if hasattr(answer, "to_json_dict") else answer
    stored = answer.get(REFERENCED, {}).get("Value", [])
    if answer.get(FAILED, {}).get("Value"):
        errors.append("parts failed: %s" % json.dumps(answer[FAILED]))
    want = {(ds.SOPClassUID, ds.SOPInstanceUID): ds for ds in datasets}
    seen = set()
    for item in stored:
        k = (value(item, CLASS), value(item, INSTANCE))
        ds = want.get(k)
        if ds is None or k in seen:
            errors.append("the answer lists %s, not a file sent or twice" % (k,))
            continue
        seen.add(k)
        path = "/studies/%s/series/%s/instances/%s" % (ds.StudyInstanceUID, ds.SeriesInstanceUID, ds.SOPInstanceUID)
        if not str(value(item, URL) or "").endswith(path):
            errors.append("the Retrieve URL of %s is %r, not its WADO-RS URL" % (ds.SOPInstanceUID, value(item, URL)))
    if len(seen) != len(want):
        errors.append("the answer lists %d of the %d files sent" % (len(seen), len(want)))

    mismatched = pydicom.dcmread(other)
    try:
        client.store_instances(datasets=[mismatched], study_instance_uid=study)
        errors.append("a file of another study was stored into the study")
    except requests.HTTPError as e:
        r = e.response
        body = r.json() if r is not None and r.content else {}
        failed = body.get(FAILED, {}).get("Value", [])
        if r is None or r.status_code != 409 or len(failed) != 1 or value(failed[0], REASON) != DOES_NOT_MATCH or \
                value(failed[0], INSTANCE) != mismatched.SOPInstanceUID:
            errors.append("a file of another study: %s %s, want 409 with Failure Reason A900H" %
                          (getattr(r, "status_code", None), json.dumps(body)))
    print("store: %d stored, %d refused as of another study" % (len(seen), 1))
    return errors


def compare(root, files):
    errors = []
    listed = {}
    for m in glob.glob(os.path.join(root, "meta", "**", "manifest.json"), recursive=True):
        with open(m, "rb") as fh:
            for f in json.load(fh)["files"]:
                listed[f["key"]] = f
    archived = {}
    for p in glob.glob(os.path.join(root, "data", "**", "*.dcm"), recursive=True):
        with open(p, "rb") as fh:
            b = fh.read()
        key = os.path.relpath(p, root).replace(os.sep, "/")
        sha = hashlib.sha256(b).hexdigest()
        entry = listed.get(key)
        if entry is None or entry.get("sha256") != sha or int(entry.get("length", entry.get("size", -1))) != len(b):
            errors.append("%s is not listed by its manifest with its size and SHA-256" % key)
        if sha in archived:
            errors.append("%s is archived twice" % key)
        archived[sha] = p
    for f in files:
        sent = sent_bytes(f)
        sha = hashlib.sha256(sent).hexdigest()
        p = archived.pop(sha, None)
        if p is None:
            errors.append("%s: the bytes sent are not archived" % os.path.basename(f))
            continue
        if pydicom.dcmread(p) != pydicom.dcmread(f):
            errors.append("%s: pydicom reads the archived file as another data set" % os.path.basename(f))
    if archived:
        errors.append("%d archived files were not sent" % len(archived))
    print("compare: %d files archived as sent" % (len(files) - len([e for e in errors if "not archived" in e])))
    return errors


def main(argv):
    if len(argv) >= 6 and argv[1] == "store":
        errors = store(argv[2], argv[3], argv[4], argv[5:])
    elif len(argv) >= 4 and argv[1] == "compare":
        errors = compare(argv[2], argv[3:])
    else:
        print(__doc__, file=sys.stderr)
        return 2
    for e in errors:
        print(e)
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
