"""The self-test of audit_verify.py: it builds an export with keys made by
openssl, checks that the tool accepts it, then breaks it in each way the tool
claims to find and checks the kind and the seq it names. Run by
`audit_verify.py --self-test`.
"""

import base64
import copy
import hashlib
import json
import os
import subprocess
import sys
import tempfile

from nopacs_scripts.formats.audit import ZERO, encode_entry, signed_text
from nopacs_scripts.report import Failure
from nopacs_scripts.tools import audit_verify as av

INSTALLATION = "inst-selftest"
LOG = "tenant-selftest"
BASE_US = 1_790_000_000_123_456  # whole microseconds, with a fraction.


def b64(raw):
    return base64.b64encode(raw).decode()


def stamp(us):
    s, frac = divmod(us, 1_000_000)
    import time
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(s)) + ".%06dZ" % frac


def sh(cmd):
    out = subprocess.run(cmd, capture_output=True)
    if out.returncode != 0:
        raise RuntimeError("%s: %s" % (cmd[:3], out.stderr.decode()))


class Signer:
    """An Ed25519 key made and used by openssl."""

    def __init__(self, openssl, tmp, name):
        self.openssl = openssl
        self.priv = os.path.join(tmp, name + ".key.pem")
        self.pub = os.path.join(tmp, name + ".pub.pem")
        self.tmp = tmp
        sh([openssl, "genpkey", "-algorithm", "ED25519", "-out", self.priv])
        sh([openssl, "pkey", "-in", self.priv, "-pubout", "-out", self.pub])
        self.raw = av.pem_key(self.pub)

    def sign(self, message):
        msg, sig = os.path.join(self.tmp, "m"), os.path.join(self.tmp, "s")
        with open(msg, "wb") as f:
            f.write(message)
        sh([self.openssl, "pkeyutl", "-sign", "-inkey", self.priv, "-rawin", "-in", msg, "-out", sig])
        with open(sig, "rb") as f:
            return f.read()


def entry_fields(seq):
    return {"seq": seq, "id": "id-%03d" % seq, "at_us": BASE_US + seq * 1_000_000, "key_id": "" if seq == 2 else "k%d" % seq,
            "local_user": "", "remote_addr": "192.0.2.%d:4000" % seq, "method": "/nopacs.v1.TenantService/Get",
            "tenant_name": "tenant-é", "object": "o%d" % seq, "outcome": ("done", "refused", "failed")[seq % 3],
            "reason": "" if seq % 3 == 0 else "no grant ✓", "details": {"scope": "tenant", "b": "2", "a": ""} if seq % 2 else {},
            "request_id": "req-%d" % seq}


def entry_message(f, encoded, chain):
    outcome = {"done": "AUDIT_OUTCOME_DONE", "refused": "AUDIT_OUTCOME_REFUSED", "failed": "AUDIT_OUTCOME_FAILED"}[f["outcome"]]
    e = {"id": f["id"], "time": stamp(f["at_us"]), "keyId": f["key_id"], "localUserId": f["local_user"],
         "clientAddress": f["remote_addr"], "method": f["method"], "tenant": f["tenant_name"], "object": f["object"],
         "outcome": outcome, "requestId": f["request_id"], "details": f["details"], "reason": f["reason"]}
    return {"entry": {"seq": str(f["seq"]), "entry": e, "encoded": b64(encoded), "chain": b64(chain)}}


def checkpoint(signer, frm, to, head, previous, when, version, log=LOG, pruned=False):
    c = {"installation": INSTALLATION, "log": log, "from": frm, "to": to, "head": head.hex(), "previous": previous.hex(),
         "time_us": when, "key_version": version}
    message = signed_text(c)
    m = {"fromSeq": str(frm), "toSeq": str(to), "head": b64(head), "previousDigest": b64(previous),
         "digest": b64(hashlib.sha256(message).digest()), "time": stamp(when), "keyVersion": version,
         "signature": b64(signer.sign(message)), "signedText": b64(message), "installationId": INSTALLATION, "log": log}
    if pruned:
        m["pruned"] = True
    return {"checkpoint": m}, hashlib.sha256(message).digest()


def build(signers, first=1, prune_first=False):
    """An export of seq 1 to 6 sealed in two checkpoints (1..3 under key 1,
    4..6 under key 2), as a list of messages. first is where the range starts;
    prune_first leaves the first checkpoint's entries out."""
    h, heads, enc, chains = ZERO, {0: ZERO}, {}, {}
    for seq in range(1, 7):
        enc[seq] = encode_entry(entry_fields(seq))
        h = hashlib.sha256(h + enc[seq]).digest()
        heads[seq], chains[seq] = h, h
    out = [None]
    cp1, d1 = checkpoint(signers[0], 1, 3, heads[3], ZERO, BASE_US + 10_000_000, 1, pruned=prune_first)
    cp2, d2 = checkpoint(signers[1], 4, 6, heads[6], d1, BASE_US + 20_000_000, 2)
    seqs = [s for s in range(1, 7) if not (prune_first and s <= 3) and s >= first]
    for s in seqs:
        out.append(entry_message(entry_fields(s), enc[s], chains[s]))
        if s == 3:
            out.append(cp1)
        if s == 6:
            out.append(cp2)
    if prune_first:
        out.insert(1, cp1)
    keys = [{"keyVersion": 1, "publicKey": b64(signers[0].raw), "installationId": INSTALLATION},
            {"keyVersion": 2, "publicKey": b64(signers[1].raw), "installationId": INSTALLATION}]
    start = 1 if prune_first else first
    out[0] = {"header": {"installationId": INSTALLATION, "log": LOG, "fromSeq": str(start), "encodingVersion": 1,
                         "chainBefore": b64(heads[start - 1] if start > 1 else ZERO), "publicKeys": keys,
                         "anchors": [{"log": "master", "digest": b64(hashlib.sha256(b"anchor").digest()), "toSeq": "9"}]}}
    return out, d1, d2, heads


def lines_of(messages):
    return [json.dumps(m) for m in messages]


def expect(name, messages, keys, openssl, kind, seq, **kw):
    try:
        av.verify_lines(lines_of(messages), keys, openssl, **kw)
    except av.Problem as p:
        if (p.kind, p.seq) != (kind, seq):
            raise Failure("self-test %s: found %s at seq %d (%s), wanted %s at seq %d" % (name, p.kind, p.seq, p.detail, kind, seq))
        print("self-test: %-34s -> %s at seq %d" % (name, p.kind, p.seq))
        return
    raise Failure("self-test %s: nothing found, wanted %s at seq %d" % (name, kind, seq))


def run_all(openssl):
    with tempfile.TemporaryDirectory() as tmp:
        signers = [Signer(openssl, tmp, "one"), Signer(openssl, tmp, "two")]
        attacker = Signer(openssl, tmp, "attacker")
        keys = {(None, 1): signers[0].pub, (None, 2): signers[1].pub}
        good, d1, d2, heads = build(signers)

        v = av.verify_lines(lines_of(good), keys, openssl, expect_seq=6, expect_digest=d2.hex())
        assert (v.entries, v.checkpoints, v.last_to) == (6, 2, 6), "the intact export must verify"
        print("self-test: %-34s -> ok (6 entries, 2 checkpoints)" % "intact export")
        av.verify_lines(lines_of(good), {**keys, (INSTALLATION, 1): signers[0].pub}, openssl)

        part, _, _, _ = build(signers, first=4)
        av.verify_lines(lines_of(part), keys, openssl)
        print("self-test: %-34s -> ok" % "range from seq 4 (chain_before)")
        pruned, _, _, _ = build(signers, prune_first=True)
        v = av.verify_lines(lines_of(pruned), keys, openssl)
        assert v.pruned == 1 and v.entries == 3
        print("self-test: %-34s -> ok" % "first checkpoint pruned")

        def mutate(fn, base=good):
            m = copy.deepcopy(base)
            fn(m)
            return m

        def entry(m, seq):
            return next(x["entry"] for x in m if "entry" in x and x["entry"]["seq"] == str(seq))

        def cps(m):
            return [x["checkpoint"] for x in m if "checkpoint" in x]

        expect("entry field changed", mutate(lambda m: entry(m, 2)["entry"].update(reason="x")), keys, openssl,
               "ENCODING_MISMATCH", 2)

        def reencoded(m):
            f = entry_fields(2)
            f["reason"] = "x"
            entry(m, 2)["encoded"] = b64(encode_entry(f))
            entry(m, 2)["entry"]["reason"] = "x"
        expect("entry and its encoding changed", mutate(reencoded), keys, openssl, "CHAIN_BROKEN", 2)

        def refolded(m):
            reencoded(m)
            h = base64.b64decode(entry(m, 1)["chain"])
            for s in (2, 3):
                enc = base64.b64decode(entry(m, s)["encoded"])
                h = hashlib.sha256(h + enc).digest()
                entry(m, s)["chain"] = b64(h)
        expect("chain refolded past a change", mutate(refolded), keys, openssl, "CHAIN_BROKEN", 3)
        expect("entry removed", mutate(lambda m: m.remove(next(x for x in m if "entry" in x and x["entry"]["seq"] == "2"))),
               keys, openssl, "ENTRY_MISSING", 2)
        expect("entry repeated", mutate(lambda m: m.insert(3, copy.deepcopy(m[2]))), keys, openssl, "ENTRY_OUT_OF_ORDER", 2)

        def flip(m):
            s = bytearray(base64.b64decode(cps(m)[0]["signature"]))
            s[0] ^= 1
            cps(m)[0]["signature"] = b64(bytes(s))
        expect("signature flipped", mutate(flip), keys, openssl, "BAD_SIGNATURE", 3)
        expect("checkpoint time changed", mutate(lambda m: cps(m)[1].update(time=stamp(BASE_US + 20_000_001), signedText="", digest="")),
               keys, openssl, "BAD_SIGNATURE", 6)
        expect("signed text altered", mutate(lambda m: cps(m)[1].update(signedText=b64(b"x"))), keys, openssl,
               "BAD_SIGNATURE", 6)

        def attacker_reseal(m):
            """The log rewritten and sealed again under a key of the attacker's,
            whose public key the header carries."""
            f = entry_fields(5)
            f["reason"] = "rewritten"
            entry(m, 5)["encoded"] = b64(encode_entry(f))
            entry(m, 5)["entry"]["reason"] = "rewritten"
            h = base64.b64decode(entry(m, 4)["chain"])
            for s in (5, 6):
                h = hashlib.sha256(h + base64.b64decode(entry(m, s)["encoded"])).digest()
                entry(m, s)["chain"] = b64(h)
            c, _ = checkpoint(attacker, 4, 6, h, d1, BASE_US + 20_000_000, 2)
            cps(m)[1].clear()
            cps(m)[1].update(c["checkpoint"])
            m[0]["header"]["publicKeys"][1]["publicKey"] = b64(attacker.raw)
        expect("resealed with the attacker's key", mutate(attacker_reseal), keys, openssl, "KEY_MISMATCH", 0)
        no_header_key = {(None, 1): signers[0].pub}
        expect("resealed, key 2 not given", mutate(attacker_reseal), no_header_key, openssl, "BAD_SIGNATURE", 6)

        def attacker_new_version(m):
            attacker_reseal(m)
            c, _ = checkpoint(attacker, 4, 6, base64.b64decode(cps(m)[1]["head"]), d1, BASE_US + 20_000_000, 3)
            cps(m)[1].update(c["checkpoint"])
        expect("sealed under a version not given", mutate(attacker_new_version), no_header_key, openssl, "BAD_SIGNATURE", 6)
        expect("wrong key given for version 2", good, {(None, 1): signers[0].pub, (None, 2): attacker.pub}, openssl,
               "KEY_MISMATCH", 0)

        def previous_wrong(m):
            cps(m)[1]["previousDigest"] = b64(hashlib.sha256(b"x").digest())
        expect("previous digest wrong", mutate(previous_wrong), keys, openssl, "CHECKPOINT_OUT_OF_LINE", 6)

        def version_back(m):
            c1, nd1 = checkpoint(signers[1], 1, 3, heads[3], ZERO, BASE_US + 10_000_000, 2)
            c2, _ = checkpoint(signers[0], 4, 6, heads[6], nd1, BASE_US + 20_000_000, 1)
            cps(m)[0].update(c1["checkpoint"])
            cps(m)[1].update(c2["checkpoint"])
        expect("key version goes down", mutate(version_back), keys, openssl, "CHECKPOINT_OUT_OF_LINE", 6)

        def time_back(m):
            c, _ = checkpoint(signers[1], 4, 6, heads[6], d1, BASE_US + 5_000_000, 2)
            cps(m)[1].update(c["checkpoint"])
        expect("time goes down", mutate(time_back), keys, openssl, "CHECKPOINT_OUT_OF_LINE", 6)

        expect("last checkpoint cut off", mutate(lambda m: m.pop()), keys, openssl, "CHECKPOINT_OUT_OF_LINE", 4)
        expect("other log than the header's", mutate(lambda m: m[0]["header"].update(log="master")), keys, openssl,
               "CHECKPOINT_OUT_OF_LINE", 3)
        expect("seq below the expected", good, keys, openssl, "ROLLED_BACK", 6, expect_seq=7)
        expect("digest not in the log", good, keys, openssl, "ROLLED_BACK", 6, expect_digest="00" * 32)
        try:
            av.verify_lines(["{"], keys, openssl)
        except av.Problem as p:
            assert p.kind == "FORMAT"
            print("self-test: %-34s -> FORMAT" % "a line that is not JSON")
        av.verify_lines(lines_of(good), keys, openssl, expect_seq=6)

        path = os.path.join(tmp, "export.jsonl")
        with open(path, "w", encoding="utf-8") as f:
            f.write("\n".join(lines_of(good)) + "\n")
        code = av.main([path, "--public-key", "1=" + signers[0].pub, "--public-key", "2=" + signers[1].pub,
                        "--openssl", openssl, "--expect-seq", "6"])
        assert code == 0
        code = av.main([path, "--public-key", "1=" + signers[0].pub, "--public-key", "2=" + attacker.pub,
                        "--openssl", openssl])
        assert code == 1
        print("self-test: command line exits 0 on the intact export and 1 on a wrong key")
    print("audit_verify self-test: all passed")
    return 0


def run(openssl):
    try:
        return run_all(openssl)
    except RuntimeError as e:
        print("audit_verify self-test: openssl failed (3.0 or later is needed): %s" % e, file=sys.stderr)
        return 1
