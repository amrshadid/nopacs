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

import binascii
import os

from cryptography.exceptions import InvalidTag
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

from nopacs_scripts.keys import KeyFileError, read_keys as read_key_file
from nopacs_scripts.report import failing

FORMAT = 0x01
HEADER = 5
NONCE = 12
TAG = 16

fail = failing("check-seal: ")


def read_keys(path):
    """The key file's keys; a file in another format, or two keys of one
    version, fails the check."""
    try:
        keys = read_key_file(path)
    except KeyFileError as e:
        fail(str(e))
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
    if len(argv) != 5:
        fail("usage: seal.py <key file> <context> <hex sealed> <hex plaintext> <seal version>")
    keys = read_keys(argv[0])
    context = argv[1]
    go_sealed = binascii.unhexlify(argv[2])
    plaintext = binascii.unhexlify(argv[3])
    version = int(argv[4])
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

