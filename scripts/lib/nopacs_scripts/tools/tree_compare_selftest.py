"""The self-test of tree_compare.py: two directory trees are made equal, then
changed one way at a time (an object only on one side, a byte flipped with the
size kept, a size changed, an ignored pattern), a bucket is stood in for by a
small object with the two calls the tool makes (ListObjectsV2 in two pages,
GetObject), and each difference must be named and the summary counted right.
Run by `tree_compare.py --self-test`.
"""

import collections
import io
import json
import os
import shutil
import tempfile

from nopacs_scripts.proof import Reporting
from nopacs_scripts.tools import tree_compare as tc


class FakeBucket:
    """The two calls scan_s3 makes, over a dictionary of keys to bytes,
    listing two keys a page so the continuation is used."""

    def __init__(self, objects):
        self.objects = dict(objects)

    def list_objects_v2(self, Bucket, Prefix, ContinuationToken=None):
        keys = sorted(k for k in self.objects if k.startswith(Prefix))
        start = int(ContinuationToken or 0)
        page = keys[start:start + 2]
        out = {"Contents": [{"Key": k} for k in page], "IsTruncated": start + 2 < len(keys)}
        if out["IsTruncated"]:
            out["NextContinuationToken"] = str(start + 2)
        return out

    def get_object(self, Bucket, Key):
        return {"Body": io.BytesIO(self.objects[Key])}


def write(root, files):
    """Writes {relative path: bytes} under root."""
    for rel, data in files.items():
        path = os.path.join(root, *rel.split("/"))
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as f:
            f.write(data)


def run():
    """Runs every check; 0 when all hold, 1 otherwise."""
    t = Reporting()
    t.counts = collections.Counter()
    base = {"data/a.dcm": b"A" * 1000, "data/sub/b.dcm": b"B" * 10, "meta/m.json": b"{}", "empty": b""}
    tmp = tempfile.mkdtemp(prefix="tree-compare-")
    try:
        a, b = os.path.join(tmp, "a"), os.path.join(tmp, "b")
        write(a, base)
        write(b, base)
        r = tc.compare(tc.scan_dir(a), tc.scan_dir(b))
        t.result("equal trees (nested folders and an empty file) compare equal and count objects and bytes",
                 tc.equal(r) and r["same"] == 4 and r["bytes"] == 1012, "same %d, bytes %d" % (r["same"], r["bytes"]))
        t.result("the summary line of equal trees says so", tc.lines(r)[-1].startswith("tree_compare: A and B are equal: 4 objects, 1012 bytes"), tc.lines(r)[-1])

        write(a, {"data/only-a.dcm": b"x"})
        write(b, {"data/sub/only-b.dcm": b"y"})
        r = tc.compare(tc.scan_dir(a), tc.scan_dir(b))
        t.result("an object on one side only is named for that side",
                 r["only_a"] == ["data/only-a.dcm"] and r["only_b"] == ["data/sub/only-b.dcm"] and not r["different"], json.dumps(r))
        os.unlink(os.path.join(a, "data", "only-a.dcm"))
        os.unlink(os.path.join(b, "data", "sub", "only-b.dcm"))

        flipped = bytearray(base["data/a.dcm"])
        flipped[500] ^= 1
        write(b, {"data/a.dcm": bytes(flipped)})
        r = tc.compare(tc.scan_dir(a), tc.scan_dir(b))
        t.result("a flipped byte at the same size is different, with both digests",
                 [d["key"] for d in r["different"]] == ["data/a.dcm"] and r["different"][0]["a"][0] == r["different"][0]["b"][0] == 1000
                 and r["different"][0]["a"][1] != r["different"][0]["b"][1] and r["same"] == 3, json.dumps(r))
        write(b, {"data/a.dcm": base["data/a.dcm"], "meta/m.json": b"{ }"})
        r = tc.compare(tc.scan_dir(a), tc.scan_dir(b))
        t.result("a changed size is different", [d["key"] for d in r["different"]] == ["meta/m.json"], json.dumps(r))
        t.result("a difference is a line of its own and the summary counts each kind",
                 tc.lines(r)[0].startswith("different: meta/m.json (A 2 bytes sha256 ") and "0 only in A, 0 only in B, 1 with other bytes, 3 equal" in tc.lines(r)[-1],
                 "; ".join(tc.lines(r)))

        r = tc.compare(tc.scan_dir(a, ["meta/*"]), tc.scan_dir(b, ["meta/*"]))
        t.result("an ignored pattern drops its keys from both sides", tc.equal(r) and r["same"] == 3, json.dumps(r))
        r = tc.compare(tc.scan_dir(a, ["*.json", "empty"]), tc.scan_dir(b, ["*.json", "empty"]))
        t.result("a pattern with a wildcard crosses folders, and several patterns work together", tc.equal(r) and r["same"] == 2, json.dumps(r))

        write(b, {"meta/m.json": base["meta/m.json"]})
        bucket = FakeBucket({"pre/" + k: v for k, v in base.items() if v} | {"pre2/other": b"z", "pre/dir/": b""})
        sb = tc.scan_s3(bucket, "bkt", "pre")
        t.result("a bucket's keys under a prefix are listed across pages, the folder marker and other prefixes left out",
                 sorted(sb) == sorted(k for k, v in base.items() if v), "keys %s" % sorted(sb))
        r = tc.compare(tc.scan_dir(a, ["empty"]), sb)
        t.result("a directory equals a bucket prefix of the same bytes", tc.equal(r) and r["same"] == 3, json.dumps(r))
        bucket.objects["pre/data/sub/b.dcm"] = b"C" * 10
        r = tc.compare(tc.scan_dir(a, ["empty"]), tc.scan_s3(bucket, "bkt", "pre"))
        t.result("a changed object of the bucket is different from the directory's", [d["key"] for d in r["different"]] == ["data/sub/b.dcm"], json.dumps(r))

        status = tc.main([a, b, "--json"])
        t.result("the command exits 0 on equal trees", status == 0, "exit %d" % status)
        write(b, {"x": b"1"})
        status = tc.main([a, b])
        t.result("the command exits 1 on trees that differ", status == 1, "exit %d" % status)
        try:
            tc.main([os.path.join(tmp, "missing"), b])
            failed = False
        except BaseException as e:  # noqa: BLE001 - Failure is not an Exception
            failed = type(e).__name__ == "Failure"
        t.result("a tree that is not a directory is refused", failed, "raised Failure: %s" % failed)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    print("tree_compare self-test: %d ok, %d FAIL" % (t.counts["ok"], t.counts["FAIL"]))
    return 0 if t.counts["FAIL"] == 0 else 1
