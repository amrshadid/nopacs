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

import hashlib
import hmac
import json
import re
import struct

from nopacs_scripts.keys import KeyFileError, hkdf, read_keys as read_key_file
from nopacs_scripts.report import failing

LABEL = b"nopacs/correction-record/v1"
MAC_FIELD = re.compile(rb',"mac":"([0-9a-f]{64})"}\n?$')


fail = failing("check-record-mac: ")


def read_keys(path):
    """The key file's keys; a file in another format fails the check."""
    try:
        return read_key_file(path)
    except KeyFileError as e:
        fail(str(e))


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
    key = hkdf(keys[version], None, LABEL)
    t, i = tenant.encode(), op.encode()
    message = struct.pack(">I", len(t)) + t + struct.pack(">I", len(i)) + i + body
    return hmac.new(key, message, hashlib.sha256).hexdigest(), m.group(1).decode()


def main(argv):
    if len(argv) != 3:
        fail("usage: record-mac.py <key file> <tenant id> <record file>")
    keys = read_keys(argv[0])
    tenant = argv[1]
    with open(argv[2], "rb") as f:
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

