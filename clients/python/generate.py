#!/usr/bin/env python3
"""Make the Python code of the API contract, or check that it is current.

The contract is the proto files in api/proto. This script compiles them with
the protoc that grpcio-tools bundles (the version pinned in
scripts/requirements/proto.txt) and writes the modules into src/nopacs/v1,
where the proto package nopacs.v1 puts them: the files are
src/nopacs/v1/<file>_pb2.py, <file>_pb2.pyi and <file>_pb2_grpc.py, they are
checked in and never edited by hand, and protoc's own imports
(`from nopacs.v1 import x_pb2`) are right as written. Nothing is rewritten, so
the output is what the compiler wrote. src/nopacs/v1/__init__.py is written by
hand and is left alone.

Usage:
  generate.py            write the generated files again
  generate.py --check    exit 1 when they differ from a fresh generation,
                         listing the files that differ

The output depends on the compiler's version: run it with the pinned one.
"""

import importlib.resources
import pathlib
import shutil
import sys
import tempfile

from grpc_tools import protoc

HERE = pathlib.Path(__file__).resolve().parent
PROTO_ROOT = HERE.parent.parent / "api" / "proto"
GEN = HERE / "src" / "nopacs" / "v1"
# What protoc writes for a proto file, as opposed to the hand-written
# __init__.py beside it.
GENERATED = ("*_pb2.py", "*_pb2.pyi", "*_pb2_grpc.py")


def generate(out: pathlib.Path) -> None:
    """Compile every proto file into out, which gets nopacs/v1/ under it."""
    protos = sorted(str(p.relative_to(PROTO_ROOT)) for p in PROTO_ROOT.rglob("*.proto"))
    if not protos:
        raise SystemExit(f"no proto files under {PROTO_ROOT}")
    # grpcio-tools carries the well-known types (timestamp, duration).
    wkt = importlib.resources.files("grpc_tools") / "_proto"
    args = [
        "grpc_tools.protoc",
        f"-I{PROTO_ROOT}",
        f"-I{wkt}",
        f"--python_out={out}",
        f"--pyi_out={out}",
        f"--grpc_python_out={out}",
        *protos,
    ]
    if protoc.main(args) != 0:
        raise SystemExit("grpc_tools.protoc failed")


def tree(root: pathlib.Path) -> dict:
    """Answer each generated file directly in root, by name, with its bytes."""
    return {
        p.name: p.read_bytes()
        for pattern in GENERATED
        for p in sorted(root.glob(pattern))
    }


def main() -> int:
    check = sys.argv[1:] == ["--check"]
    if sys.argv[1:] and not check:
        print(__doc__, file=sys.stderr)
        return 2
    with tempfile.TemporaryDirectory() as tmp:
        fresh = pathlib.Path(tmp)
        generate(fresh)
        if check:
            have, want = tree(GEN), tree(fresh / "nopacs" / "v1")
            bad = sorted(k for k in have.keys() | want.keys() if have.get(k) != want.get(k))
            for k in bad:
                print(f"differs from a fresh generation: {k}", file=sys.stderr)
            if bad:
                print("run clients/python/generate.py and commit the result", file=sys.stderr)
                return 1
            print(f"src/nopacs/v1 equals a fresh generation ({len(want)} files)")
            return 0
        for old in tree(GEN):
            (GEN / old).unlink()
        for name in tree(fresh / "nopacs" / "v1"):
            shutil.copy(fresh / "nopacs" / "v1" / name, GEN / name)
        print(f"wrote {len(tree(GEN))} files under {GEN}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
