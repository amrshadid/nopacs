"""Sections 17 and 19 of the proof (scripts/prove/reading.py): DICOMweb with
dicomweb-client, STOW-RS included, and every way in to what a tenant holds
closed to a caller without a valid key or a grant, each refusal audited."""

import json
import os
import subprocess
import sys
import time
import uuid

import pydicom
from pydicom.uid import generate_uid

from nopacs_scripts import read as rk

BASE58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"


def stow_study(state, tenant):
    """A new study for STOW-RS, made from the public CT files: new Study,
    Series and SOP Instance UIDs, nothing else changed. Returns its UID and
    the files' paths."""
    out = os.path.join(state.dir, "stow-" + tenant)
    os.makedirs(out, exist_ok=True)
    study, series, paths = generate_uid(), generate_uid(), []
    for i, name in enumerate(n for n in rk.SENT if n.startswith("ct")):
        ds = pydicom.dcmread(os.path.join(rk.CORPUS, name))
        ds.StudyInstanceUID, ds.SeriesInstanceUID = study, series
        ds.SOPInstanceUID = ds.file_meta.MediaStorageSOPInstanceUID = generate_uid()
        path = os.path.join(out, "%02d.dcm" % i)
        ds.save_as(path, enforce_file_format=True)
        paths.append(path)
    return str(study), paths


def cmd_dicomweb(work, args):
    state = rk.State(work)
    for t in state.tenants():
        name = t["name"]
        root = "https://127.0.0.1:%d/tenants/%s/dicom-web" % (state["port"], name)
        env = {"REQUESTS_CA_BUNDLE": state["ca"], "NOPACS_DICOMWEB_KEY": state.key("reader")}
        main_files = sorted(p for p, _ in rk.archived(state, name, "main").values())
        rc, out, full = rk.run_check(work, sys.executable, "dicomweb-client.py", [root] + main_files, env=env)
        work.result("17.1 dicomweb-client searches and loads every study of %s (%s), equal to pydicom's" % (name, t["db"]), rc == 0,
                    "QIDO-RS studies, series and instances; WADO-RS each instance, its metadata and bulk data, the first and last "
                    "frame of each image (multi-frame RLE and JPEG included), against the %d archived files; dicomweb-client.py: %s" % (
                        len(main_files), rk.last_line(out)), full.splitlines()[-20:])

        study, files = stow_study(state, name)
        other = os.path.join(rk.CORPUS, "MR_small.dcm")
        env = {"REQUESTS_CA_BUNDLE": state["ca"], "NOPACS_STOW_KEY": state.key("writer")}
        rc, out, full = rk.run_check(work, sys.executable, "stow-dicomweb-client.py", ["store", root, study, other] + files, env=env)
        work.result("17.2 dicomweb-client stores a study in %s by STOW-RS; a part of another study is refused with A900H" % name, rc == 0,
                    "%d files in one request to the stow receiver with the writer key: each listed with its Retrieve URL; MR_small "
                    "into the same study: 409, Failure Reason A900H; stow-dicomweb-client.py: %s" % (len(files), " / ".join(
                        x for x in out.strip().splitlines() if x.startswith("store:")) or rk.last_line(out)), full.splitlines()[-20:])

        done, seen = rk.wait_receptions(state, name, "web", len(files))
        rc, out, full = rk.run_check(work, sys.executable, "stow-dicomweb-client.py", ["compare", state.storage_dir(name, "web")] + files)
        work.result("17.3 the study stored by STOW-RS in %s is archived byte for byte, as its manifest says" % name, done and rc == 0,
                    "the web receiver's receptions: %s; compare (hashlib against each scan's manifest, pydicom against each file "
                    "sent, nothing archived twice or unsent): %s" % (seen, rk.last_line(out)), full.splitlines()[-20:])

        def ended(ev):
            r = ev.get("receptionStep") or {}
            return (ev.get("tenant") == name and r.get("receiver") == "web" and r.get("step") == "RECEPTION_STEP_DONE"
                    and int(r.get("counts", {}).get("filesArchived", 0)) == len(files))

        ev, evs = rk.watch_until(state, ended)
        steps = [e["receptionStep"]["step"].replace("RECEPTION_STEP_", "").lower() for e in evs
                 if e.get("tenant") == name and (e.get("receptionStep") or {}).get("receiver") == "web"]
        work.result("17.4 the STOW-RS reception of %s is in the event stream, done with every file archived" % name, ev is not None,
                    "MonitorService/Watch from the first event kept, on the local socket: the web receiver's steps %s%s" % (
                        steps, "; done with %s files archived" % ev["receptionStep"]["counts"]["filesArchived"] if ev else ""),
                    [json.dumps(e)[:200] for e in evs[-6:]])

        everything = main_files + sorted(p for p, _ in rk.archived(state, name, "web").values())
        env = {"REQUESTS_CA_BUNDLE": state["ca"], "NOPACS_DICOMWEB_KEY": state.key("reader")}
        rc, out, full = rk.run_check(work, sys.executable, "dicomweb-client.py", [root] + everything, env=env)
        work.result("17.5 the study stored by STOW-RS in %s is found and loaded by dicomweb-client with the rest" % name, rc == 0,
                    "every study again, the stored one with them (%d files); dicomweb-client.py: %s" % (len(everything), rk.last_line(out)),
                    full.splitlines()[-20:])
    return 0


def multipart(path):
    """A STOW-RS request body of one part, the file (PS3.18 10.5.1.1)."""
    boundary = uuid.uuid4().hex
    with open(path, "rb") as f:
        data = f.read()
    body = b"--" + boundary.encode() + b"\r\nContent-Type: application/dicom\r\n\r\n" + data + b"\r\n--" + boundary.encode() + b"--\r\n"
    return body, 'multipart/related; type="application/dicom"; boundary=' + boundary


def ways_in(state, tenant):
    """Every way in to what a tenant holds, each as (name, method, path,
    body, headers, audited method), aimed at things the tenant does hold."""
    held = rk.archived(state, tenant, "main")
    uid, (path, entry) = sorted(held.items())[0]
    _, subject, session, scan, _ = entry["key"].split("/")
    ds = pydicom.dcmread(path, stop_before_pixels=True)
    stow_body, stow_type = multipart(sorted(p for p, _ in rk.archived(state, tenant, "web").values())[0])
    jsonh = {"Content-Type": "application/json", "Connect-Protocol-Version": "1"}
    wado = "/tenants/%s/dicom-web/studies/%s/series/%s/instances/%s" % (tenant, ds.StudyInstanceUID, ds.SeriesInstanceUID, uid)
    return [
        ("catalog ListSubjects", "POST", "/nopacs.v1.CatalogService/ListSubjects", json.dumps({"tenant": tenant}), jsonh,
         "/nopacs.v1.CatalogService/ListSubjects"),
        ("catalog ListScans", "POST", "/nopacs.v1.CatalogService/ListScans", json.dumps({"tenant": tenant}), jsonh,
         "/nopacs.v1.CatalogService/ListScans"),
        ("metadata GetScanMetadata", "POST", "/nopacs.v1.MetadataService/GetScanMetadata",
         json.dumps({"tenant": tenant, "subjectLabel": subject, "sessionLabel": session, "scanLabel": scan}), jsonh,
         "/nopacs.v1.MetadataService/GetScanMetadata"),
        ("data ReadFiles (stream)", "POST", "/nopacs.v1.DataService/ReadFiles", rk.stream_frame({"tenant": tenant, "subjectLabel": subject}),
         {"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"}, "/nopacs.v1.DataService/ReadFiles"),
        ("download route", "GET", rk.instance_path(tenant, entry["key"]), None, {},
         "GET /tenants/{tenant}/instances/{subject}/{session}/{scan}/{instance}"),
        ("DICOMweb QIDO-RS", "GET", "/tenants/%s/dicom-web/studies" % tenant, None, {"Accept": "application/dicom+json"},
         "GET /tenants/{tenant}/dicom-web/{rest...}"),
        ("DICOMweb WADO-RS", "GET", wado, None, {"Accept": 'multipart/related; type="application/dicom"'},
         "GET /tenants/{tenant}/dicom-web/{rest...}"),
        ("STOW-RS", "POST", "/tenants/%s/dicom-web/studies" % tenant, stow_body, {"Content-Type": stow_type, "Accept": "application/dicom+json"},
         "POST /tenants/{tenant}/dicom-web/{rest...}"),
    ]


def refusal(status, headers, body):
    """What a refusal says: its status, its error's code and message (in a
    Connect stream, those of the end of the stream, which carries the
    error), and the WWW-Authenticate challenge; also whether the body held
    anything but the error."""
    err, extra = {}, False
    try:
        if headers.get("content-type", "").startswith("application/connect+"):
            fr = rk.frames(body)
            extra = any(not f & 2 for f, _ in fr)
            err = next((m.get("error", {}) for f, m in fr if f & 2), {})
        else:
            err = json.loads(body or b"{}")
            extra = set(err) - {"code", "message"} != set()
    except ValueError:
        extra = True
    return (status, err.get("code", ""), err.get("message", ""), headers.get("www-authenticate", "")), extra


def audit_rows(state, sqlite3, ids):
    """The audit entries of these request ids, from the master database by
    sqlite3 and from each tenant's log (its SQLite file by sqlite3, or the
    log listed through the API for a tenant on Postgres)."""
    rows = {}
    quoted = ",".join("'%s'" % i for i in ids if i.isalnum())
    sql = "SELECT request_id, outcome, key_id, method, tenant_name, reason FROM audit WHERE request_id IN (%s)" % quoted
    dbs = [os.path.join(state["data"], "master.db")]
    dbs += [os.path.join(state["data"], "tenants", t["name"] + ".db") for t in state.tenants() if t["db"] == "SQLite"]
    for db in dbs:
        p = subprocess.run([sqlite3, "-json", "file:%s?mode=ro" % db, sql], capture_output=True, text=True, timeout=60)
        for r in json.loads(p.stdout or "[]"):
            rows[r["request_id"]] = dict(r, log=os.path.basename(db))
    for t in state.tenants():
        if t["db"] == "SQLite":
            continue
        rc, out, _ = rk.cli(state, "--json", "audit", "list", "--tenant", t["name"], "--outcome", "refused", "--all")
        for e in (json.loads(out).get("entries", []) if rc == 0 else []):
            if e.get("requestId") in ids:
                rows[e["requestId"]] = {"request_id": e["requestId"], "outcome": "refused", "key_id": e.get("keyId", ""),
                                        "method": e.get("method", ""), "tenant_name": e.get("tenant", ""), "reason": e.get("reason", ""),
                                        "log": t["name"]}
    return rows


def cmd_closed(work, args):
    state = rk.State(work)
    wrong = "nopacs_%s_%s" % ("".join(BASE58[b % 58] for b in os.urandom(12)), "".join(BASE58[b % 58] for b in os.urandom(44)))
    with open(os.path.join(work.path, "secrets"), "a") as f:
        f.write(wrong + "\n")
    callers = [("no key", None, ""), ("a wrong key", wrong, wrong.split("_")[1]),
               ("a key without a grant", state.key("nogrant"), state.key_id("nogrant"))]
    unauth = (401, "unauthenticated", "the request carries no valid key", "Bearer")
    denied = (403, "permission_denied", "the key may not do this", "")
    sent = []  # (tenant, way, caller, request id, audited method, key id)
    for t in state.tenants():
        name = t["name"]
        before = rk.receptions(state, name, "web")[1]
        ways = ways_in(state, name)
        bad_auth, bad_grant, leaked = [], [], []
        for way, method, path, body, headers, audited in ways:
            answers = {}
            for caller, key, key_id in callers:
                status, h, data = rk.request(state, method, path, key, body, headers)
                said, extra = refusal(status, h, data)
                answers[caller] = data
                sent.append((name, way, caller, h.get("nopacs-request-id", ""), audited, key_id))
                if extra:
                    leaked.append("%s, %s: the answer holds more than the error" % (way, caller))
                # A Connect stream answers 200 and carries its error in the
                # message that ends it (the Connect protocol's end-of-stream
                # message); a unary call and an HTTP route answer the
                # error's status.
                ok_status = 200 if way.endswith("(stream)") else None
                if caller == "a key without a grant":
                    if said != (ok_status or denied[0],) + denied[1:]:
                        bad_grant.append("%s: %s" % (way, said))
                elif said != (ok_status or unauth[0],) + unauth[1:]:
                    bad_auth.append("%s, %s: %s" % (way, caller, said))
            if answers["no key"] != answers["a wrong key"]:
                bad_auth.append("%s: no key and a wrong key are answered differently" % way)
        after = rk.receptions(state, name, "web")[1]
        if before != after:
            leaked.append("the web receiver's receptions changed: %s, then %s" % (before, after))
        listed = ", ".join(w[0] for w in ways)
        work.result("19.1 no key and a wrong key are refused the same way on every way in to %s" % name, not bad_auth,
                    "%s: each 401 (the stream: 200 and its end-of-stream error), unauthenticated, 'the request carries no "
                    "valid key', WWW-Authenticate Bearer; the two answers byte for byte the same on each" % listed, bad_auth)
        work.result("19.2 a key without a grant on %s is refused the same way on every way in" % name, not bad_grant,
                    "the same %d ways in: each 403 (the stream: 200 and its end-of-stream error), permission_denied, 'the key may "
                    "not do this'" % len(ways), bad_grant)
        work.result("19.3 a refused request reads and stores nothing in %s" % name, not leaked,
                    "every refusal's body is its error alone (a stream's, its end alone, with no message before it); the STOW-RS "
                    "requests started no reception (%s before and after)" % after, leaked)

    time.sleep(1)  # the audit writer writes what it holds every 200 ms
    rows = audit_rows(state, args.sqlite3, [s[3] for s in sent])
    problems, logs = [], {}
    for tenant, way, caller, rid, audited, key_id in sent:
        r = rows.get(rid)
        if not rid or r is None:
            problems.append("%s, %s, %s: request id %r not in any audit log" % (tenant, way, caller, rid))
            continue
        logs[r["log"]] = logs.get(r["log"], 0) + 1
        if r["outcome"] != "refused" or r["key_id"] != key_id or r["method"] != audited or not r["reason"]:
            problems.append("%s, %s, %s: audited as %s" % (tenant, way, caller, {k: r[k] for k in ("outcome", "key_id", "method", "reason")}))
    reasons = sorted({r["reason"] for r in rows.values()})
    work.result("19.4 each refusal is in the audit log, refused, with the key it carried and its reason", not problems and bool(sent),
                "%d refusals found by their request ids (%s) with sqlite3: outcome refused, the key's public id (none for no key), "
                "the route or procedure, reasons %s" % (len(sent), ", ".join("%d in %s" % (n, k) for k, n in sorted(logs.items())), reasons),
                problems)
    return 0
