#!/usr/bin/env python3
"""Check NoPACs' sealed-secret format against Python's cryptography package.

A sealed value that only the code that made it can open proves nothing about
the format. This script implements the format from its description in
internal/secret/doc.go, with AES-GCM from the cryptography package (OpenSSL),
and nothing of NoPACs':

    1 byte    format, 0x01
    4 bytes   key version, big-endian
    12 bytes  nonce
    n+16      ciphertext, then the 16-byte tag

    additional data = the five header bytes, then the context in UTF-8

The Go test TestSealOpensInPython (internal/secret/outside_test.go) runs it
with `make check-seal`:

    seal.py <key file> <context> <hex sealed by Go> <hex plaintext> <seal version>

It opens the value Go sealed and compares the plaintext, checks that the same
value does not open under another context or with one byte changed, then seals
the plaintext under <seal version> and prints "value <hex>" for Go to open.
Any disagreement exits with status 1 and a reason on standard error.
"""

import base64
import binascii
import os
import sys

from cryptography.exceptions import InvalidTag
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

FORMAT = 0x01
HEADER = 5
NONCE = 12
TAG = 16
PREFIX = "nopacs-key-v1"


def fail(reason):
    print("check-seal: " + reason, file=sys.stderr)
    sys.exit(1)


def read_keys(path):
    """Read a key file: one "nopacs-key-v1 <version> <base64 key>" per line."""
    keys = {}
    with open(path, "rb") as f:
        text = f.read().decode("ascii")
    for number, line in enumerate(text.splitlines(), 1):
        fields = line.split(" ")
        if len(fields) != 3 or fields[0] != PREFIX:
            fail("key file line %d is not in the documented format" % number)
        version = int(fields[1])
        key = base64.b64decode(fields[2], validate=True)
        if len(key) != 32:
            fail("key file line %d does not hold a 32-byte key" % number)
        if version in keys:
            fail("key file version %d appears twice" % version)
        keys[version] = key
    return keys


def additional_data(header, context):
    return header + context.encode("utf-8")


def open_sealed(keys, context, sealed):
    if len(sealed) < HEADER + NONCE + TAG:
        raise InvalidTag()
    if sealed[0] != FORMAT:
        raise InvalidTag()
    version = int.from_bytes(sealed[1:HEADER], "big")
    if version not in keys:
        raise InvalidTag()
    nonce = sealed[HEADER:HEADER + NONCE]
    body = sealed[HEADER + NONCE:]
    return AESGCM(keys[version]).decrypt(nonce, body, additional_data(sealed[:HEADER], context))


def seal(keys, version, context, plaintext):
    header = bytes([FORMAT]) + version.to_bytes(4, "big")
    nonce = os.urandom(NONCE)
    body = AESGCM(keys[version]).encrypt(nonce, plaintext, additional_data(header, context))
    return header + nonce + body


def main(argv):
    if len(argv) != 6:
        fail("usage: seal.py <key file> <context> <hex sealed> <hex plaintext> <seal version>")
    keys = read_keys(argv[1])
    context = argv[2]
    go_sealed = binascii.unhexlify(argv[3])
    plaintext = binascii.unhexlify(argv[4])
    version = int(argv[5])
    if version not in keys:
        fail("seal version %d is not in the key file" % version)

    try:
        opened = open_sealed(keys, context, go_sealed)
    except InvalidTag:
        fail("the value Go sealed does not open in Python")
    if opened != plaintext:
        fail("the value Go sealed opens in Python to a different plaintext")
    print("opened the %d-byte value Go sealed with key version %d"
          % (len(go_sealed), int.from_bytes(go_sealed[1:HEADER], "big")))

    try:
        open_sealed(keys, context + "x", go_sealed)
        fail("the value Go sealed opens in Python under another context")
    except InvalidTag:
        pass
    changed = bytearray(go_sealed)
    changed[-1] ^= 1
    try:
        open_sealed(keys, context, bytes(changed))
        fail("the value Go sealed opens in Python with a byte changed")
    except InvalidTag:
        pass
    print("refused it under another context and with a byte changed")

    py_sealed = seal(keys, version, context, plaintext)
    print("sealed %d bytes with key version %d" % (len(py_sealed), version))
    print("value " + binascii.hexlify(py_sealed).decode("ascii"))


if __name__ == "__main__":
    main(sys.argv)
