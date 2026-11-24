#!/usr/bin/env python3
"""Verify an exported audit log without NoPACs: the chain with hashlib, each
checkpoint's Ed25519 signature with openssl, against public keys the auditor
names on the command line.

    audit_verify.py EXPORT --public-key [INSTALLATION@]VERSION=PEM ...
                    [--expect-seq N] [--expect-digest HEX] [--openssl PATH]
    audit_verify.py --self-test [--openssl PATH]

EXPORT is a file of JSON lines (or - for standard input), the messages of
AuditSealService.ExportAudit (api/proto/nopacs/v1/audit_seal.proto) written as
protobuf JSON, one ExportAuditResponse per line, in the order the server sent
them:

    {"header": {...}}        first, once: AuditExportHeader
    {"entry": {...}}         SealedAuditEntry, in order of seq
    {"checkpoint": {...}}    AuditCheckpoint, after the last entry it covers

Field names are the proto's, in snake_case or lowerCamelCase. int64 values are
numbers or decimal strings; bytes are standard base64; a time is an RFC 3339
string in UTC ("2026-11-21T10:00:00.123456Z") or {"seconds", "nanos"}; an
outcome is its enum name (AUDIT_OUTCOME_DONE) or number; a field at its
default may be left out. What is read:

    header      installation_id, log, from_seq, chain_before, public_keys
                (key_version, public_key or public_key_pem, installation_id),
                anchors (log, digest, to_seq)
    entry       seq, encoded, chain, and entry: id, time, key_id,
                local_user_id, client_address, method, tenant, object,
                outcome, request_id, details, reason
    checkpoint  from_seq, to_seq, head, previous_digest, time, key_version,
                signature, installation_id, log, pruned, and, when present,
                digest and signed_text

What is checked, and the kind named for the first thing wrong, with the seq it
shows at (the checkpoint's to_seq for a checkpoint):

    ENCODING_MISMATCH   an entry's encoded form is not what its fields encode
                        to (audit_seal.proto, "The chain")
    CHAIN_BROKEN        an entry's chain value, or a checkpoint's head, is not
                        what SHA-256 folds from the entries before it
    ENTRY_MISSING       a seq is absent where a checkpoint covers it
    ENTRY_OUT_OF_ORDER  a seq repeats or goes back
    CHECKPOINT_OUT_OF_LINE  a range does not follow the one before, previous_digest
                        is not the digest of the signed text before, the log
                        differs from the header's, entries are left uncovered, or
                        a key version (per installation) or the time goes down
    BAD_SIGNATURE       openssl does not verify the signature over the signed
                        text rebuilt from the fields, signed_text differs from
                        that rebuild, or no public key was given for the
                        installation and version the checkpoint names
    KEY_MISMATCH        the header's public key for an installation and version
                        differs from the one given on the command line
    ROLLED_BACK         the last sealed seq is below --expect-seq, or no
                        checkpoint has --expect-digest
    FORMAT              the file is not what is described above

A pruned checkpoint (retention removed its entries) has no entries in the file:
its head is taken from it, on the strength of its signature, and the fold goes
on from there. A range that starts after seq 1 (header from_seq) is folded from
the header's chain_before; the first checkpoint's head, signed, must be reached
from it, so a wrong chain_before is found.

Public keys are never taken from the export alone. A person who can rewrite the
log can re-seal it with a key of their own and put its public key in the
header; a check against the header's key then passes. The keys come from where
the auditor kept them (GetAuditPublicKey, copied outside the installation when
the log was young), named as VERSION=PEM for the export's own installation or
INSTALLATION@VERSION=PEM for another (a tenant imported from elsewhere). The
header's keys are only compared with them.

--expect-seq and --expect-digest are anchors kept outside the installation:
the last sealed seq, and the digest of a checkpoint (SHA-256 of its signed
text), noted from an earlier export. A log rolled back whole, with its
checkpoints, is consistent and passes everything else.

Exit status: 0 and a summary on standard output when everything holds; 1 and
"audit_verify: <KIND> at seq <n>: <detail>" on standard error for the first
problem; 2 for a mistake in the command line. openssl must be 3.0 or later
(the macOS system LibreSSL has no Ed25519): OPENSSL in the environment, or
--openssl, or the one on the PATH. Nothing of NoPACs' is used.
"""

import argparse
import base64
import binascii
import calendar
import datetime
import hashlib
import os
import re
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from audit_format import ZERO, encode_entry, openssl_verifies, signed_text  # noqa: E402

# The prefix of an Ed25519 SubjectPublicKeyInfo (RFC 8410 4): the key follows.
SPKI_PREFIX = bytes.fromhex("302a300506032b6570032100")
OUTCOMES = {"AUDIT_OUTCOME_DONE": "done", "AUDIT_OUTCOME_REFUSED": "refused",
            "AUDIT_OUTCOME_FAILED": "failed", 1: "done", 2: "refused", 3: "failed"}


class Problem(Exception):
    """The first thing found wrong: its kind, the seq it shows at, and words."""

    def __init__(self, kind, seq, detail):
        super().__init__(detail)
        self.kind, self.seq, self.detail = kind, seq, detail


def camel(name):
    head, *rest = name.split("_")
    return head + "".join(w.capitalize() for w in rest)


def get(d, name, default=None):
    """A field by its proto name, in snake_case or lowerCamelCase."""
    if not isinstance(d, dict):
        raise Problem("FORMAT", 0, "a message is not a JSON object")
    if name in d:
        return d[name]
    return d.get(camel(name), default)


def as_int(v, what):
    if v is None:
        return 0
    if isinstance(v, bool) or not isinstance(v, (int, str)):
        raise Problem("FORMAT", 0, "%s is not an integer" % what)
    try:
        return int(v)
    except ValueError:
        raise Problem("FORMAT", 0, "%s is not an integer" % what)


def as_bytes(v, what, size=None):
    if v is None or v == "":
        raw = b""
    else:
        try:
            raw = base64.b64decode(v, validate=True)
        except (binascii.Error, TypeError, ValueError):
            raise Problem("FORMAT", 0, "%s is not base64" % what)
    if size is not None and len(raw) != size:
        raise Problem("FORMAT", 0, "%s is %d bytes, want %d" % (what, len(raw), size))
    return raw


def as_text(v, what):
    if v is None:
        return ""
    if not isinstance(v, str):
        raise Problem("FORMAT", 0, "%s is not a string" % what)
    return v


def micros(ts, what):
    """A Timestamp as whole microseconds since the epoch, in integers only.
    The chain holds whole microseconds, so a finer time cannot be encoded."""
    if ts is None:
        return 0
    if isinstance(ts, dict):
        seconds, nanos = as_int(get(ts, "seconds"), what), as_int(get(ts, "nanos"), what)
    elif isinstance(ts, str):
        m = re.fullmatch(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?(Z|\+00:00)", ts)
        if not m:
            raise Problem("FORMAT", 0, "%s is not an RFC 3339 UTC time" % what)
        when = datetime.datetime.strptime(m.group(1), "%Y-%m-%dT%H:%M:%S")
        seconds = calendar.timegm(when.timetuple())
        nanos = int((m.group(2) or "").ljust(9, "0"))
    else:
        raise Problem("FORMAT", 0, "%s is not a time" % what)
    if nanos % 1000:
        raise Problem("FORMAT", 0, "%s is not in whole microseconds" % what)
    return seconds * 1000000 + nanos // 1000


def pem_key(path):
    """The 32 raw bytes of an Ed25519 public key PEM (SubjectPublicKeyInfo)."""
    with open(path, encoding="ascii") as f:
        body = "".join(line.strip() for line in f if not line.startswith("-----"))
    der = base64.b64decode(body, validate=True)
    if len(der) != 44 or not der.startswith(SPKI_PREFIX):
        raise ValueError("%s is not an Ed25519 public key PEM" % path)
    return der[12:]


def parse_key_spec(spec, installation):
    """[INSTALLATION@]VERSION=PEM into ((installation, version), path)."""
    left, eq, path = spec.partition("=")
    if not eq or not path:
        raise ValueError("--public-key %r is not [INSTALLATION@]VERSION=PEM" % spec)
    inst, at, version = left.rpartition("@")
    if not version.isdigit() or int(version) < 1:
        raise ValueError("--public-key %r: the version is not a number from 1" % spec)
    return (inst if at else installation, int(version)), path


class Verifier:
    """One pass over an export's lines, holding what the next line is checked
    against."""

    def __init__(self, keys, openssl, expect_seq=None, expect_digest=None):
        self.keys = keys  # {(installation or None, version): pem path}
        self.openssl = openssl
        self.expect_seq = expect_seq
        self.expect_digest = expect_digest
        self.header = None
        self.h = ZERO
        self.next_seq = 0
        self.cp_next = 0
        self.pending = 0
        self.entries = 0
        self.checkpoints = 0
        self.pruned = 0
        self.prev_digest = None
        self.digests = set()
        self.last_to = 0
        self.last_digest = None
        self.last_time = None
        self.versions = {}
        self.notes = []

    def key_path(self, installation, version):
        found = self.keys.get((installation, version))
        if found is None and installation == self.header["installation"]:
            found = self.keys.get((None, version))
        return found

    def take_header(self, m):
        if self.header is not None:
            raise Problem("FORMAT", 0, "a second header")
        inst = as_text(get(m, "installation_id"), "header installation_id")
        log = as_text(get(m, "log"), "header log")
        from_seq = as_int(get(m, "from_seq"), "header from_seq")
        before = as_bytes(get(m, "chain_before"), "header chain_before", None)
        if from_seq in (0, 1) and not before:
            before = ZERO
        if len(before) != 32:
            raise Problem("FORMAT", 0, "header chain_before is not 32 bytes")
        if from_seq < 0:
            raise Problem("FORMAT", 0, "header from_seq is negative")
        version = as_int(get(m, "encoding_version"), "header encoding_version")
        if version not in (0, 1):
            raise Problem("FORMAT", 0, "encoding version %d is not one this tool knows" % version)
        self.header = {"installation": inst, "log": log}
        for k in get(m, "public_keys", []) or []:
            kinst = as_text(get(k, "installation_id"), "public key installation_id") or inst
            kver = as_int(get(k, "key_version"), "public key key_version")
            raw = as_bytes(get(k, "public_key"), "public key", None)
            path = self.key_path(kinst, kver)
            if path is None:
                self.notes.append("header lists a key for %s@%d that was not given on the command "
                                  "line: not trusted" % (kinst, kver))
            elif raw and raw != pem_key(path):
                raise Problem("KEY_MISMATCH", 0, "the header's public key for %s@%d differs from "
                              "the one given" % (kinst, kver))
        self.h, self.next_seq, self.cp_next = before, from_seq, from_seq
        self.anchors = []
        for a in get(m, "anchors", []) or []:
            self.anchors.append((as_text(get(a, "log"), "anchor log"),
                                 as_bytes(get(a, "digest"), "anchor digest", 32).hex(),
                                 as_int(get(a, "to_seq"), "anchor to_seq")))

    def take_entry(self, m):
        seq = as_int(get(m, "seq"), "entry seq")
        if self.next_seq == 0:
            raise Problem("CHECKPOINT_OUT_OF_LINE", seq, "an entry in an export that says it holds none")
        if seq > self.next_seq:
            raise Problem("ENTRY_MISSING", self.next_seq, "seq %d is absent" % self.next_seq)
        if seq < self.next_seq:
            raise Problem("ENTRY_OUT_OF_ORDER", seq, "seq %d comes after %d" % (seq, self.next_seq - 1))
        e = get(m, "entry")
        outcome = get(e, "outcome")
        if outcome not in OUTCOMES:
            raise Problem("FORMAT", seq, "entry outcome is not done, refused or failed")
        details = get(e, "details", {}) or {}
        if not isinstance(details, dict) or not all(isinstance(v, str) for v in details.values()):
            raise Problem("FORMAT", seq, "entry details are not a map of strings")
        fields = {
            "seq": seq, "id": as_text(get(e, "id"), "entry id"), "at_us": micros(get(e, "time"), "entry time"),
            "key_id": as_text(get(e, "key_id"), "entry key_id"),
            "local_user": as_text(get(e, "local_user_id"), "entry local_user_id"),
            "remote_addr": as_text(get(e, "client_address"), "entry client_address"),
            "method": as_text(get(e, "method"), "entry method"),
            "tenant_name": as_text(get(e, "tenant"), "entry tenant"),
            "object": as_text(get(e, "object"), "entry object"), "outcome": OUTCOMES[outcome],
            "reason": as_text(get(e, "reason"), "entry reason"), "details": details,
            "request_id": as_text(get(e, "request_id"), "entry request_id"),
        }
        mine = encode_entry(fields)
        theirs = as_bytes(get(m, "encoded"), "entry encoded")
        if mine != theirs:
            raise Problem("ENCODING_MISMATCH", seq, "the encoded form is not what the entry's fields encode to")
        self.h = hashlib.sha256(self.h + mine).digest()
        if self.h != as_bytes(get(m, "chain"), "entry chain", 32):
            raise Problem("CHAIN_BROKEN", seq, "the chain value is not what the entries before fold to")
        self.next_seq += 1
        self.pending += 1
        self.entries += 1

    def take_checkpoint(self, m):
        frm, to = as_int(get(m, "from_seq"), "checkpoint from_seq"), as_int(get(m, "to_seq"), "checkpoint to_seq")
        pruned = bool(get(m, "pruned", False))
        if self.cp_next == 0 or frm != self.cp_next or to < frm:
            raise Problem("CHECKPOINT_OUT_OF_LINE", to, "its range %d..%d does not follow on from the one before" % (frm, to))
        log = as_text(get(m, "log"), "checkpoint log")
        if log != self.header["log"]:
            raise Problem("CHECKPOINT_OUT_OF_LINE", to, "it seals a different log than the export's")
        head = as_bytes(get(m, "head"), "checkpoint head", 32)
        if pruned:
            if self.pending:
                raise Problem("CHECKPOINT_OUT_OF_LINE", to, "a pruned checkpoint with entries in the file")
            self.h, self.next_seq, self.pruned = head, to + 1, self.pruned + 1
        else:
            if self.next_seq - 1 < to:
                raise Problem("ENTRY_MISSING", self.next_seq, "seq %d is absent, a checkpoint covers it" % self.next_seq)
            if self.next_seq - 1 > to:
                raise Problem("CHECKPOINT_OUT_OF_LINE", to, "entries run past the checkpoint's range")
            if self.h != head:
                raise Problem("CHAIN_BROKEN", to, "the chain gives head %s, the checkpoint %s" % (self.h.hex(), head.hex()))
        previous = as_bytes(get(m, "previous_digest"), "checkpoint previous_digest") or ZERO
        if len(previous) != 32:
            raise Problem("FORMAT", to, "previous_digest is not 32 bytes")
        want = self.prev_digest if self.prev_digest is not None else (ZERO if frm == 1 else None)
        if want is not None and previous != want:
            raise Problem("CHECKPOINT_OUT_OF_LINE", to, "previous_digest is not the digest of the checkpoint before it")
        inst = as_text(get(m, "installation_id"), "checkpoint installation_id") or self.header["installation"]
        version = as_int(get(m, "key_version"), "checkpoint key_version")
        when = micros(get(m, "time"), "checkpoint time")
        if version < self.versions.get(inst, 0) or (self.last_time is not None and when < self.last_time):
            raise Problem("CHECKPOINT_OUT_OF_LINE", to, "its key version or time goes back")
        message = signed_text({"installation": inst, "log": log, "from": frm, "to": to, "head": head.hex(),
                               "previous": previous.hex(), "time_us": when, "key_version": version})
        given = as_bytes(get(m, "signed_text"), "checkpoint signed_text")
        if given and given != message:
            raise Problem("BAD_SIGNATURE", to, "signed_text is not what the fields rebuild")
        digest = hashlib.sha256(message).digest()
        stated = as_bytes(get(m, "digest"), "checkpoint digest")
        if stated and stated != digest:
            raise Problem("CHECKPOINT_OUT_OF_LINE", to, "digest is not the SHA-256 of the signed text")
        path = self.key_path(inst, version)
        if path is None:
            raise Problem("BAD_SIGNATURE", to, "no public key was given for %s@%d" % (inst, version))
        signature = as_bytes(get(m, "signature"), "checkpoint signature", 64)
        if not openssl_verifies(self.openssl, path, message, signature):
            raise Problem("BAD_SIGNATURE", to, "openssl does not verify the signature with the key given for %s@%d" % (inst, version))
        self.versions[inst], self.last_time = version, when
        self.prev_digest, self.cp_next, self.pending = digest, to + 1, 0
        self.digests.add(digest.hex())
        self.last_to, self.last_digest = to, digest.hex()
        self.checkpoints += 1

    def take(self, message):
        if not isinstance(message, dict):
            raise Problem("FORMAT", 0, "a line is not a JSON object")
        kinds = [k for k in ("header", "entry", "checkpoint") if k in message]
        if len(kinds) != 1:
            raise Problem("FORMAT", 0, "a line holds none or several of header, entry, checkpoint")
        if kinds[0] != "header" and self.header is None:
            raise Problem("FORMAT", 0, "the first line is not the header")
        {"header": self.take_header, "entry": self.take_entry,
         "checkpoint": self.take_checkpoint}[kinds[0]](message[kinds[0]])

    def finish(self):
        if self.header is None:
            raise Problem("FORMAT", 0, "the export is empty")
        if self.pending:
            raise Problem("CHECKPOINT_OUT_OF_LINE", self.next_seq - self.pending,
                          "entries from seq %d on are not covered by a checkpoint" % (self.next_seq - self.pending))
        if self.expect_seq is not None and self.last_to < self.expect_seq:
            raise Problem("ROLLED_BACK", self.last_to, "the last sealed seq %d is below the expected %d"
                          % (self.last_to, self.expect_seq))
        if self.expect_digest is not None and self.expect_digest not in self.digests:
            raise Problem("ROLLED_BACK", self.last_to, "no checkpoint has the expected digest")


def verify_lines(lines, keys, openssl, expect_seq=None, expect_digest=None):
    """Verify an export given as an iterable of JSON-line texts; return the
    Verifier, or raise Problem."""
    import json
    v = Verifier(keys, openssl, expect_seq, expect_digest)
    for number, line in enumerate(lines, 1):
        if not line.strip():
            continue
        try:
            message = json.loads(line)
        except ValueError:
            raise Problem("FORMAT", v.next_seq, "line %d is not JSON" % number)
        try:
            v.take(message)
        except Problem as p:
            if p.kind == "FORMAT" and p.seq == 0:
                p.seq = v.next_seq
                p.detail = "line %d: %s" % (number, p.detail)
            raise
    v.finish()
    return v


def summary(v):
    text =["audit_verify: ok: log %r of installation %r, %d entries and %d checkpoints (%d pruned), up to seq %d; "
            "openssl verified every signature" % (v.header["log"], v.header["installation"], v.entries,
                                                  v.checkpoints, v.pruned, v.last_to)]
    if v.last_digest:
        text.append("last checkpoint: to_seq %d digest %s" % (v.last_to, v.last_digest))
    for log, digest, to in v.anchors:
        text.append("anchor kept of %s: to_seq %d digest %s (compare with that log's export)" % (log, to, digest))
    return "\n".join(text)


def main(argv):
    p = argparse.ArgumentParser(description="Verify an exported audit log with hashlib and openssl; "
                                "the file format is in this tool's docstring.")
    p.add_argument("export", nargs="?", help="JSON lines of ExportAuditResponse, or - for standard input")
    p.add_argument("--public-key", action="append", default=[], metavar="[INSTALLATION@]VERSION=PEM",
                   help="a public key kept outside the installation; repeat per version (never read from the export)")
    p.add_argument("--expect-seq", type=int, help="a seq the log is known to have reached")
    p.add_argument("--expect-digest", help="hex digest of a checkpoint known to be in the log")
    p.add_argument("--openssl", default=os.environ.get("OPENSSL", "openssl"), help="openssl 3.0 or later (default $OPENSSL, else openssl)")
    p.add_argument("--self-test", action="store_true", help="check the tool against exports it builds and breaks")
    args = p.parse_args(argv)
    if args.self_test:
        import audit_verify_selftest
        return audit_verify_selftest.run(args.openssl)
    if not args.export or not args.public_key:
        p.error("an export and at least one --public-key are required")
    keys = {}
    try:
        for spec in args.public_key:
            key, path = parse_key_spec(spec, None)
            pem_key(path)
            keys[key] = path
        digest = None
        if args.expect_digest:
            digest = args.expect_digest.lower()
            if not re.fullmatch(r"[0-9a-f]{64}", digest):
                raise ValueError("--expect-digest is not 64 hex digits")
    except (ValueError, OSError, binascii.Error) as e:
        p.error(str(e))
    stream = sys.stdin if args.export == "-" else open(args.export, encoding="utf-8")
    try:
        v = verify_lines(stream, keys, args.openssl, args.expect_seq, digest)
    except Problem as e:
        print("audit_verify: %s at seq %d: %s" % (e.kind, e.seq, e.detail), file=sys.stderr)
        return 1
    finally:
        if stream is not sys.stdin:
            stream.close()
    for note in v.notes:
        print("audit_verify: note: " + note, file=sys.stderr)
    print(summary(v))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
