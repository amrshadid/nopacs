"""SHA-256 of files, by hashlib, to compare with what the archive recorded."""

import hashlib


def sha256_file(path):
    """The hex SHA-256 of the file at path, read in 1 MiB pieces."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()
