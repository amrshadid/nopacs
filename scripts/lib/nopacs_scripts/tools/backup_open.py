"""Open a NoPACs backup file outside NoPACs: with the installation's key file,
Python's hmac and hashlib (HKDF, RFC 5869) and the cryptography package's
AES-256-GCM, checking every chunk, and refusing a changed file; with
--sections, read the plaintext's layout too and check every section's SHA-256.

    backup_open.py BACKUP --key-file KEYFILE [--sections] [--out PLAINTEXT] [--expect-sha256 HEX]
    backup_open.py --self-test

The file is the one internal/secret/doc.go describes (chunked AES-256-GCM, the
nonce a counter with a last-chunk flag, the header in every chunk's
authenticated data), so a file cut at any byte, with chunks swapped, dropped or
added, or with a bit changed, does not open.

The plaintext is the one internal/backup/doc.go describes: lines of UTF-8 JSON,
each ended by one "\\n". A header; then one section per database, the master
first, then each tenant by name, each made of a "section" line, for each table
a "table" line ({"name", "columns": [[name, kind], ...]}) and its rows (one
JSON array a row, one value per column), a "sequences" line and an "end" line
with the section's counts and its recovery point; then a footer listing every
section's index, tenant, tables, rows and SHA-256. A section's SHA-256 covers
its bytes from the first byte of its "section" line to the "\\n" that ends its
"end" line. A value is, by its column's kind: int, an integer; text, a string
(or {"bytes": base64} for text that is not UTF-8); bytes, base64 with padding;
bool, true or false; time, an integer of microseconds since 1970 in UTC;
float, a number; null for NULL. With --sections the tool checks the order of
the lines, each row's count of values and each value against its kind, each
section's counts, each section's SHA-256 against the footer, and that nothing
follows the footer; it prints one line per section and refuses the file at the
first thing that does not hold.

With --out the plaintext is written only after the final chunk opened as the
final one (and, with --sections, after every section checked): it goes to a
staging file beside OUT (mode 0600) that is renamed over OUT at the end and
removed on any error, so a refused file leaves nothing behind, as a restore
must. Without --out nothing is written.

Exit status: 0 and a summary on standard output; 1 and
"backup_open: refused: <reason>" on standard error for a file that does not
open, whose layout or a section's SHA-256 does not hold (with --sections), or
whose plaintext SHA-256 is not --expect-sha256; 2 for a mistake in the command
line or the key file. Nothing of NoPACs' is used.
"""

import argparse
import base64
import binascii
import hashlib
import json
import os
import struct
import sys
import tempfile

from nopacs_scripts.formats.backup import TAG, Refused, open_chunks
from nopacs_scripts.keys import KeyFileError, read_keys

FORMAT_NAME = "nopacs-backup"
FORMAT_VERSION = 1
KINDS = ("int", "text", "bytes", "bool", "time", "float")


def check_value(value, kind):
    """Raise Refused unless value is in the form its column's kind writes."""
    if value is None:
        return
    ok = False
    if kind in ("int", "time"):
        ok = isinstance(value, int) and not isinstance(value, bool) and -(1 << 63) <= value < (1 << 63)
    elif kind == "text":
        if isinstance(value, str):
            ok = True
        elif isinstance(value, dict) and set(value) == {"bytes"} and isinstance(value["bytes"], str):
            try:
                raw = base64.b64decode(value["bytes"], validate=True)
                raw.decode("utf-8")
            except binascii.Error:
                ok = False
            except UnicodeDecodeError:
                ok = True
    elif kind == "bytes":
        if isinstance(value, str):
            try:
                base64.b64decode(value, validate=True)
                ok = True
            except binascii.Error:
                ok = False
    elif kind == "bool":
        ok = isinstance(value, bool)
    elif kind == "float":
        ok = isinstance(value, (int, float)) and not isinstance(value, bool)
    if not ok:
        raise Refused("a value is not in the form of its column's kind %s" % kind)


def obj(line, kind):
    """The line as an object of kind, or Refused."""
    try:
        o = json.loads(line)
    except ValueError:
        raise Refused("a line is not JSON")
    if not isinstance(o, dict) or o.get("kind") != kind:
        raise Refused("a %s line was expected" % kind)
    return o


def parse_sections(plain):
    """Check the plaintext's layout (see the docstring) and return (header,
    sections, footer); each section a dict with index, database, tenant,
    tables, rows, sha256 and point. Raise Refused at the first thing that does
    not hold."""
    if not plain.endswith(b"\n"):
        raise Refused("the plaintext does not end with a line's end")
    lines = plain.split(b"\n")[:-1]
    pos = [0]

    def take():
        if pos[0] >= len(lines):
            raise Refused("the plaintext ends before its footer")
        line = lines[pos[0]]
        pos[0] += 1
        return line

    header = obj(take(), "header")
    if header.get("format") != FORMAT_NAME or header.get("version") != FORMAT_VERSION:
        raise Refused("the plaintext is not a %s of version %d" % (FORMAT_NAME, FORMAT_VERSION))
    count = header.get("sections")
    if not isinstance(count, int) or count < 1:
        raise Refused("the header announces no sections")
    sections = []
    for index in range(count):
        digest = hashlib.sha256()
        first = take()
        digest.update(first + b"\n")
        start = obj(first, "section")
        want = "master" if index == 0 else "tenant"
        if start.get("index") != index or start.get("database") != want:
            raise Refused("the sections are not the master's, then the tenants', in order")
        tables = rows = 0
        columns = None
        while True:
            line = take()
            digest.update(line + b"\n")
            if line.startswith(b"["):
                if columns is None:
                    raise Refused("a row comes before its table")
                try:
                    row = json.loads(line)
                except ValueError:
                    raise Refused("a row is not JSON")
                if not isinstance(row, list) or len(row) != len(columns):
                    raise Refused("a row of a table is not one value per column")
                for value, (_, kind) in zip(row, columns):
                    check_value(value, kind)
                rows += 1
                continue
            try:
                o = json.loads(line)
            except ValueError:
                raise Refused("a line is not JSON")
            kind = o.get("kind") if isinstance(o, dict) else None
            if kind == "table":
                cols = o.get("columns")
                if not o.get("name") or not isinstance(cols, list) or not cols or \
                        any(not isinstance(c, list) or len(c) != 2 or c[1] not in KINDS for c in cols):
                    raise Refused("a table line is not in its form")
                columns = cols
                tables += 1
            elif kind == "sequences":
                if not isinstance(o.get("sequences"), dict):
                    raise Refused("a sequences line is not in its form")
                break
            else:
                raise Refused("a line inside a section is neither a table, a row nor its sequences")
        last = take()
        digest.update(last + b"\n")
        end = obj(last, "end")
        if end.get("index") != index or end.get("tables") != tables or end.get("rows") != rows:
            raise Refused("section %d's end does not count what it holds" % index)
        sections.append({"index": index, "database": start["database"], "tenant": start.get("tenant", ""), "tables": tables,
                         "rows": rows, "sha256": digest.hexdigest(), "point": end.get("point", {})})
    footer = obj(take(), "footer")
    if pos[0] != len(lines):
        raise Refused("something follows the footer")
    listed = footer.get("sections")
    if not isinstance(listed, list) or len(listed) != len(sections):
        raise Refused("the footer lists another number of sections than the plaintext holds")
    for got, f in zip(sections, listed):
        mine = {k: got[k] for k in ("index", "tenant", "tables", "rows", "sha256")}
        if f != mine:
            raise Refused("section %d's SHA-256 or counts are not what the footer says" % got["index"])
    return header, sections, footer


def open_file(keys, data, out=None, expect=None, sections=False):
    """Authenticate every chunk of data; return (plaintext size, SHA-256 hex,
    chunk count, parsed sections or None). With sections the plaintext's
    layout is checked (parse_sections). With out, the plaintext is staged
    beside it and renamed over it after the last chunk (and after the
    sections and the SHA-256 matched expect, when given); on Refused or any
    error nothing is left."""
    digest = hashlib.sha256()
    size = chunks = 0
    stage = None
    kept = [] if sections else None
    parsed = None
    if out:
        fd, stage = tempfile.mkstemp(prefix=os.path.basename(out) + ".", suffix=".partial",
                                     dir=os.path.dirname(os.path.abspath(out)))
        handle = os.fdopen(fd, "wb")
    try:
        for piece in open_chunks(keys, data):
            digest.update(piece)
            size += len(piece)
            chunks += 1
            if out:
                handle.write(piece)
            if kept is not None:
                kept.append(piece)
        if expect and digest.hexdigest() != expect.lower():
            raise Refused("the plaintext's SHA-256 is %s, not the expected %s" % (digest.hexdigest(), expect.lower()))
        if kept is not None:
            parsed = parse_sections(b"".join(kept))
        if out:
            handle.flush()
            os.fsync(handle.fileno())
            handle.close()
            os.replace(stage, out)
            stage = None
    finally:
        if stage is not None:
            handle.close()
            os.unlink(stage)
    return size, digest.hexdigest(), chunks, parsed


def main(argv):
    p = argparse.ArgumentParser(description="Open a NoPACs backup with the installation's key file; "
                                "see this tool's docstring.")
    p.add_argument("backup", nargs="?", help="the backup file")
    p.add_argument("--key-file", help="the installation key file (nopacs-key-v1 lines)")
    p.add_argument("--sections", action="store_true", help="check the plaintext's layout and every section's SHA-256")
    p.add_argument("--out", help="write the plaintext here, after every chunk opened")
    p.add_argument("--expect-sha256", help="hex SHA-256 the plaintext must have")
    p.add_argument("--self-test", action="store_true", help="check the tool against backups it makes and breaks")
    args = p.parse_args(argv)
    if args.self_test:
        from nopacs_scripts.tools import backup_open_selftest
        return backup_open_selftest.run()
    if not args.backup or not args.key_file:
        p.error("a backup and --key-file are required")
    try:
        keys = read_keys(args.key_file)
        with open(args.backup, "rb") as f:
            data = f.read()
    except (KeyFileError, OSError, ValueError) as e:
        print("backup_open: %s" % e, file=sys.stderr)
        return 2
    try:
        size, sha, chunks, parsed = open_file(keys, data, args.out, args.expect_sha256, args.sections)
    except Refused as e:
        print("backup_open: refused: %s" % e, file=sys.stderr)
        return 1
    version, chunk = struct.unpack(">II", data[9:17])
    print("backup_open: ok: %d bytes of ciphertext, key version %d, %d chunks of %d (%d-byte tags), every chunk opened, "
          "the last as the last" % (len(data), version, chunks, chunk, TAG))
    print("plaintext: %d bytes sha256 %s%s" % (size, sha, (" written to " + args.out) if args.out else " (not written)"))
    if parsed:
        header, sections, _ = parsed
        print("installation %s, made at %s" % (header.get("installation"), header.get("time")))
        for s in sections:
            name = "master" if s["database"] == "master" else "tenant " + s["tenant"]
            print("section %d %s: %d tables, %d rows, as of %s, sha256 %s ok" % (
                s["index"], name, s["tables"], s["rows"], s["point"].get("snapshot_time"), s["sha256"]))
        print("sections: %d, every SHA-256 as the footer says" % len(sections))
    return 0

