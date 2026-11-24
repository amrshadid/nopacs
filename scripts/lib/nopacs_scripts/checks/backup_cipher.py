"""Check NoPACs' backup file format against Python's standard library and
the cryptography package.

A backup that only the code that wrote it can open proves nothing about the
format. This script implements the format from its description in
internal/secret/doc.go, with HKDF written out from RFC 5869 over hmac and
hashlib, AES-GCM from the cryptography package (OpenSSL), and nothing of
NoPACs':

    8 bytes   "NOPACSBK"
    1 byte    format, 0x01
    4 bytes   key version, big-endian
    4 bytes   chunk size S, big-endian
    32 bytes  salt
    chunks:   ciphertext of S bytes (the last: 0 to S) || 16-byte tag

    backup key = HKDF-SHA-256(key of that version, no salt, "nopacs/backup/v1")
    file key   = HKDF-SHA-256(backup key, salt, "nopacs/backup/v1/file")
    nonce_i    = i (8 bytes big-endian) || 00 00 00 || last
    AD_i       = header (49 bytes) || i (8 bytes big-endian) || last

The Go test TestBackupOpensInPython (internal/secret/outside_test.go) runs it
with `make check-backup-cipher`:

    backup-cipher.py open <key file> <backup by Go> <hex sha256 of plaintext> <out>

It opens the file Go wrote and compares the plaintext's SHA-256, checks that a
cut at a chunk boundary, two chunks swapped, a flipped bit and an appended
chunk each fail, then writes the same plaintext as a new backup at <out> for
Go to open. Any disagreement exits with status 1 and a reason on standard
error.

    backup-cipher.py make <key file> <version> <chunk size> <hex plaintext> <out>

writes one backup, for a test that needs a fixed file made outside Go.
"""

import binascii
import hashlib
import struct

from nopacs_scripts.formats.backup import HEADER, TAG, Refused, make_backup, open_backup
from nopacs_scripts.keys import KeyFileError, read_keys as read_key_file
from nopacs_scripts.report import failing


fail = failing("check-backup-cipher: ")


def read_keys(path):
    try:
        return read_key_file(path)
    except KeyFileError as e:
        fail(str(e))


def expect_refused(keys, data, what):
    try:
        open_backup(keys, data)
    except Refused:
        return
    fail("the backup Go wrote still opens in Python with " + what)


def check_open(argv):
    keys = read_keys(argv[1])
    with open(argv[2], "rb") as f:
        data = f.read()
    want = argv[3]
    try:
        plaintext = open_backup(keys, data)
    except Refused as e:
        fail("the backup Go wrote does not open in Python: %s" % e)
    if hashlib.sha256(plaintext).hexdigest() != want:
        fail("the backup Go wrote opens in Python to a different plaintext")
    size = struct.unpack(">I", data[13:17])[0]
    chunks = (len(data) - HEADER + size + TAG - 1) // (size + TAG)
    print("opened the %d-byte backup Go wrote: %d chunks of %d, %d bytes of plaintext"
          % (len(data), chunks, size, len(plaintext)))
    if chunks < 3:
        fail("the backup Go wrote has fewer than 3 chunks, too few to check reordering")

    span = size + TAG
    c0, c1 = data[HEADER:HEADER + span], data[HEADER + span:HEADER + 2 * span]
    expect_refused(keys, data[:HEADER + span], "a cut after chunk 0")
    expect_refused(keys, data[:HEADER] + c1 + c0 + data[HEADER + 2 * span:], "chunks 0 and 1 swapped")
    flipped = bytearray(data)
    flipped[HEADER + span + 3] ^= 0x01
    expect_refused(keys, bytes(flipped), "a bit flipped in chunk 1")
    expect_refused(keys, data + c0, "chunk 0 appended")
    print("refused a cut at a chunk boundary, two chunks swapped, a flipped bit and an appended chunk")

    version = struct.unpack(">I", data[9:13])[0]
    written = make_backup(keys, version, size, plaintext)
    with open(argv[4], "wb") as f:
        f.write(written)
    print("wrote a %d-byte backup with key version %d" % (len(written), version))


def main(argv):
    if len(argv) == 5 and argv[0] == "open":
        check_open(argv)
    elif len(argv) == 6 and argv[0] == "make":
        keys = read_keys(argv[1])
        data = make_backup(keys, int(argv[2]), int(argv[3]), binascii.unhexlify(argv[4]))
        with open(argv[5], "wb") as f:
            f.write(data)
    else:
        fail("usage: backup-cipher.py open <key file> <backup> <hex sha256> <out> | "
             "make <key file> <version> <chunk size> <hex plaintext> <out>")

