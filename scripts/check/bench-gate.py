#!/usr/bin/env python3
"""Fails when reception got slower than on a base commit.

It runs one benchmark, files a second from landing to verified for a
reception of small slices on SQLite and a directory, on the base commit's
tree and on the working tree, in turns (base, head, base, head, ...), so
both see the same machine at the same moments; a CI runner's speed differs
from run to run far more than from minute to minute. It compares the
medians and exits 1 when the working tree's is below the base's by more
than the margin. When the base has no such benchmark (it predates it), it
says so and passes.

  bench-gate.py --base REF [--go GO] [--count N] [--margin PERCENT]

Needs git and Go.
"""

import argparse
import os
import re
import shutil
import statistics
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
PACKAGE = "./internal/receive"
BENCH = "^BenchmarkLandToVerified$/^sqlite-dir$/^slices-256KiB$"
LINE = re.compile(r"^BenchmarkLandToVerified/sqlite-dir/slices-256KiB\S*\s+\d+\s+.*?([\d.]+) files/s", re.M)


def run(tree, go, binary):
    """One run of the benchmark in tree, by its compiled test binary;
    returns files a second, or None when tree has no such benchmark."""
    p = subprocess.run([binary, "-test.run", "^$", "-test.bench", BENCH, "-test.benchtime", "2x"],
                       cwd=os.path.join(tree, PACKAGE), capture_output=True, text=True, timeout=1800)
    m = LINE.search(p.stdout)
    if p.returncode != 0:
        raise SystemExit("bench-gate: the benchmark failed in %s:\n%s%s" % (tree, p.stdout[-2000:], p.stderr[-2000:]))
    return float(m.group(1)) if m else None


def build(tree, go, out):
    """Compiles tree's test binary of the package once, so every turn
    measures the same binary and no turn pays for a build."""
    p = subprocess.run([go, "test", "-c", "-o", out, PACKAGE], cwd=tree, capture_output=True, text=True, timeout=1800)
    if p.returncode != 0:
        raise SystemExit("bench-gate: building the tests in %s failed:\n%s" % (tree, p.stderr[-2000:]))


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--base", required=True, help="the commit to compare with, such as origin/develop")
    ap.add_argument("--go", default="go")
    ap.add_argument("--count", type=int, default=5)
    ap.add_argument("--margin", type=float, default=25.0, help="the slowdown, in percent, that fails")
    args = ap.parse_args()

    work = tempfile.mkdtemp(prefix="nopacs-bench-gate.")
    try:
        compare(args, work)
    finally:
        shutil.rmtree(work, ignore_errors=True)


def compare(args, work):
    base = os.path.join(work, "base")
    os.makedirs(base)
    archive = subprocess.run(["git", "archive", "--format=tar", args.base], cwd=ROOT, capture_output=True, check=True)
    subprocess.run(["tar", "-x", "-C", base], input=archive.stdout, check=True)
    if not os.path.isdir(os.path.join(base, PACKAGE)):
        print("bench-gate: %s has no package %s; nothing to compare with, so it passes" % (args.base, PACKAGE))
        return
    bins = {"base": os.path.join(work, "base.test"), "head": os.path.join(work, "head.test")}
    build(base, args.go, bins["base"])
    build(ROOT, args.go, bins["head"])

    got = {"base": [], "head": []}
    for i in range(args.count):
        for name, tree in (("base", base), ("head", ROOT)):
            v = run(tree, args.go, bins[name])
            if v is None:
                if name == "base":
                    print("bench-gate: %s has no benchmark %s; nothing to compare with, so it passes" % (args.base, BENCH))
                    return
                raise SystemExit("bench-gate: the working tree has no benchmark %s" % BENCH)
            got[name].append(v)
            print("bench-gate: turn %d, %s: %.2f files/s" % (i + 1, name, v), flush=True)

    b, h = statistics.median(got["base"]), statistics.median(got["head"])
    change = (h - b) / b * 100
    print("bench-gate: median files/s, base %s %.2f, working tree %.2f: %+.1f%% (fails below -%.0f%%)"
          % (args.base, b, h, change, args.margin))
    if change < -args.margin:
        print("bench-gate: reception is slower than on %s by more than the margin" % args.base)
        sys.exit(1)


if __name__ == "__main__":
    main()
