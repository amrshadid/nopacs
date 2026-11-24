#!/usr/bin/env python3
"""The sections of scripts/prove/run.sh that read the archive, from outside.

They run on an installation of their own (made and served by section 13 of
run.sh, in the work directory's rd/), so the main server's audit log keeps
only the requests of its ledger. A tenant on SQLite, and one on Postgres
when the runner has a Postgres, each with a directory storage, receive
public studies from dcmtk's storescu; every later section reads what they
archived, with a tool that is not NoPACs, and compares it with what pydicom
reads of the archived files:

  setup     13: the tenants, storages, receivers and keys; the studies sent
            with storescu and archived; each archived data set the one sent
  catalog   14: every ListSubjects, ListSessions and ListScans answer, paged,
            equals pydicom's reading (scripts/check/catalog-pydicom.py)
  metadata  15: every scan's GetScanMetadata, paged, equals pydicom's
            to_json_dict but for bulk data (scripts/check/metadata-pydicom.py)
  downloads 16: every file whole, in ranges and resumed with curl
            (scripts/check/download-curl.py), random ranges equal the file's
            bytes, and "nopacs download" killed midway and run again leaves
            every file whole (sha256sum)
  dicomweb  17: dicomweb-client searches and loads every study, its
            metadata and frames (scripts/check/dicomweb-client.py), and
            stores a study by STOW-RS, which passes the reception checks
            (scripts/check/stow-dicomweb-client.py, and the events)
  client    18: the Python client of clients/python against the server
  closed    19: no key, a wrong key and a key without a grant refused the
            same way on every way in to what a tenant holds, each audited

Each check prints one line, ok or FAIL with what it compared, and is added
to the work directory's results.tsv (scripts/prove/steps.py). Needs pydicom,
dicomweb-client and requests (scripts/requirements/prove-reading.txt), and
grpcio and protobuf for the client (scripts/requirements/check-server.txt).
"""

import argparse
import json
import os
import random
import socket
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pydicom  # noqa: E402
from pydicom.uid import generate_uid  # noqa: E402

import readkit as rk  # noqa: E402
from steps import Work  # noqa: E402


# The storescu option that proposes a compressed transfer syntax, by its UID.
PROPOSE = {"1.2.840.10008.1.2.5": "-xr", "1.2.840.10008.1.2.4.50": "-xy"}


def make_key(state, name):
    """A key made by "nopacs key create", kept in rd/keys/NAME (0600) and
    added to the secrets no line may print."""
    out = rk.cli_ok(state, "key", "create", "--holder", "reading proof " + name, "--no-expiry")
    key = out.splitlines()[0].strip()
    path = os.path.join(state.dir, "keys", name)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    os.write(fd, (key + "\n").encode())
    os.close(fd)
    with open(os.path.join(state.work.path, "secrets"), "a") as f:
        f.write(key + "\n" + key.split("_")[2] + "\n")
    return key.split("_")[1]


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def wait_up(state, tenant, receiver, timeout=30):
    deadline = time.time() + timeout
    while time.time() < deadline:
        rc, out, _ = rk.cli(state, "--json", "receiver", "show", tenant, receiver)
        if rc == 0 and json.loads(out).get("receiver", {}).get("health", {}).get("state") == "RECEIVER_STATE_UP":
            return True
        time.sleep(0.1)
    return False


def comparable(ds):
    """A data set as pydicom reads it, for comparing two encodings of it:
    without group lengths (PS3.5 7.2, which a sender may drop or add) and
    Data Set Trailing Padding (PS3.10 7.2)."""
    d = ds.to_json_dict()
    return {k: v for k, v in d.items() if not k.endswith("0000") and k != "FFFCFFFC"}


def large_image(state):
    """A synthetic image of 4 MiB of pixels, made from MR_small's header
    with new UIDs, in a study of its own: a file larger than the 1 MiB a
    download's chunk holds, so a download stopped in the middle of it
    leaves a .part to continue. Its pixels are pseudo-random bytes from a
    fixed seed, the same on every run and no one's data."""
    ds = pydicom.dcmread(os.path.join(rk.CORPUS, "MR_small.dcm"))
    ds.StudyInstanceUID, ds.SeriesInstanceUID = generate_uid(), generate_uid()
    ds.SOPInstanceUID = ds.file_meta.MediaStorageSOPInstanceUID = generate_uid()
    ds.SeriesDescription, ds.SeriesNumber = "synthetic large image", 99
    ds.Rows, ds.Columns = 2048, 1024
    # Bytes that do not compress, so the download carries all of them
    # whatever the encoding of its stream (Connect may compress a message).
    ds.PixelData = random.Random(73).randbytes(2048 * 1024 * 2)
    path = os.path.join(state.dir, "large.dcm")
    ds.save_as(path, enforce_file_format=True)
    return path


def cmd_setup(work, args):
    state = rk.State(work)
    state.d = {"bin": args.bin, "data": args.data, "port": args.port, "ca": args.ca, "roots": os.path.join(state.dir, "roots"),
               "tenants": []}
    os.makedirs(os.path.join(state.dir, "keys"), mode=0o700, exist_ok=True)
    os.makedirs(state.d["roots"], mode=0o700, exist_ok=True)
    tenants = [{"name": "rlite", "db": "SQLite"}]
    if args.pg_env:
        tenants.append({"name": "rpg", "db": "Postgres", "env": args.pg_env})
    problems = []
    try:
        rk.cli_ok(state, "setting", "set", "storage.directory-roots", "fast=" + state.d["roots"])
        reader, writer = make_key(state, "reader"), make_key(state, "writer")
        make_key(state, "nogrant")
        for t in tenants:
            name = t["name"]
            create = ["--db-env", t["env"]] if t.get("env") else []
            rk.cli_ok(state, "tenant", "create", name, *create)
            t["dimse_port"] = free_port()
            rk.cli_ok(state, "setting", "set", "receiver.ports", str(t["dimse_port"]), "--tenant", name)
            rk.cli_ok(state, "setting", "set", "receiver.allow-as-received", "true", "--tenant", name)
            for storage in ("main", "web"):
                rk.cli_ok(state, "storage", "add", name, storage, "--type", "dir", "--root", "fast", "--path", storage)
            rk.cli_ok(state, "receiver", "add", name, "scanner", "--storage", "main", "--port", str(t["dimse_port"]), "--aet", "ARCHIVE",
                      "--allow-aet", "STORESCU", "--quiet-period", rk.QUIET, "--enabled")
            rk.cli_ok(state, "grant", "set", reader, "--role", "reader", "--tenant", name)
            rk.cli_ok(state, "grant", "set", writer, "--role", "writer", "--tenant", name)
            rk.cli_ok(state, "receiver", "add", name, "web", "--type", "stow", "--storage", "web", "--allow-key", writer,
                      "--quiet-period", rk.QUIET, "--enabled")
            for r in ("scanner", "web"):
                if not wait_up(state, name, r):
                    problems.append("%s: receiver %s is not up" % (name, r))
            state.d["tenants"].append(t)
    except RuntimeError as e:
        problems.append(str(e))
    state.save()
    names = ", ".join("%s on %s" % (t["name"], t["db"]) for t in state.tenants())
    work.result("13.1 a reading installation: tenants, directory storages, a DIMSE and a STOW receiver each, keys", not problems,
                "%s; storages main and web on a directory root; receivers scanner (DIMSE, port of its own) and web "
                "(STOW-RS, listing the writer key); keys reader, writer and one with no grant" % names, problems)
    if problems:
        return 1

    sources = [os.path.join(rk.CORPUS, f) for f in rk.SENT] + [large_image(state)]
    # storescu proposes the uncompressed transfer syntaxes alone unless told
    # to propose another, and cannot convert RLE or JPEG to them: each
    # compressed file is sent with the option that proposes its own (dcmtk's
    # storescu -xr, -xy), so every file is sent as it is encoded. -R
    # proposes only the SOP classes the files need: the segmentation's is
    # not among storescu's defaults.
    groups = {}
    for s in sources:
        ts = str(pydicom.dcmread(s, stop_before_pixels=True).file_meta.TransferSyntaxUID)
        groups.setdefault(PROPOSE.get(ts, ""), []).append(s)
    for t in state.tenants():
        answered, rcs, out = 0, [], ""
        for option, files in sorted(groups.items()):
            p = subprocess.run([args.storescu, "-v", "-R", *([option] if option else []), "-aet", "STORESCU", "-aec", "ARCHIVE",
                                "127.0.0.1", str(t["dimse_port"]), *files], capture_output=True, text=True, timeout=300)
            answered += (p.stdout + p.stderr).count("Received Store Response (Success)")
            rcs.append(p.returncode)
            out += p.stdout + p.stderr
        done, seen = rk.wait_receptions(state, t["name"], "scanner", len(sources))
        held = rk.archived(state, t["name"], "main")
        work.result("13.2 the studies sent with storescu to %s are archived" % t["name"],
                    set(rcs) == {0} and answered == len(sources) and done and len(held) == len(sources),
                    "storescu -R, -R -xr and -R -xy: %d of %d files answered success (exit %s); %s; %d files in the storage's "
                    "manifests" % (answered, len(sources), rcs, seen, len(held)), out.splitlines()[-8:])

        # What the archive holds is the data set that was sent: the later
        # sections compare the answers with the archived files, so this ties
        # them to the public files themselves.
        bad, by_uid = [], {}
        for s in sources:
            ds = pydicom.dcmread(s)
            by_uid[str(ds.SOPInstanceUID)] = ds
        for uid, (path, _) in held.items():
            src = by_uid.get(uid)
            got = pydicom.dcmread(path)
            if src is None:
                bad.append("%s: archived, not sent" % uid)
            elif comparable(got) != comparable(src):
                diff = sorted(k for k in set(comparable(got)) | set(comparable(src)) if comparable(got).get(k) != comparable(src).get(k))
                bad.append("%s: the data set differs from the file sent in %s" % (uid, diff[:6]))
        work.result("13.3 each file archived in %s is the data set sent, as pydicom reads both" % t["name"],
                    not bad and len(held) == len(sources),
                    "%d archived files against the %d files sent (the public files and one synthetic large image), "
                    "to_json_dict equal but for group lengths" % (len(held), len(sources)), bad)
    state.d["sent"] = sources
    state.save()
    return 0


def catalog_answers(state, tenant, key):
    """The catalog's answers for a tenant, in the form
    scripts/check/catalog-pydicom.py reads, every list paged three at a
    time; also the number of pages."""
    t = {"tenant": tenant}
    subjects, n1 = rk.pages(state, "nopacs.v1.CatalogService/ListSubjects", t, key, "subjects", 3)
    sessions, n2 = rk.pages(state, "nopacs.v1.CatalogService/ListSessions", t, key, "sessions", 3)
    scans, n3 = rk.pages(state, "nopacs.v1.CatalogService/ListScans", t, key, "scans", 3)
    out = {
        "subjects": [{"label": s["label"], "session_count": int(s.get("sessionCount", 0)), "instance_count": int(s.get("instanceCount", 0)),
                      "size_bytes": int(s.get("sizeBytes", 0))} for s in subjects],
        "sessions": [{"subject_label": s["subjectLabel"], "label": s["label"], "study_instance_uid": s["studyInstanceUid"],
                      "study_date": s.get("studyDate", ""), "modalities": s.get("modalities", []), "scan_count": int(s.get("scanCount", 0)),
                      "instance_count": int(s.get("instanceCount", 0)), "size_bytes": int(s.get("sizeBytes", 0))} for s in sessions],
        "scans": [],
    }
    for s in scans:
        row = {"subject_label": s["subjectLabel"], "session_label": s["sessionLabel"], "label": s["label"],
               "series_instance_uid": s["seriesInstanceUid"], "modality": s.get("modality", ""), "series_description": s.get("seriesDescription", ""),
               "instance_count": int(s.get("instanceCount", 0)), "size_bytes": int(s.get("sizeBytes", 0))}
        if "seriesNumber" in s:
            row["series_number"] = int(s["seriesNumber"])
        out["scans"].append(row)
    by_date, _ = rk.pages(state, "nopacs.v1.CatalogService/ListSessions", dict(t, order="CATALOG_ORDER_STUDY_DATE"), key, "sessions", 3)
    out["sessions_by_study_date"] = [s["studyInstanceUid"] for s in by_date]
    by_desc, _ = rk.pages(state, "nopacs.v1.CatalogService/ListScans", dict(t, order="CATALOG_ORDER_SERIES_DESCRIPTION"), key, "scans", 3)
    out["scans_by_series_description"] = [s["seriesInstanceUid"] for s in by_desc]
    out["sessions_by_modality"], out["scans_by_modality"] = {}, {}
    for m in sorted({s["modality"] for s in out["scans"] if s["modality"]}):
        got, _ = rk.pages(state, "nopacs.v1.CatalogService/ListSessions", dict(t, modality=m), key, "sessions", 3)
        out["sessions_by_modality"][m] = [s["studyInstanceUid"] for s in got]
        got, _ = rk.pages(state, "nopacs.v1.CatalogService/ListScans", dict(t, modality=m), key, "scans", 3)
        out["scans_by_modality"][m] = [s["seriesInstanceUid"] for s in got]
    return out, n1 + n2 + n3


def cmd_catalog(work, args):
    state = rk.State(work)
    key = state.key("reader")
    for t in state.tenants():
        name = t["name"]
        try:
            got, n_pages = catalog_answers(state, name, key)
        except RuntimeError as e:
            work.result("14.1 the catalog of %s (%s) equals pydicom's reading of the archived files" % (name, t["db"]), False, str(e))
            continue
        got["files"] = [p for p, _ in rk.archived(state, name, "main").values()]
        path = os.path.join(state.dir, "catalog-%s.json" % name)
        with open(path, "w") as f:
            json.dump(got, f)
        rc, out, full = rk.run_check(work, sys.executable, "catalog-pydicom.py", [path])
        work.result("14.1 the catalog of %s (%s) equals pydicom's reading of the archived files" % (name, t["db"]), rc == 0,
                    "%d subjects, %d sessions, %d scans over %d pages of three, the study date and series description orders and "
                    "each modality's lists; catalog-pydicom.py: %s" % (
                        len(got["subjects"]), len(got["sessions"]), len(got["scans"]), n_pages, rk.last_line(out)),
                    full.splitlines()[-20:])
    return 0


def cmd_metadata(work, args):
    state = rk.State(work)
    key = state.key("reader")
    for t in state.tenants():
        name = t["name"]
        held = rk.archived(state, name, "main")
        entries, problems, n_pages = [], [], 0
        try:
            scans, _ = rk.pages(state, "nopacs.v1.CatalogService/ListScans", {"tenant": name}, key, "scans", 100)
            for s in scans:
                body = {"tenant": name, "subjectLabel": s["subjectLabel"], "sessionLabel": s["sessionLabel"], "scanLabel": s["label"]}
                got, n = rk.pages(state, "nopacs.v1.MetadataService/GetScanMetadata", body, key, "instances", 2)
                n_pages += n
                if len(got) != int(s["instanceCount"]):
                    problems.append("scan %s: %d instances answered, the catalog counts %s" % (s["label"], len(got), s["instanceCount"]))
                for i in got:
                    uid = i.get("sopInstanceUid", "")
                    if i.get("jsonTooLarge") or uid not in held:
                        problems.append("instance %s: json_too_large %s, archived %s" % (uid, i.get("jsonTooLarge", False), uid in held))
                        continue
                    if i.get("transferSyntaxUid") != str(pydicom.dcmread(held[uid][0], stop_before_pixels=True).file_meta.TransferSyntaxUID):
                        problems.append("instance %s: transfer syntax %s is not the file's" % (uid, i.get("transferSyntaxUid")))
                    entries.append({"file": held[uid][0], "id": uid, "json": i["dicomJson"], "omitted": i.get("bulkDataOmitted", False)})
        except RuntimeError as e:
            problems.append(str(e))
        answered = {e["id"] for e in entries}
        if answered != set(held):
            problems.append("instances answered %d, archived %d" % (len(answered), len(held)))
        path = os.path.join(state.dir, "metadata-%s.json" % name)
        with open(path, "w") as f:
            json.dump(entries, f)
        rc, out, full = rk.run_check(work, sys.executable, "metadata-pydicom.py", [path])
        work.result("15.1 every scan's metadata in %s (%s) equals pydicom's to_json_dict of its files, but for bulk data" % (name, t["db"]),
                    rc == 0 and not problems,
                    "%d instances over %d pages of two, each with its transfer syntax; metadata-pydicom.py (each BulkDataURI's "
                    "offset and length read back from the file; Specific Character Set as ISO_IR 192): %s" % (
                        len(entries), n_pages, rk.last_line(out)), problems + full.splitlines()[-20:])
    return 0


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--work", required=True)
    sub = ap.add_subparsers(dest="cmd", required=True)
    s = sub.add_parser("setup")
    s.add_argument("--bin", required=True)
    s.add_argument("--data", required=True)
    s.add_argument("--port", type=int, required=True)
    s.add_argument("--ca", required=True)
    s.add_argument("--storescu", required=True)
    s.add_argument("--pg-env", default="", help="the variable holding a Postgres address for a second tenant")
    sub.add_parser("catalog")
    sub.add_parser("metadata")
    sub.add_parser("downloads")
    sub.add_parser("dicomweb")
    sub.add_parser("client")
    c = sub.add_parser("closed")
    c.add_argument("--sqlite3", default="sqlite3")
    args = ap.parse_args()
    work = Work(args.work)
    if args.cmd in ("downloads", "client"):
        import reading_files as mod
    elif args.cmd in ("dicomweb", "closed"):
        import reading_web as mod
    else:
        mod = sys.modules[__name__]
    fn = getattr(mod, "cmd_" + args.cmd)
    try:
        fn(work, args)
    except Exception as e:  # a section that cannot go on is one failed check, never a stopped proof
        work.result("%s: the section ran to its end" % args.cmd, False, "%s: %s" % (type(e).__name__, work.redact(str(e))))
    return 0


if __name__ == "__main__":
    sys.exit(main())
