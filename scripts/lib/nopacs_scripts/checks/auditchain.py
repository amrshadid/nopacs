"""Check NoPACs' audit chain against Python's hashlib and openssl.

A chain that only the code that made it can recompute proves nothing about
its format. This script implements the entry encoding, the chain and the
checkpoint's signed text from their description in
internal/core/auditchain/doc.go, with hashlib and struct only, and checks
each checkpoint's Ed25519 signature with `openssl pkeyutl -verify -rawin`
and the exported PEM public key. Nothing of NoPACs' is used.

    field(b)   = len(b) as 4 bytes big-endian || b
    entry      = field("nopacs/audit-entry/v1") field(seq u64) field(id)
                 field(time us i64) field(key id) field(local user)
                 field(remote address) field(method) field(tenant name)
                 field(object) field(outcome) field(reason)
                 count(details) u32 || (field(name) field(value))* by name
                 field(request id)
    h_0 = 32 zero bytes;  h_n = SHA-256(h_{n-1} || entry_n)
    signed     = field("nopacs/audit-checkpoint/v1") field(installation)
                 field(log) field(from u64) field(to u64) field(head)
                 field(previous) field(time us i64) field(key version u32)
    previous of a checkpoint = SHA-256(signed text of the one before)

The Go test TestChainChecksInPython (internal/core/auditchain/outside_test.go)
runs it with `make check-auditchain`:

    auditchain.py <dir> <openssl>

<dir> holds entries.jsonl, checkpoints.jsonl and public-<version>.pem. The
script recomputes every entry's encoding and compares it with the one Go
wrote, folds the chain, checks each checkpoint's range, head, previous and
signature, and that key version and time never go down from one checkpoint
to the next, and prints "head <seq> <hex>" per checkpoint. Then it checks that
it would notice: one entry changed gives another head, a flipped signature
and a changed signed text fail openssl. Any disagreement exits with status 1
and a reason on standard error.
"""

import hashlib
import json
import os

from nopacs_scripts.formats.audit import encode_entry as encode, openssl_verifies, signed_text
from nopacs_scripts.report import failing


fail = failing("check-auditchain: ")


def read_lines(path):
    with open(path, encoding="utf-8") as f:
        return [json.loads(line) for line in f if line.strip()]


def fold(entries):
    """Return the head after each seq."""
    heads = {}
    h = bytes(32)
    for want, e in enumerate(entries, 1):
        if e["seq"] != want:
            fail("entry %d has seq %d" % (want, e["seq"]))
        h = hashlib.sha256(h + encode(e)).digest()
        heads[e["seq"]] = h
    return heads


def main(argv):
    if len(argv) != 2:
        fail("usage: auditchain.py <dir> <openssl>")
    directory, openssl = argv[0], argv[1]
    entries = read_lines(os.path.join(directory, "entries.jsonl"))
    checkpoints = read_lines(os.path.join(directory, "checkpoints.jsonl"))

    for e in entries:
        mine = encode(e)
        theirs = bytes.fromhex(e["encoded"])
        if mine != theirs:
            at = next((i for i in range(min(len(mine), len(theirs))) if mine[i] != theirs[i]),
                      min(len(mine), len(theirs)))
            fail("entry %d: Go's encoding differs from Python's at byte %d" % (e["seq"], at))
    print("encoded %d entries as Go did" % len(entries))

    heads = fold(entries)
    previous = bytes(32)
    next_from = 1
    last_version, last_time = 0, None
    for c in checkpoints:
        if c["from"] != next_from or c["to"] < c["from"]:
            fail("checkpoint to %d does not follow the one before" % c["to"])
        if c["key_version"] < last_version or (last_time is not None and c["time_us"] < last_time):
            fail("checkpoint to %d: its key version or time goes back" % c["to"])
        last_version, last_time = c["key_version"], c["time_us"]
        if heads[c["to"]].hex() != c["head"]:
            fail("checkpoint to %d: the chain gives head %s, the checkpoint %s"
                 % (c["to"], heads[c["to"]].hex(), c["head"]))
        if c["previous"] != previous.hex():
            fail("checkpoint to %d does not name the checkpoint before" % c["to"])
        pem = os.path.join(directory, "public-%d.pem" % c["key_version"])
        message = signed_text(c)
        if not openssl_verifies(openssl, pem, message, bytes.fromhex(c["signature"])):
            fail("checkpoint to %d: openssl does not verify its signature" % c["to"])
        print("head %d %s" % (c["to"], c["head"]))
        previous = hashlib.sha256(message).digest()
        next_from = c["to"] + 1
    if next_from != len(entries) + 1:
        fail("entries after seq %d are not covered by a checkpoint" % (next_from - 1))
    print("verified %d entries under %d checkpoints; openssl verified every signature"
          % (len(entries), len(checkpoints)))

    # The check would notice: each change below must make it fail.
    changed = [dict(e) for e in entries]
    changed[0]["reason"] = changed[0]["reason"] + "x"
    if fold(changed)[checkpoints[0]["to"]].hex() == checkpoints[0]["head"]:
        fail("a changed entry still reaches the checkpoint's head")
    c = checkpoints[0]
    pem = os.path.join(directory, "public-%d.pem" % c["key_version"])
    sig = bytearray(bytes.fromhex(c["signature"]))
    sig[0] ^= 1
    if openssl_verifies(openssl, pem, signed_text(c), bytes(sig)):
        fail("openssl verifies a flipped signature")
    later = dict(c, time_us=c["time_us"] + 1)
    if openssl_verifies(openssl, pem, signed_text(later), bytes.fromhex(c["signature"])):
        fail("openssl verifies the signature over a changed signed text")
    print("refused a changed entry, a flipped signature and a changed signed text")

