"""The self-test of backup_open.py: it makes backups with a key file written
here, checks they open to the same plaintext, then changes them in each way
the tool claims to refuse and checks that nothing is left staged or written.
Run by `backup_open.py --self-test`.
"""

import base64
import hashlib
import json
import os
import tempfile

from nopacs_scripts.formats.backup import HEADER, TAG, Refused, make_backup
from nopacs_scripts.keys import read_keys
from nopacs_scripts.report import Failure
from nopacs_scripts.tools import backup_open as bo


def write_key_file(path, versions):
    with open(path, "w", encoding="ascii") as f:
        for v in versions:
            f.write("nopacs-key-v1 %d %s\n" % (v, base64.b64encode(os.urandom(32)).decode()))


def run():
    with tempfile.TemporaryDirectory() as tmp:
        key_path = os.path.join(tmp, "nopacs.key")
        write_key_file(key_path, [1, 2])
        other_path = os.path.join(tmp, "other.key")
        write_key_file(other_path, [1, 2])
        keys, other = read_keys(key_path), read_keys(other_path)
        size = 64
        plain = os.urandom(300)
        data = make_backup(keys, 2, size, plain)
        span = size + TAG
        out = os.path.join(tmp, "plain.bin")

        s, sha, chunks, _ = bo.open_file(keys, data, out)
        assert (s, sha, chunks) == (300, hashlib.sha256(plain).hexdigest(), 5), (s, sha, chunks)
        assert open(out, "rb").read() == plain
        print("self-test: %-34s -> ok (5 chunks, plaintext equal)" % "intact backup")
        os.unlink(out)
        for n in (0, size, 2 * size, 5 * size):
            p = os.urandom(n)
            got = bo.open_file(keys, make_backup(keys, 1, size, p))
            assert got[0] == n and got[1] == hashlib.sha256(p).hexdigest()
        print("self-test: %-34s -> ok (empty, 1, 2 and 5 full chunks)" % "chunk boundaries")

        def refused(name, body, key_set=keys, **kw):
            try:
                bo.open_file(key_set, body, out, **kw)
            except Refused as e:
                left = os.listdir(tmp)
                assert "plain.bin" not in left and not [x for x in left if x.endswith(".partial")], left
                print("self-test: %-34s -> refused: %s" % (name, e))
                return
            raise Failure("self-test %s: the file opened" % name)

        c0, c1 = data[HEADER:HEADER + span], data[HEADER + span:HEADER + 2 * span]
        refused("cut after chunk 0", data[:HEADER + span])
        refused("cut at the last byte", data[:-1])
        refused("header only", data[:HEADER])
        refused("chunks 0 and 1 swapped", data[:HEADER] + c1 + c0 + data[HEADER + 2 * span:])
        refused("chunk 1 dropped", data[:HEADER] + c0 + data[HEADER + 2 * span:])
        flipped = bytearray(data)
        flipped[HEADER + span + 3] ^= 1
        refused("a bit flipped in chunk 1", bytes(flipped))
        refused("chunk 0 appended", data + c0)
        refused("an empty chunk's tag appended", data + b"\x00" * TAG)
        hdr = bytearray(data)
        hdr[10] ^= 1
        refused("key version in the header changed", bytes(hdr))
        salt = bytearray(data)
        salt[20] ^= 1
        refused("salt changed", bytes(salt))
        refused("a different key file", data, other)
        refused("not a backup", b"x" * 100)
        refused("plaintext hash not the expected", data, expect="00" * 32)

        path = os.path.join(tmp, "backup.bin")
        with open(path, "wb") as f:
            f.write(data)
        assert bo.main([path, "--key-file", key_path, "--out", out, "--expect-sha256", hashlib.sha256(plain).hexdigest()]) == 0
        assert open(out, "rb").read() == plain
        os.unlink(out)
        with open(path, "wb") as f:
            f.write(data[:-1])
        assert bo.main([path, "--key-file", key_path, "--out", out]) == 1 and not os.path.exists(out)
        assert bo.main([path, "--key-file", os.path.join(tmp, "missing")]) == 2
        print("self-test: command line exits 0, 1 (nothing written) and 2")
        sections_test(keys, key_path, tmp)
    print("backup_open self-test: all passed")
    return 0


def make_plain(rows, footer_from=None, extra=b""):
    """A plaintext of the layout bo.parse_sections reads: the master with a
    table of the given rows, and one tenant with an empty table. The footer
    is computed from footer_from's rows when given, so a test can make a
    footer that does not match what the sections hold."""
    def build(table_rows):
        out = [json.dumps({"kind": "header", "format": "nopacs-backup", "version": 1, "installation": "i1",
                           "time": "2026-11-22T00:00:00Z", "key_version": 1, "sections": 2}).encode() + b"\n"]
        listed = []
        for index, (database, tenant, body) in enumerate((("master", "", table_rows), ("tenant", "lab", []))):
            sec = [json.dumps({"kind": "section", "index": index, "database": database, "tenant": tenant,
                               "tenant_id": "T1" if tenant else "", "driver": "sqlite" if tenant else ""}).encode()]
            sec.append(json.dumps({"kind": "table", "name": "t", "columns": [["id", "text"], ["n", "int"], ["b", "bytes"],
                                                                         ["on", "bool"], ["at", "time"], ["x", "float"]]}).encode())
            sec += [json.dumps(r).encode() for r in body]
            sec.append(b'{"kind":"sequences","sequences":{}}')
            sec.append(json.dumps({"kind": "end", "index": index, "tables": 1, "rows": len(body), "point": {}}).encode())
            raw = b"".join(x + b"\n" for x in sec)
            listed.append({"index": index, "tenant": tenant, "tables": 1, "rows": len(body),
                           "sha256": hashlib.sha256(raw).hexdigest()})
            out.append(raw)
        return out, listed
    out, listed = build(rows)
    if footer_from is not None:
        _, listed = build(footer_from)
    out.append(json.dumps({"kind": "footer", "sections": listed}).encode() + b"\n")
    return b"".join(out) + extra


def sections_test(keys, key_path, tmp):
    """The layout's check: a whole plaintext is read, with each section's
    SHA-256; a row changed after its footer was written, a value not of its
    column's kind, a row of the wrong length, a line after the footer and a
    footer missing are refused."""
    good = [["a", 1, base64.b64encode(b"\x00\x01").decode(), True, 1763769600000000, 1.5],
            [{"bytes": base64.b64encode(b"\xff\xfe").decode()}, None, None, None, None, None]]
    s = bo.parse_sections(make_plain(good))
    assert len(s[1]) == 2 and s[1][0]["rows"] == 2 and s[1][1]["tenant"] == "lab", s
    data = make_backup(keys, 1, 64, make_plain(good))
    got = bo.open_file(keys, data, sections=True)
    assert got[3] is not None and len(got[3][1]) == 2
    print("self-test: %-34s -> ok (2 sections, every SHA-256 as the footer)" % "sections of a whole plaintext")
    changed = [list(good[0]), good[1]]
    changed[0][1] = 2

    def refused(name, plain):
        try:
            bo.parse_sections(plain)
        except Refused as e:
            print("self-test: %-34s -> refused: %s" % (name, e))
            return
        raise Failure("self-test %s: the plaintext was taken" % name)

    refused("a row changed after its footer", make_plain(changed, footer_from=good))
    refused("an int written as text", make_plain([["a", "1", None, None, None, None]]))
    refused("UTF-8 text written as bytes", make_plain([[{"bytes": base64.b64encode(b"ok").decode()}, 1, None, None, None, None]]))
    refused("a row one value short", make_plain([["a", 1, None, None, None]]))
    refused("a line after the footer", make_plain(good, extra=b'{"kind":"footer","sections":[]}\n'))
    refused("the footer missing", make_plain(good).rsplit(b"\n", 2)[0] + b"\n")
    path = os.path.join(tmp, "sections.bin")
    with open(path, "wb") as f:
        f.write(make_backup(keys, 1, 64, make_plain(changed, footer_from=good)))
    assert bo.main([path, "--key-file", key_path, "--sections"]) == 1
    print("self-test: command line refuses a section whose SHA-256 is not the footer's")
