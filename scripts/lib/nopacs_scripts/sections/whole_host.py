"""The groups of the proof of keeping the archive whole that lose the host:
a restore onto a fresh data directory, measured; an event cursor from before
a restore; and one writer per storage across a move of the tenant. Each
reads the outcome with tools that are not NoPACs: dcmtk's storescu, curl,
hashlib, os.walk, sqlite3, and the event feed read by Python's standard
library.

  fresh    the data directory and its incoming area destroyed, restored on a
           fresh directory: the report's losses per receiver against the
           proof's own tally, the credentials to give again, the time taken
  cursor   a cursor from before a restore refused with EVENT_HISTORY_CHANGED;
           the new history begins with catalog_rebuilt
  writer   export from A, import into B on the same storages: A refuses
           arrivals and, restarted, is fenced: no reception, no purge, no
           reclaim, and no read (TENANT_FENCED), its state still shown
"""

import hashlib
import os
import subprocess
import time

from nopacs_scripts.db import query
from nopacs_scripts.net import free_port
from nopacs_scripts.proof import machine
from nopacs_scripts.report import Failure
from nopacs_scripts.sections.whole import instance_route, objects_under, row_of, scene
from nopacs_scripts.whole import listing, make_study
from nopacs_scripts.wholemore import error_text, server_errors, stream

SLOW_QUIET = "120s"  # a quiet period no step of the group waits out: its reception stays unfinished


def backup_and_destroy(p):
    """Stops the server, makes a backup, keeps a copy of the key file apart
    and removes the whole data directory; returns (backup, key copy)."""
    p.stop()
    backup, keyfile = os.path.join(p.work, "host.backup"), os.path.join(p.work, "key.apart")
    p.m("backup", "create", "--out", backup)
    subprocess.run(["cp", os.path.join(p.data, "key"), keyfile], check=True)
    subprocess.run(["rm", "-rf", p.data], check=True)
    return backup, keyfile


def du(path):
    """The files and bytes under a directory, by os.walk."""
    n = size = 0
    for d, _, names in os.walk(path):
        for x in names:
            n += 1
            size += os.path.getsize(os.path.join(d, x))
    return n, size


def openssl_pair(where):
    """A self-signed certificate and its private key made by openssl, as a
    site's TLS files for a sender: ca.pem, client.pem and client.key."""
    os.makedirs(where, exist_ok=True)
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", os.path.join(where, "client.key"), "-out",
                    os.path.join(where, "client.pem"), "-subj", "/CN=prove-peer", "-days", "2"], check=True, capture_output=True)
    subprocess.run(["cp", os.path.join(where, "client.pem"), os.path.join(where, "ca.pem")], check=True)


# ---- group: fresh ----

def group_fresh(p):
    p.setup()
    s = scene(p, deid=False)
    # A sender whose TLS files are in the data directory (the tenant's TLS
    # directory): credentials the site gave that a backup does not hold.
    peer_port = free_port()
    openssl_pair(os.path.join(p.data, "tls", "lab"))
    p.m("setting", "set", "sender.destinations", "127.0.0.1 ports=%d" % peer_port)
    p.m("sender", "add", "lab", "peer", "--type", "dimse", "--host", "127.0.0.1", "--port", str(peer_port), "--called-aet", "PEER",
        "--calling-aet", "NOPACS", "--tls-ca-file", "ca.pem", "--tls-certificate-file", "client.pem", "--tls-private-key-file", "client.key",
        "--tls-server-name", "prove-peer")
    # A receiver whose quiet period no step waits out: what it was sent is
    # acknowledged and still in the incoming area at the backup.
    slow = free_port()
    p.m("setting", "set", "receiver.ports", "%d,%d,%d" % (p.raw_port, p.deid_port, slow), "--tenant", "lab")
    p.m("receiver", "add", "lab", "slow", "--storage", "main", "--port", str(slow), "--aet", "ARCHIVE", "--allow-aet", "STORESCU",
        "--quiet-period", SLOW_QUIET, "--as-received", "--enabled")
    files = make_study(p.args.pydicom_corpus, os.path.join(p.work, "studies"), "Slow^Five", "SL-5", ["CT_small.dcm", "MR_small.dcm", "CT_small.dcm"])
    sent, _ = p.storescu(slow, files)
    time.sleep(1.0)
    rs = p.mj("reception", "list", "lab", "--receiver", "slow", "--all").get("receptions", [])
    open_rs = [r for r in rs if r["step"] not in ("RECEPTION_STEP_DONE", "RECEPTION_STEP_QUARANTINED", "RECEPTION_STEP_FAILED")]
    in_files, in_bytes = du(os.path.join(p.data, "incoming"))
    held = len(p.held())
    p.note("the proof's tally before the backup: storescu sent %d files to slow, answered success; reception list shows %d unfinished; "
           "the incoming area holds %d files, %d bytes; the catalog holds %d" % (sent, len(open_rs), in_files, in_bytes, held))
    backup, keyfile = backup_and_destroy(p)
    p.data = os.path.join(p.work, "fresh")
    t0 = time.time()
    r = p.run("backup", "restore", backup, "--key-file", keyfile)
    t_restore = time.time() - t0
    report = r.stdout + r.stderr
    if r.returncode != 0:
        raise Failure("restore exit %d: %s" % (r.returncode, report[-600:]))
    p.serve()
    p.m("key", "reinstate", p.key_id)
    row = row_of(p, s.uids["al1"][0])
    code, body = 0, b""
    while time.time() < t0 + 120:
        code, _, body, _ = p.curl(instance_route(row))
        if code == 200:
            break
        time.sleep(0.2)
    t_read = time.time() - t0
    print("     the restore reported: %s" % " | ".join(x.strip() for x in report.strip().splitlines() if x.strip())[:2200], flush=True)
    lost_line = [x.strip() for x in report.splitlines() if x.strip().startswith("receiver slow:")]
    want = "receiver slow: %d receptions unfinished, %d files" % (len(open_rs), sent)
    p.result("9 the report names what was lost per receiver, equal to the proof's tally (storescu's successes, reception list before the backup)",
             r.returncode == 0 and lost_line and lost_line[0].startswith(want) and not [x for x in report.splitlines() if x.strip().startswith("receiver raw:")],
             "the report says %r; the tally: %r; nothing named lost of raw, whose %d files were archived" % (lost_line[0] if lost_line else None, want, held),
             report.splitlines()[-14:])
    tls_left = os.path.exists(os.path.join(p.data, "tls", "lab", "client.key"))
    named = [x.strip() for x in report.splitlines() if "client.key" in x or "ca.pem" in x or "credential" in x.lower() or "tls" in x.lower()]
    p.result("9 the report names the external credentials to give again: the sender's TLS files that lived in the destroyed data directory",
             bool(named), "the restored data directory holds the TLS key: %s; lines of the report naming a credential or the files: %s" % (tls_left, named or "none"))
    held_after = len(p.held())
    p.result("9 the archive serves again from the fresh directory: the catalog rebuilt holds what was archived (sqlite3), a download its bytes (curl, hashlib)",
             held_after == held and code == 200 and hashlib.sha256(body).hexdigest() == row["sha256"],
             "%d of %d instances held; GET HTTP %d" % (held_after, held, code))
    p.result("9 the recovery time, measured: the restore %.1f s, the first download served %.1f s after the restore began" % (t_restore, t_read),
             code == 200, "on %s; %d instances in %d bytes of storage" % (machine(), held_after, sum(v[0] for v in listing(p.storage_root()).values())))


# ---- group: cursor ----

def events(p, body, seconds=4):
    """The events a Watch answers in a few seconds, and how it ended; the
    installation's on the server's own socket, as the local operator, since
    every tenant's feed needs the operator role on the installation."""
    st, msgs, end = stream(p, "nopacs.v1.MonitorService/Watch", body, timeout=seconds, local="installation" in body["scope"])
    return st, [m["event"] for m in msgs if "event" in m], [m["keepAlive"] for m in msgs if "keepAlive" in m], end


def history(p, alive):
    """The id of the tenant's event history, as its status or a keep-alive says it."""
    _, status = p.call("nopacs.v1.MonitorService/GetStatus", {"scope": {"tenant": "lab"}})
    said = [status["tenants"][0].get("eventHistory", "")] + [a.get("eventHistory", "") for a in alive] + \
        [a.get("tenantEventHistories", {}).get("lab", "") for a in alive]
    return status, next((x for x in said if x), "")


def group_cursor(p):
    p.setup()
    scene(p, deid=False)
    st, evs, alive, _ = events(p, {"scope": {"installation": {}}, "resume": True})
    mine = [e for e in evs if e.get("tenant") == "lab"]
    old_event = mine[-1]["cursor"] if mine else ""
    status, old_history = history(p, alive)
    old_status = status["tenants"][0].get("eventCursor", "")
    old_rows = [tuple(r) for r in query(p.catalog(), "SELECT * FROM event_history")]
    p.note("before the restore: %d events of lab read, the last %s; the status's cursor %s..., history %s" % (len(mine), mine[-1].get("type") if mine else None,
                                                                                                           old_status[:16], old_history))
    backup, keyfile = backup_and_destroy(p)
    r = p.run("backup", "restore", backup, "--key-file", keyfile)
    if r.returncode != 0:
        raise Failure("restore exit %d: %s" % (r.returncode, (r.stdout + r.stderr)[-600:]))
    p.serve()
    p.m("key", "reinstate", p.key_id)
    for name, cur in (("an event's cursor", old_event), ("the status's cursor", old_status)):
        st, evs2, _, end = events(p, {"scope": {"tenant": "lab"}, "cursor": cur}, seconds=8)
        text = error_text(end.get("error")) if st == 200 else error_text(end)
        p.result("10 %s from before the restore is refused with EVENT_HISTORY_CHANGED" % name, cur and "EVENT_HISTORY_CHANGED" in text and not evs2,
                 "HTTP %d, %d events (%s), the stream ended with: %s" % (st, len(evs2), [e.get("type", "").replace("EVENT_TYPE_", "").lower() for e in evs2[:4]],
                                                                          " ".join(text.split())[:240] or end))
    st, evs3, alive3, end = events(p, {"scope": {"installation": {}}, "resume": True})
    _, new_history = history(p, alive3)
    first = [e for e in evs3 if e.get("tenant") == "lab"]
    new_rows = [tuple(r) for r in query(p.catalog(), "SELECT * FROM event_history")]
    at = [e.get("type") for e in first].index("EVENT_TYPE_CATALOG_REBUILT") if "EVENT_TYPE_CATALOG_REBUILT" in [e.get("type") for e in first] else -1
    p.result("10 the new history's first event of the tenant is catalog_rebuilt, under a new history id", first and first[0].get("type") == "EVENT_TYPE_CATALOG_REBUILT"
             and new_history and new_history != old_history,
             "first events: %s; catalog_rebuilt is event %d of %d; history (status, keep-alive) %r -> %r; the event_history table (sqlite3) %s -> %s"
             % ([e.get("type", "").replace("EVENT_TYPE_", "").lower() for e in first[:4]], at + 1, len(first), old_history, new_history, old_rows, new_rows))
    if first:
        st, evs4, _, end = events(p, {"scope": {"tenant": "lab"}, "cursor": first[0]["cursor"]})
        p.result("10 a cursor of the new history is taken", st == 200 and "error" not in end, "HTTP %d, %d events after it, ended %s"
                 % (st, len(evs4), error_text(end.get("error")) or "by the client's timeout"))


# ---- group: writer ----

def tenant_state(p, data=None):
    r = p.run("tenant", "show", "lab", "--json", data=data)
    return r.stdout if r.returncode == 0 else r.stderr


def group_writer(p):
    p.setup()
    s = scene(p, deid=False)
    be = row_of(p, s.uids["be1"][0])
    al = row_of(p, s.uids["al1"][0])
    cid = p.mj("correction", "delete", "lab", be["subject"], "--reason", "consent withdrawn", "--yes")["correction"]["id"]
    held_before = len(p.held())
    export, tkey = os.path.join(p.work, "lab.export"), os.path.join(p.work, "lab.transfer")
    p.m("tenant", "export", "lab", "--out", export, "--key-out", tkey)
    more = make_study(p.args.pydicom_corpus, os.path.join(p.work, "studies"), "After^Export", "AE-1", ["CT_small.dcm"])
    n, out = p.storescu(p.raw_port, more)
    code, _, body, _ = p.curl(instance_route(al))
    st, subjects = p.call("nopacs.v1.CatalogService/ListSubjects", {"tenant": "lab", "pageSize": 100})
    p.result("11 after the export, A refuses an arrival (storescu) and still serves a read (curl, hashlib)", n == 0 and code == 200
             and hashlib.sha256(body).hexdigest() == al["sha256"], "storescu: %d of 1 answered success (%s); GET of an instance HTTP %d %s; ListSubjects HTTP %d"
             % (n, " ".join([x for x in out.splitlines() if "Association" in x or "Rejected" in x or "Failed" in x][:2])[:160], code,
                body[:80] if code != 200 else "", st), ["server log: " + x[:300] for x in server_errors(p) if "TENANT_MOVED" in x][:2])
    p.stop()
    # B: a second installation of its own key file, listing the same root
    a_data, b_data = p.data, os.path.join(p.work, "data-b")
    for args in (("init",),):
        r = p.run(*args, data=b_data)
        if r.returncode != 0:
            raise Failure("init of B: %s" % r.stderr)
    p.serve(data=b_data)
    r = p.run("setting", "set", "storage.directory-roots", "fast=" + p.roots, data=b_data)
    p.stop()
    r = p.run("tenant", "import", export, "--transfer-key", tkey, data=b_data)
    p.result("11 B imports the tenant on the same storages", r.returncode == 0, "import exit %d: %s" % (r.returncode, " | ".join((r.stdout + r.stderr).strip().splitlines()[-3:])[:300]))
    p.serve(data=b_data)
    bkey = p.run("key", "create", "--holder", "installation b", "--no-expiry", data=b_data).stdout.splitlines()[0].strip()
    p.run("grant", "set", bkey.split("_")[1], "--role", "admin", "--tenant", "lab", data=b_data)
    code, _, body, _ = p.curl(instance_route(al), key=bkey)
    p.result("11 B serves the tenant's instance from the same storage (curl, hashlib)", code == 200 and hashlib.sha256(body).hexdigest() == al["sha256"],
             "GET on B HTTP %d" % code)
    p.stop()
    # A again, on its own data directory
    p.serve(data=a_data)
    deadline, state = time.time() + 90, ""
    while time.time() < deadline:
        state = tenant_state(p)
        if "FENCED" in state.upper():
            break
        time.sleep(1)
    p.result("11 A restarted shows the tenant fenced (nopacs tenant show)", "TENANT_STATE_FENCED" in state or '"fenced"' in state.lower(),
             " ".join(state.split())[:300])
    # A fenced tenant serves no data: what B deletes or corrects on the shared
    # storage never reaches A's catalog, so A refuses every read, naming B
    # (the installation id B's data directory holds), and shows its state.
    with open(os.path.join(b_data, "installation"), encoding="ascii") as f:
        id_b = f.read().strip()
    code, _, body, _ = p.curl(instance_route(al))
    qcode, _, qbody, _ = p.curl("/tenants/lab/dicom-web/studies")
    st, answer = p.call("nopacs.v1.CatalogService/ListSubjects", {"tenant": "lab", "pageSize": 100})
    def fenced(c, b):
        text = b.decode("utf-8", "replace") if isinstance(b, bytes) else str(b)
        return c == 400 and "TENANT_FENCED: the tenant lab serves no data" in text and id_b in text and al["subject"] not in text
    p.result("11 the fenced A refuses reads with TENANT_FENCED naming B, and shows its state (curl, nopacs tenant show)",
             fenced(code, body) and fenced(qcode, qbody) and fenced(st, answer) and id_b in state
             and ("TENANT_STATE_FENCED" in state or '"fenced"' in state.lower()),
             "GET of an instance HTTP %d %s; QIDO-RS HTTP %d; ListSubjects HTTP %d %s" % (code, body[:200], qcode, st, str(answer)[:200]))
    n2, out2 = p.storescu(p.raw_port, more)
    p.result("11 the fenced A refuses a reception (storescu)", n2 == 0 and len(p.held()) == held_before,
             "storescu: %d of 1 answered success; A's catalog holds %d instances (it held %d)" % (n2, len(p.held()), held_before))
    before = listing(p.storage_root())
    r = p.run("correction", "purge", "lab", cid, "--wait", "--yes")
    left = objects_under(listing(p.storage_root()), "data/%s/" % be["subject"])
    p.result("11 the fenced A refuses a purge of a delete made before the export, and the objects stay (os.walk)", r.returncode != 0 and left
             and listing(p.storage_root()) == before, "purge exit %d: %s; %d objects of the subject still listed"
             % (r.returncode, r.stderr.strip()[:200], len(left)))
    r = p.run("tenant", "reclaim", "lab")
    p.result("11 nopacs tenant reclaim on A is refused, the record naming B", r.returncode != 0, "reclaim exit %d: %s" % (r.returncode, r.stderr.strip()[:240]))


GROUPS = {"fresh": group_fresh, "cursor": group_cursor, "writer": group_writer}
