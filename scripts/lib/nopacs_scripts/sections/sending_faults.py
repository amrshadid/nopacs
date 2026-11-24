"""The sections of the proof of sending about faults and trust: a peer behind
TLS, a peer down then up, a damaged archived byte, a peer out of resources,
secrets, role refusals, and a kill on Postgres. sending.py runs them; each
reads what happened only with tools that are not NoPACs: dcmtk's storescp and
openssl, pynetdicom, Orthanc's REST API, Python's sqlite3 and the files on
disk."""

import json
import os
import shlex
import signal
import subprocess
import time
import urllib.parse

from pydicom import dcmread

from nopacs_scripts import send as sendkit
from nopacs_scripts.db import query
from nopacs_scripts.dicom import sop_uids
from nopacs_scripts.report import Failure
from nopacs_scripts.send import CT, many, rewrite, split_part10

JOB_UNIT_ATTEMPTS = 30  # the attempts a unit of a job starts with
SPENT_BEFORE_DOWN = 2  # the attempts a unit may spend before its sender is marked down


def spent_attempts(p, sender):
    """{unit's subject: attempts spent} of the scan units of a sender's jobs,
    read from the tenant's SQLite catalog: each claim counts an attempt, and a
    unit given back without spending one is allowed one more, so what was
    spent is the attempt less what was given back."""
    rows = query(p.catalog(), "SELECT subject, attempt, max_attempts FROM work WHERE kind = ?", "send-" + sender)
    return {r["subject"]: r["attempt"] - (r["max_attempts"] - JOB_UNIT_ATTEMPTS) for r in rows if "/" in r["subject"]}


def sender_state(p, sender):
    doc = p.mj("sender", "show", "lab", sender)
    h = doc.get("health") or doc.get("sender", {}).get("health") or {}
    return h.get("state", "")


# ---- a peer behind TLS ----

def make_certificates(p):
    """A throwaway authority and a certificate for 127.0.0.1 it signed, made
    with openssl in the work directory; the authority goes where a sender's
    TLS files are read, <data-dir>/tls/<tenant>/."""
    d = os.path.join(p.work, "tls-peer")
    os.makedirs(d)

    def ossl(*args):
        subprocess.run(["openssl", *args], cwd=d, check=True, capture_output=True, timeout=60)
    ossl("req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes", "-keyout", "ca.key",
         "-out", "ca.pem", "-days", "2", "-subj", "/CN=NoPACs proof authority")
    ossl("req", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes", "-keyout", "peer.key", "-out", "peer.csr",
         "-subj", "/CN=127.0.0.1")
    with open(os.path.join(d, "ext"), "w") as f:
        f.write("subjectAltName=IP:127.0.0.1\nextendedKeyUsage=serverAuth\n")
    ossl("x509", "-req", "-in", "peer.csr", "-CA", "ca.pem", "-CAkey", "ca.key", "-CAcreateserial", "-out", "peer.pem",
         "-days", "2", "-extfile", "ext")
    tls = os.path.join(p.data, "tls", "lab")
    os.makedirs(tls, exist_ok=True)
    with open(os.path.join(d, "ca.pem"), "rb") as src, open(os.path.join(tls, "proof-ca.pem"), "wb") as dst:
        dst.write(src.read())
    return d


def over_tls(p, port, held, labels):
    """8: a scan sent by DIMSE over TLS to dcmtk's storescp (+tls), whose
    certificate a throwaway authority signed: every data set arrives byte for
    byte; a sender that trusts another authority fails the TLS step."""
    tool = os.path.join(p.args.dcmtk, "storescp") if p.args.dcmtk else "storescp"
    d = make_certificates(p)
    out = os.path.join(d, "received")
    os.makedirs(out)
    # -ic: the peer does not ask for a client certificate, as a site that
    # authenticates the server only.
    scp = subprocess.Popen([tool, "+tls", "peer.key", "peer.pem", "-ic", "-aet", "TLSPEER", "-od", out, str(port)],
                           cwd=d, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        time.sleep(0.5)
        p.m("sender", "add", "lab", "tls", "--type", "dimse", "--host", "127.0.0.1", "--port", str(port), "--called-aet", "TLSPEER",
            "--calling-aet", "NOPACS", "--tls-ca-file", "proof-ca.pem", "--enabled")
        test = p.m("sender", "test", "lab", "tls").strip().splitlines()[-1]
        j = p.wait_job(p.job("tls", "%s/%s" % labels))
        got = {}
        for name in os.listdir(out):
            b = open(os.path.join(out, name), "rb").read()
            got[str(dcmread(os.path.join(out, name), stop_before_pixels=True).SOPInstanceUID)] = split_part10(b)[1]
        equal = sum(1 for u, b in held.items() if got.get(u) == split_part10(b)[1])
        good = j["state"] == "JOB_STATE_DONE" and equal == len(held) and len(got) == len(held)
        p.result("8 a session sent by DIMSE over TLS to dcmtk's storescp arrives whole", good,
                 "sender test: %s; job %s (%s); storescp wrote %d files, data sets byte-equal to the archived: %d of %d"
                 % (test, j["state"], p.counts_of(j), len(got), equal, len(held)))
        # The same peer, trusted through another authority: the handshake fails.
        other = os.path.join(p.data, "tls", "lab", "other-ca.pem")
        subprocess.run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes",
                        "-keyout", os.path.join(d, "other.key"), "-out", other, "-days", "2", "-subj", "/CN=another authority"],
                       check=True, capture_output=True, timeout=60)
        p.m("sender", "add", "lab", "tls-wrong", "--type", "dimse", "--host", "127.0.0.1", "--port", str(port), "--called-aet",
            "TLSPEER", "--calling-aet", "NOPACS", "--tls-ca-file", "other-ca.pem", "--enabled")
        wrong = p.run("sender", "test", "lab", "tls-wrong", "--json")
        steps = json.loads(wrong.stdout).get("results", []) if wrong.stdout else []
        tls_step = [s for s in steps if s.get("step") == "SENDER_TEST_STEP_TLS"]
        good = wrong.returncode == 1 and tls_step and tls_step[0].get("outcome") == "SENDER_TEST_OUTCOME_FAILED"
        p.result("8 a sender that trusts another authority fails at the TLS step", bool(good),
                 "exit %d; the TLS step: %s" % (wrong.returncode, json.dumps(tls_step[0]) if tls_step else "-"))
        p.m("sender", "remove", "lab", "tls-wrong", "--yes")
    finally:
        scp.terminate()
        scp.wait(30)


# ---- a peer down, then up ----

def down_then_up(p, port, held, labels):
    """9: a sender whose peer listens nowhere goes down after three failed
    connections, its units spending at most two attempts; the job waits,
    and once the peer listens the next try brings the sender up and the job
    ends done, with no attempt spent while it was down."""
    p.m("sender", "add", "lab", "later", "--type", "dimse", "--host", "127.0.0.1", "--port", str(port), "--called-aet", "PEER",
        "--calling-aet", "NOPACS", "--enabled")
    job = p.job("later", "%s/%s" % labels)
    deadline = time.time() + 180
    while sender_state(p, "later") != "SENDER_STATE_DOWN" and time.time() < deadline:
        time.sleep(0.5)
    went_down = sender_state(p, "later") == "SENDER_STATE_DOWN"
    at_down = spent_attempts(p, "later")
    time.sleep(15)  # past the first try again, which fails while nothing listens
    still = p.get(job)
    while_down = spent_attempts(p, "later")
    peer = sendkit.Peer(port)
    try:
        j = p.wait_job(job, timeout=240)
        after = spent_attempts(p, "later")
        received = set(peer.received)
    finally:
        peer.stop()
    worst = max(list(after.values()) + [0])
    good = (went_down and still["state"] not in ("JOB_STATE_FAILED", "JOB_STATE_DONE") and j["state"] == "JOB_STATE_DONE"
            and worst <= SPENT_BEFORE_DOWN and while_down == at_down and received == set(held))
    p.result("9 a peer down, then up: the job waits without spending attempts and ends done", good,
             "sender down: %s; attempts spent per unit when it went down %s, 15 s later %s, at the end %s (at most %d allowed); "
             "job while down %s, at the end %s (%s); the peer received %d of %d"
             % (went_down, at_down, while_down, after, SPENT_BEFORE_DOWN, still["state"], j["state"], p.counts_of(j),
                len(received & set(held)), len(held)))


# ---- a damaged archived byte ----

def damaged(p, port, scan_port):
    """10: one archived file changed on the storage after it was archived:
    the peer never receives it, the job lists it as damaged_in_storage and
    sends the rest."""
    sources = [os.path.join(sendkit.CORPUS, f) for f in CT]
    files, study = rewrite(sources, os.path.join(p.work, "studies", "damaged"), "PRV-SEND-DMG", "Prove^Damaged")
    held, labels = p.archive(scan_port, files, study)
    rows = query(p.catalog(), sendkit.HELD, study)
    victim = sorted(rows, key=lambda r: r["uid"])[0]
    path = os.path.join(p.storage(), victim["storage_key"])
    with open(path, "r+b") as f:
        f.seek(victim["byte_offset"] + (victim["byte_length"] or os.path.getsize(path)) - 10)
        b = f.read(1)
        f.seek(-1, os.SEEK_CUR)
        f.write(bytes([b[0] ^ 0xFF]))
    peer = sendkit.Peer(port)
    try:
        p.m("sender", "add", "lab", "dmg", "--type", "dimse", "--host", "127.0.0.1", "--port", str(port), "--called-aet", "PEER",
            "--calling-aet", "NOPACS", "--enabled")
        j = p.wait_job(p.job("dmg", "%s/%s" % labels))
        fails = p.mj("job", "failures", "lab", j["id"], "--all")["failures"]
        received = set(peer.received)
    finally:
        peer.stop()
    listed = [f for f in fails if f.get("code") == "JOB_FAILURE_CODE_DAMAGED_IN_STORAGE"]
    good = (j["state"] == "JOB_STATE_PARTIAL" and victim["uid"] not in received and received == set(held) - {victim["uid"]}
            and len(listed) == 1 and listed[0].get("instanceId") == victim["uid"])
    p.result("10 an archived byte changed: never sent, listed damaged_in_storage, the rest sent", good,
             "job %s (%s); the peer received %d of %d, the damaged one: %s; failures %s"
             % (j["state"], p.counts_of(j), len(received), len(held), "yes" if victim["uid"] in received else "no",
                [(f.get("code"), f.get("instanceId") == victim["uid"]) for f in fails]))
    return held, labels


# ---- a peer out of resources, then not ----

def resources_then_retry(p, peer, held, labels):
    """11: a peer that answers one instance A700 (out of resources) has it
    tried again later in the same job, which ends done with every instance
    received and that one received twice."""
    uid = sorted(held)[0]
    peer.statuses, peer.received = {}, []
    peer.once = {uid: 0xA700}
    j = p.wait_job(p.job("peer", "%s/%s" % labels), timeout=240)
    received = list(peer.received)
    good = (j["state"] == "JOB_STATE_DONE" and set(received) == set(held) and received.count(uid) == 2
            and int(j["counts"]["instancesRefused"]) == 0)
    p.result("11 a peer out of resources (A700) is tried again later and the job ends done", good,
             "job %s (%s); the peer answered A700 once for one instance, which it received %d times; it received %d of %d"
             % (j["state"], p.counts_of(j), received.count(uid), len(set(received) & set(held)), len(held)))


# ---- secrets ----

def secrets(p, token):
    """12: a stow sender's credential appears in no file of the data
    directory (the databases with their write-ahead logs, the tenant's
    events and its audit among them), not in the server's log and not in
    the audit log as the API lists it."""
    tok = token.encode()
    found, searched = [], 0
    for root, _, names in os.walk(p.data):
        for n in names:
            path = os.path.join(root, n)
            try:
                b = open(path, "rb").read()
            except OSError:
                continue
            searched += 1
            if tok in b:
                found.append(os.path.relpath(path, p.data))
    log = open(os.path.join(p.work, "serve.log"), "rb").read()
    audit = p.m("audit", "list", "--all", "--json")
    events = query(p.catalog(), "SELECT count(*) AS n FROM events")[0]["n"]
    in_log, in_audit = tok in log, token in audit
    good = not found and not in_log and not in_audit and searched > 0
    p.result("12 the stow credential is in no database file, log or audit entry", good,
             "searched %d files of the data directory (the tenant's catalog holds %d events), %d bytes of log, %d of audit: "
             "files holding it %s; log %s; audit %s" % (searched, events, len(log), len(audit), found or "none", in_log, in_audit))


# ---- role refusals ----

def refusals(p, api_port, labels):
    """13: a key with the reader role on the tenant may read its jobs and
    may not make one: refused (exit 3), and the refusal is in the audit log
    with the key's id."""
    out = p.m("key", "create", "--holder", "proof reader", "--no-expiry")
    key = out.splitlines()[0].strip()
    key_id = key.split("_")[1]
    p.m("grant", "set", key_id, "--role", "reader", "--tenant", "lab")
    env = dict(os.environ, NOPACS_KEY=key)
    server = "http://127.0.0.1:%d" % api_port

    def as_reader(*args):
        return subprocess.run([p.bin, "--server", server, *args], capture_output=True, text=True, timeout=120, env=env)
    listed = as_reader("job", "list", "lab", "--json")
    made = as_reader("job", "create", "lab", "--sender", "peer", "%s/%s" % labels)
    added = as_reader("sender", "add", "lab", "x", "--type", "dimse", "--host", "127.0.0.1", "--port", "104",
                      "--called-aet", "X", "--calling-aet", "NOPACS")
    time.sleep(1)  # a refusal's entry is written in a batch
    entries = p.mj("audit", "list", "--all")["entries"]
    refused = [e for e in entries if e.get("keyId") == key_id and e.get("outcome") == "AUDIT_OUTCOME_REFUSED"]
    methods = sorted(e.get("method", "").rsplit("/", 1)[-1] for e in refused)
    good = (listed.returncode == 0 and made.returncode == 3 and added.returncode == 3 and "CreateJob" in methods
            and "AddSender" in methods and key not in made.stderr + added.stderr)
    p.result("13 a reader is refused making a job or a sender, and each refusal is audited", good,
             "job list: exit %d; job create: exit %d (%s); sender add: exit %d; refused entries of the key: %s"
             % (listed.returncode, made.returncode, made.stderr.strip()[:90], added.returncode, methods))


# ---- a kill on Postgres ----

def on_postgres(p, orthanc, port, aet, o_host, o_port):
    """14: a tenant on Postgres sends a session to Orthanc; the server killed
    with signal 9 midway and started again, the job ends done and Orthanc
    holds each instance once."""
    if not (p.args.postgres and p.args.psql):
        p.skip("14 a kill during a send from a tenant on Postgres", "no --postgres and --psql given")
        return
    db = "prove_sending_%d" % int(time.time() * 1000)
    psql = shlex.split(p.args.psql)
    made = subprocess.run(psql + ["-d", "postgres", "-v", "ON_ERROR_STOP=1", "-tAq", "-c", "CREATE DATABASE " + db],
                          capture_output=True, text=True, timeout=60)
    if made.returncode != 0:
        p.skip("14 a kill during a send from a tenant on Postgres", "psql could not make a database: %s" % made.stderr.strip()[:200])
        return
    try:
        base = urllib.parse.urlsplit(p.args.postgres)
        os.environ["PROVE_PG_ADDRESS"] = urllib.parse.urlunsplit((base.scheme, base.netloc, "/" + db, base.query, ""))
        # The server reads the variable at the call; it runs with this
        # process's environment from its next start, so it is started again.
        p.end(signal.SIGTERM)
        p.serve()
        p.m("tenant", "create", "pg", "--db-env", "PROVE_PG_ADDRESS")
        p.m("setting", "set", "receiver.ports", str(port), "--tenant", "pg")
        p.m("setting", "set", "receiver.allow-as-received", "true", "--tenant", "pg")
        p.m("storage", "add", "pg", "main", "--type", "dir", "--root", "fast", "--path", "pg")
        p.m("receiver", "add", "pg", "scanner", "--storage", "main", "--port", str(port), "--aet", "ARCHIVE",
            "--quiet-period", "5s", "--allow-aet", "STORESCU", "--as-received", "--enabled")
        p.m("sender", "add", "pg", "slow", "--type", "dimse", "--host", o_host, "--port", o_port, "--called-aet", aet,
            "--calling-aet", "NOPACS", "--max-associations", "1", "--max-bytes-per-second", "64KiB", "--enabled")
        deadline = time.time() + 30
        while time.time() < deadline:
            r = p.mj("receiver", "show", "pg", "scanner")
            if (r.get("health") or r.get("receiver", {}).get("health") or {}).get("state") == "RECEIVER_STATE_UP":
                break
            time.sleep(0.2)
        files, study = many(os.path.join(sendkit.CORPUS, CT[0]), os.path.join(p.work, "studies", "pg"), 120, "PRV-SEND-PG")
        want = set(sop_uids(files))
        if p.storescu(port, files) != len(files):
            raise Failure("storescu: the Postgres tenant's receiver did not take every file")
        deadline = time.time() + 180
        while time.time() < deadline:
            rs = p.mj("reception", "list", "pg").get("receptions", [])
            if rs and all(r.get("step") == "RECEPTION_STEP_DONE" for r in rs):
                break
            time.sleep(0.5)
        subjects = p.mj("catalog", "subjects", "pg").get("subjects", [])
        sessions = p.mj("catalog", "sessions", "pg", subjects[0]["label"]).get("sessions", []) if subjects else []
        orthanc.clear()
        before = orthanc.discarded()
        job = p.mj("job", "create", "pg", "--sender", "slow", "%s/%s" % (subjects[0]["label"], sessions[0]["label"]))["job"]["id"]
        deadline = time.time() + 120
        while orthanc.count() < len(want) // 3 and time.time() < deadline:
            time.sleep(0.05)
        at_kill = orthanc.count()
        p.end(signal.SIGKILL)
        p.serve()
        deadline = time.time() + 300
        j = p.mj("job", "get", "pg", job)["job"]
        while j["state"] not in ("JOB_STATE_DONE", "JOB_STATE_PARTIAL", "JOB_STATE_FAILED", "JOB_STATE_CANCELED") and time.time() < deadline:
            time.sleep(0.2)
            j = p.mj("job", "get", "pg", job)["job"]
        have, listed = orthanc.instances()
        missing = want - set(have)
        twice = None if before is None else orthanc.discarded() - before
        good = j["state"] == "JOB_STATE_DONE" and not missing and listed == len(want) and (twice is None or twice <= 64)
        p.result("14 a send from a tenant on Postgres killed by signal 9 midway ends done, each instance once", good,
                 "killed with %d of %d in Orthanc; job %s (%s); Orthanc lists %d, misses %d; sent twice: %s (at most 64)"
                 % (at_kill, len(want), j["state"], p.counts_of(j), listed, len(missing), "not read" if twice is None else twice))
        p.m("tenant", "remove", "pg", "--yes")
    finally:
        orthanc.clear()
        subprocess.run(psql + ["-d", "postgres", "-tAq", "-c", "DROP DATABASE IF EXISTS %s WITH (FORCE)" % db],
                       capture_output=True, text=True, timeout=60)
