"""NoPACs' backup file format (internal/secret/doc.go) with HKDF written out
from RFC 5869 over hmac and hashlib and AES-GCM from the cryptography package
(OpenSSL). Shared by the outside check (scripts/check/backup-cipher.py) and
the proof's reader (scripts/prove/backup_open.py). Nothing here uses NoPACs'
code.

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
"""

import base64
import hashlib
import hmac
import os
import struct

from cryptography.exceptions import InvalidTag
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

MAGIC = b"NOPACSBK"
FORMAT = 0x01
HEADER = 49
TAG = 16
PREFIX = "nopacs-key-v1"


class Refused(Exception):
    """The file does not open."""


class KeyFileError(Exception):
    """The key file is not in the documented format."""


def read_keys(path):
    """Read a key file: one "nopacs-key-v1 <version> <base64 key>" per line."""
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
        keys[int(fields[1])] = key
    return keys


def hkdf(ikm, salt, info):
    """HKDF-SHA-256 with 32 bytes of output (RFC 5869 2.2, 2.3)."""
    prk = hmac.new(salt if salt else b"\x00" * 32, ikm, hashlib.sha256).digest()
    return hmac.new(prk, info + b"\x01", hashlib.sha256).digest()


def file_key(keys, header):
    """The AES key of the file whose first 49 bytes are header."""
    version = struct.unpack(">I", header[9:13])[0]
    if version not in keys:
        raise Refused("key version %d is not in the key file" % version)
    backup = hkdf(keys[version], None, b"nopacs/backup/v1")
    return hkdf(backup, header[17:HEADER], b"nopacs/backup/v1/file")


def nonce_ad(header, index, last):
    """Nonce and additional data of chunk index."""
    flag = b"\x01" if last else b"\x00"
    return struct.pack(">Q", index) + b"\x00\x00\x00" + flag, header + struct.pack(">Q", index) + flag


def open_chunks(keys, data):
    """Yield the plaintext of each chunk in order; raise Refused at the first
    chunk that does not open, or when the final chunk is missing. A caller
    that uses what it yields before the generator ends must stage it: bytes
    yielded belong to a file that may turn out cut or changed further on."""
    if len(data) < HEADER or data[:8] != MAGIC or data[8] != FORMAT:
        raise Refused("not a backup")
    header = data[:HEADER]
    size = struct.unpack(">I", header[13:17])[0]
    if not 1 <= size <= 1 << 20:
        raise Refused("chunk size")
    aead = AESGCM(file_key(keys, header))
    pos, index = HEADER, 0
    while True:
        chunk = data[pos:pos + size + TAG]
        if len(chunk) == 0:
            raise Refused("the last chunk is missing")
        pos += len(chunk)
        last = pos == len(data)
        if len(chunk) < TAG:
            raise Refused("a chunk shorter than its tag")
        nonce, ad = nonce_ad(header, index, last)
        try:
            yield aead.decrypt(nonce, chunk, ad)
        except InvalidTag:
            raise Refused("chunk %d does not open" % index)
        if last:
            return
        index += 1


def open_backup(keys, data):
    """The whole plaintext, or Refused."""
    return b"".join(open_chunks(keys, data))


def make_backup(keys, version, size, plaintext):
    """Write a backup (for tests that need one made outside Go)."""
    header = MAGIC + bytes([FORMAT]) + struct.pack(">II", version, size) + os.urandom(32)
    aead = AESGCM(file_key(keys, header))
    pieces = [plaintext[i:i + size] for i in range(0, len(plaintext), size)] or [b""]
    out = [header]
    for index, piece in enumerate(pieces):
        nonce, ad = nonce_ad(header, index, index == len(pieces) - 1)
        out.append(aead.encrypt(nonce, piece, ad))
    return b"".join(out)
