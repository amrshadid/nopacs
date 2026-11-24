"""The installation key file and the key derivation both formats use,
written from the description in internal/secret/doc.go with hmac, hashlib
and base64 only."""

import base64
import hashlib
import hmac

PREFIX = "nopacs-key-v1"


class KeyFileError(Exception):
    """The key file is not in the documented format."""


def read_keys(path):
    """Read a key file: one "nopacs-key-v1 <version> <base64 key>" per line;
    returns {version: 32-byte key}."""
    keys = {}
    with open(path, "rb") as f:
        raw = f.read().decode("ascii")
    for number, line in enumerate(raw.splitlines(), 1):
        fields = line.split(" ")
        if len(fields) != 3 or fields[0] != PREFIX:
            raise KeyFileError("key file line %d is not in the documented format" % number)
        key = base64.b64decode(fields[2], validate=True)
        if len(key) != 32:
            raise KeyFileError("key file line %d does not hold a 32-byte key" % number)
        version = int(fields[1])
        if version in keys:
            raise KeyFileError("key file version %d appears twice" % version)
        keys[version] = key
    return keys


def hkdf(ikm, salt, info):
    """HKDF-SHA-256 with 32 bytes of output (RFC 5869 2.2, 2.3); no salt (None)
    is a string of 32 zero bytes."""
    prk = hmac.new(salt if salt else b"\x00" * 32, ikm, hashlib.sha256).digest()
    return hmac.new(prk, info + b"\x01", hashlib.sha256).digest()
