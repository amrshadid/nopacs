"""The audit chain's byte formats, written from their description in
internal/core/auditchain/doc.go and api/proto/nopacs/v1/audit_seal.proto with
hashlib and struct only, and the Ed25519 check done by openssl. Shared by the
outside check (scripts/check/auditchain.py) and the auditor's tool
(scripts/prove/audit_verify.py). Nothing here uses NoPACs' code.

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

An entry is a dict with the keys seq, id, at_us, key_id, local_user,
remote_addr, method, tenant_name, object, outcome, reason, details (a dict)
and request_id. A checkpoint is a dict with installation, log, from, to,
head and previous (hex), time_us and key_version.
"""

import os
import struct
import subprocess
import tempfile

ZERO = bytes(32)
ENTRY_FIELDS = ("key_id", "local_user", "remote_addr", "method", "tenant_name", "object",
                "outcome", "reason")


def field(b):
    """One length-prefixed field."""
    return struct.pack(">I", len(b)) + b


def text(s):
    """A UTF-8 text as a field."""
    return field(s.encode("utf-8"))


def encode_entry(e):
    """The bytes folded into the chain for one entry."""
    out = [field(b"nopacs/audit-entry/v1"), field(struct.pack(">Q", e["seq"])), text(e["id"]),
           field(struct.pack(">q", e["at_us"]))]
    for name in ENTRY_FIELDS:
        out.append(text(e[name]))
    details = sorted((k.encode("utf-8"), v.encode("utf-8")) for k, v in e["details"].items())
    out.append(struct.pack(">I", len(details)))
    for k, v in details:
        out.append(field(k) + field(v))
    out.append(text(e["request_id"]))
    return b"".join(out)


def signed_text(c):
    """The bytes a checkpoint's signature covers."""
    return b"".join([
        field(b"nopacs/audit-checkpoint/v1"), text(c["installation"]), text(c["log"]),
        field(struct.pack(">Q", c["from"])), field(struct.pack(">Q", c["to"])),
        field(bytes.fromhex(c["head"])), field(bytes.fromhex(c["previous"])),
        field(struct.pack(">q", c["time_us"])), field(struct.pack(">I", c["key_version"])),
    ])


def openssl_verifies(openssl, pem, message, signature):
    """Whether openssl (3.0 or later) verifies an Ed25519 signature of message
    under the PEM public key at path pem. Nothing but a clean verification is
    a yes: any error, including an openssl without Ed25519, is a no."""
    with tempfile.TemporaryDirectory() as tmp:
        msg = os.path.join(tmp, "message")
        sig = os.path.join(tmp, "signature")
        with open(msg, "wb") as f:
            f.write(message)
        with open(sig, "wb") as f:
            f.write(signature)
        run = subprocess.run([openssl, "pkeyutl", "-verify", "-pubin", "-inkey", pem, "-rawin",
                              "-in", msg, "-sigfile", sig], capture_output=True, text=True)
        return run.returncode == 0 and "Signature Verified Successfully" in run.stdout
