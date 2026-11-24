#!/usr/bin/env python3
"""Check the MAC of a NoPACs correction record against Python's standard
library.

A MAC that only the code that made it can check proves nothing about the
format. This script computes it from the description in
internal/core/record/auth.go and internal/secret/doc.go, with HKDF written
out from RFC 5869 over hmac and hashlib, and nothing of NoPACs':

    key     = HKDF-SHA-256(installation key of version mac_key_version,
                           no salt, "nopacs/correction-record/v1")
    body    = the record's bytes without the newline after it and without
              `,"mac":"<64 hex digits>"` before the closing brace
    message = len(tenant) || tenant || len(id) || id || body
              (each len 4 bytes, big-endian)
    mac     = HMAC-SHA-256(key, message)

The Go test TestRecordMACInPython (internal/core/record/outside_test.go) runs
it with `make check-record-mac`:

    record-mac.py <key file> <tenant id> <record file>

It prints "mac <hex>" when the MAC it computes is the record's, and checks
that the same record does not verify for another tenant, with a byte of its
body changed, or with its MAC taken out. Any disagreement exits with status 1
and a reason on standard error.
"""

import base64
import hashlib
import hmac
import json
import re
import struct
import sys

PREFIX = "nopacs-key-v1"
LABEL = b"nopacs/correction-record/v1"
MAC_FIELD = re.compile(rb',"mac":"([0-9a-f]{64})"}\n?$')


def fail(reason):
    print("check-record-mac: " + reason, file=sys.stderr)
    sys.exit(1)


def read_keys(path):
    """Read a key file: one "nopacs-key-v1 <version> <base64 key>" per line."""
    keys = {}
    with open(path, "rb") as f:
        raw = f.read().decode("ascii")
    for number, line in enumerate(raw.splitlines(), 1):
        fields = line.split(" ")
        if len(fields) != 3 or fields[0] != PREFIX:
            fail("key file line %d is not in the documented format" % number)
        key = base64.b64decode(fields[2], validate=True)
        if len(key) != 32:
            fail("key file line %d does not hold a 32-byte key" % number)
        keys[int(fields[1])] = key
    return keys


def hkdf(ikm, info):
    """HKDF-SHA-256 with no salt and 32 bytes of output (RFC 5869 2.2, 2.3)."""
    prk = hmac.new(b"\x00" * 32, ikm, hashlib.sha256).digest()
    return hmac.new(prk, info + b"\x01", hashlib.sha256).digest()


def mac_of(keys, tenant, data):
    """The MAC the record data should carry for tenant, and the one it does."""
    m = MAC_FIELD.search(data)
    if m is None:
        return None, None
    body = data[: m.start()] + b"}"
    record = json.loads(body)
    version, op = record["mac_key_version"], record["id"]
    if version not in keys:
        return None, m.group(1).decode()
    key = hkdf(keys[version], LABEL)
    t, i = tenant.encode(), op.encode()
    message = struct.pack(">I", len(t)) + t + struct.pack(">I", len(i)) + i + body
    return hmac.new(key, message, hashlib.sha256).hexdigest(), m.group(1).decode()


def main():
    if len(sys.argv) != 4:
        fail("usage: record-mac.py <key file> <tenant id> <record file>")
    keys = read_keys(sys.argv[1])
    tenant = sys.argv[2]
    with open(sys.argv[3], "rb") as f:
        data = f.read()

    want, got = mac_of(keys, tenant, data)
    if want is None or not hmac.compare_digest(want, got):
        fail("the record's MAC %s is not the one computed, %s" % (got, want))
    print("mac " + want)

    other, _ = mac_of(keys, tenant + "x", data)
    if other == got:
        fail("the record verifies for another tenant")
    print("refused for another tenant")

    changed = data.replace(b'"state":"', b'"state":"x', 1)
    w, g = mac_of(keys, tenant, changed)
    if w == g:
        fail("the record verifies with a byte of its body changed")
    print("refused with its body changed")

    if MAC_FIELD.search(MAC_FIELD.sub(b"}\n", data)) is not None:
        fail("the MAC field is still there once taken out")
    print("refused with its MAC taken out")


if __name__ == "__main__":
    main()
