"""Compare two storage trees object by object, from outside NoPACs: by key,
size and SHA-256 (computed here, by reading every object).

    tree_compare.py A B [--ignore PATTERN ...] [--json] [S3 options]
    tree_compare.py --self-test

A and B are each a directory, or s3://BUCKET/PREFIX for a bucket's objects
under a prefix (read with boto3, the current version of each key). A key is its
path under the directory, or under the prefix, with "/" separators, so a
directory and a bucket compare when they hold the same layout. --ignore is a
shell pattern (fnmatch) matched against the whole key; it is repeatable and
drops what matches from both sides, for the files a replica is not meant to
copy.

It prints each difference, one per line ("only in A: KEY", "only in B: KEY",
"different: KEY (A 12 bytes sha256 ab..., B 12 bytes sha256 cd...)"), then
one summary line. With --json the data is printed instead: {"only_a": [...],
"only_b": [...], "different": [{"key", "a", "b"}], "same": N, "bytes": N}.

S3 options (one server for both sides that name a bucket): --s3-endpoint URL,
--s3-access-key-id ID, --s3-secret-env VARIABLE (the variable that holds the
secret key, so it is not on a command line), --s3-region (default
us-east-1). Path-style addressing is used.

Exit status: 0 when the trees are equal; 1 when they differ; 2 for a mistake
in the command line or a tree that cannot be read. Nothing of NoPACs' is used.
"""

import argparse
import fnmatch
import hashlib
import json
import os
import sys

from nopacs_scripts.report import Failure

CHUNK = 1 << 20


def ignored(key, patterns):
    """True when key matches one of the shell patterns."""
    return any(fnmatch.fnmatchcase(key, p) for p in patterns)


def scan_dir(root, ignore=()):
    """{key: (size, SHA-256 hex)} of every file under the directory root,
    read to its end; a key is the path under root with "/" separators."""
    if not os.path.isdir(root):
        raise Failure("tree_compare: %s is not a directory" % root)
    out = {}
    for d, _, names in os.walk(root):
        for n in names:
            path = os.path.join(d, n)
            key = os.path.relpath(path, root).replace(os.sep, "/")
            if ignored(key, ignore):
                continue
            digest, size = hashlib.sha256(), 0
            with open(path, "rb") as f:
                while True:
                    piece = f.read(CHUNK)
                    if not piece:
                        break
                    digest.update(piece)
                    size += len(piece)
            out[key] = (size, digest.hexdigest())
    return out


def scan_s3(client, bucket, prefix="", ignore=()):
    """{key: (size, SHA-256 hex)} of every current object of the bucket
    under prefix (listed with ListObjectsV2, each read with GetObject); a key
    is the name after the prefix."""
    if prefix and not prefix.endswith("/"):
        prefix += "/"
    out = {}
    token = None
    while True:
        kw = {"Bucket": bucket, "Prefix": prefix}
        if token:
            kw["ContinuationToken"] = token
        page = client.list_objects_v2(**kw)
        for o in page.get("Contents", []):
            key = o["Key"][len(prefix):]
            if not key or key.endswith("/") or ignored(key, ignore):
                continue
            body = client.get_object(Bucket=bucket, Key=o["Key"])["Body"]
            digest, size = hashlib.sha256(), 0
            while True:
                piece = body.read(CHUNK)
                if not piece:
                    break
                digest.update(piece)
                size += len(piece)
            out[key] = (size, digest.hexdigest())
        if not page.get("IsTruncated"):
            return out
        token = page.get("NextContinuationToken")


def compare(a, b):
    """Compares two {key: (size, sha256)} maps: {"only_a": [keys], "only_b":
    [keys], "different": [{"key", "a", "b"}], "same": N, "bytes": N} with every
    list sorted; bytes counts the equal objects' sizes."""
    both = sorted(set(a) & set(b))
    different = [{"key": k, "a": list(a[k]), "b": list(b[k])} for k in both if a[k] != b[k]]
    return {"only_a": sorted(set(a) - set(b)), "only_b": sorted(set(b) - set(a)), "different": different,
            "same": len(both) - len(different), "bytes": sum(a[k][0] for k in both if a[k] == b[k])}


def equal(result):
    """True when the comparison found no difference."""
    return not (result["only_a"] or result["only_b"] or result["different"])


def lines(result, name_a="A", name_b="B"):
    """The text lines that report a comparison: each difference, then the
    summary."""
    out = ["only in %s: %s" % (name_a, k) for k in result["only_a"]]
    out += ["only in %s: %s" % (name_b, k) for k in result["only_b"]]
    for d in result["different"]:
        out.append("different: %s (%s %d bytes sha256 %s, %s %d bytes sha256 %s)" % (d["key"], name_a, d["a"][0], d["a"][1], name_b, d["b"][0], d["b"][1]))
    if equal(result):
        out.append("tree_compare: %s and %s are equal: %d objects, %d bytes, every SHA-256 the same" % (name_a, name_b, result["same"], result["bytes"]))
    else:
        out.append("tree_compare: %s and %s differ: %d only in %s, %d only in %s, %d with other bytes, %d equal" % (
            name_a, name_b, len(result["only_a"]), name_a, len(result["only_b"]), name_b, len(result["different"]), result["same"]))
    return out


def s3_client(endpoint, key_id, secret, region="us-east-1"):
    """A boto3 client with path-style addressing on the endpoint given."""
    try:
        import boto3
        from botocore.config import Config
    except ImportError:
        raise Failure("tree_compare: reading a bucket needs boto3, which this Python lacks (scripts/requirements/prove-whole.txt)")
    return boto3.client("s3", endpoint_url=endpoint, aws_access_key_id=key_id, aws_secret_access_key=secret, region_name=region,
                        config=Config(s3={"addressing_style": "path"}, retries={"max_attempts": 2}))


def scan(spec, ignore, client=None):
    """The scan of one side: a directory path, or s3://BUCKET/PREFIX read with
    client."""
    if spec.startswith("s3://"):
        if client is None:
            raise Failure("tree_compare: %s names a bucket; give --s3-endpoint, --s3-access-key-id and --s3-secret-env" % spec)
        bucket, _, prefix = spec[len("s3://"):].partition("/")
        return scan_s3(client, bucket, prefix, ignore)
    return scan_dir(spec, ignore)


def main(argv):
    p = argparse.ArgumentParser(description="Compare two storage trees by key, size and SHA-256; see this tool's docstring.")
    p.add_argument("a", nargs="?", help="a directory, or s3://BUCKET/PREFIX")
    p.add_argument("b", nargs="?", help="a directory, or s3://BUCKET/PREFIX")
    p.add_argument("--ignore", action="append", default=[], metavar="PATTERN", help="a shell pattern of keys to leave out of both sides; repeatable")
    p.add_argument("--json", action="store_true", help="print the comparison as JSON")
    p.add_argument("--s3-endpoint", help="the S3 server's URL")
    p.add_argument("--s3-access-key-id", help="the access key id")
    p.add_argument("--s3-secret-env", help="the environment variable that holds the secret key")
    p.add_argument("--s3-region", default="us-east-1")
    p.add_argument("--self-test", action="store_true", help="check the tool against trees it makes and changes")
    args = p.parse_args(argv)
    if args.self_test:
        from nopacs_scripts.tools import tree_compare_selftest
        return tree_compare_selftest.run()
    if not args.a or not args.b:
        p.error("two trees are required")
    client = None
    if args.s3_endpoint:
        secret = os.environ.get(args.s3_secret_env or "")
        if not args.s3_access_key_id or not secret:
            p.error("--s3-endpoint needs --s3-access-key-id and --s3-secret-env naming a variable that is set")
        client = s3_client(args.s3_endpoint, args.s3_access_key_id, secret, args.s3_region)
    try:
        result = compare(scan(args.a, args.ignore, client), scan(args.b, args.ignore, client))
    except OSError as e:
        print("tree_compare: %s" % e, file=sys.stderr)
        return 2
    if args.json:
        print(json.dumps(result, sort_keys=True))
    else:
        for line in lines(result, args.a, args.b):
            print(line)
    return 0 if equal(result) else 1
