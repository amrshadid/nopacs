"""Sections 16 and 18 of the proof (scripts/prove/reading.py): files
downloaded with curl and with "nopacs download", and the Python client of
clients/python against the real server."""

import hashlib
import json
import os
import random
import signal
import socket
import subprocess
import sys
import threading
import time

import pydicom

from nopacs_scripts import read as rk
from nopacs_scripts.paths import ROOT


def headers_file(state):
    """A file of request headers for curl's -H @file, holding the reader's
    key, so the key is never one of curl's arguments."""
    path = os.path.join(state.dir, "reader.h")
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    os.write(fd, ("Authorization: Bearer %s\n" % state.key("reader")).encode())
    os.close(fd)
    return path


def curl_range(state, hdr, url, spec, work_dir):
    """curl --range SPEC; returns (status, Content-Range, body)."""
    body, head = os.path.join(work_dir, "range.body"), os.path.join(work_dir, "range.head")
    p = subprocess.run(["curl", "-sS", "--cacert", state["ca"], "-H", "@" + hdr, "--range", spec, "-o", body, "-D", head,
                        "-w", "%{http_code}", url], capture_output=True, text=True, timeout=60)
    content_range = ""
    with open(head, encoding="latin-1") as f:
        for line in f:
            if line.lower().startswith("content-range:"):
                content_range = line.split(":", 1)[1].strip()
    with open(body, "rb") as f:
        data = f.read()
    return int(p.stdout.strip() or 0), content_range, data


class Slow:
    """A TCP relay to the server that passes the server's bytes at a
    steady rate, so that a client downloading through it can be stopped
    while it is in the middle of a subject, at a moment the proof sees
    rather than guesses. TLS passes through it untouched."""

    def __init__(self, port, rate):
        self.port, self.rate, self.passed = port, rate, 0
        self.stop = threading.Event()
        self.socks = []
        self.ls = socket.socket()
        self.ls.bind(("127.0.0.1", 0))
        self.ls.listen(8)
        self.addr = self.ls.getsockname()[1]
        threading.Thread(target=self.accept, daemon=True).start()

    def accept(self):
        while True:
            try:
                c, _ = self.ls.accept()
            except OSError:
                return
            u = socket.create_connection(("127.0.0.1", self.port))
            self.socks += [c, u]
            threading.Thread(target=self.pump, args=(c, u, False), daemon=True).start()
            threading.Thread(target=self.pump, args=(u, c, True), daemon=True).start()

    def pump(self, src, dst, slow):
        step = min(max(self.rate // 20, 1), 65536)
        try:
            while not self.stop.is_set():
                data = src.recv(step if slow else 65536)
                if not data:
                    break
                dst.sendall(data)
                if slow:
                    self.passed += len(data)
                    time.sleep(len(data) / self.rate)
        except OSError:
            pass
        finally:
            # One side gone ends the other, so a client is never left
            # waiting on a relay that no longer forwards.
            for x in (src, dst):
                try:
                    x.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass

    def close(self):
        self.stop.set()
        for s in [self.ls] + self.socks:
            try:
                s.close()
            except OSError:
                pass


def check_tree(state, tenant, to, subject):
    """What "nopacs download" left in to for a subject, against the storage's
    manifests: (files whole, files expected, .part files, problems), each
    file's SHA-256 by the system's sha256sum."""
    want = {}
    for path, (_, e) in rk.archived(state, tenant, "main").items():
        parts = e["key"].split("/")
        if parts[1] == subject:
            want[os.path.join(to, *parts[1:])] = e
    # A .part is a file still arriving; beside it, <name>.part.sha256 is
    # the checksum of the file it is the start of. Both are the command's
    # own, and neither may be left once the download is whole.
    found, parts = [], []
    for d, _, names in os.walk(to):
        for n in names:
            (parts if n.endswith((".part", ".part.sha256")) else found).append(os.path.join(d, n))
    sums = rk.sha256_tool(found)
    whole = [p for p in found if p in want and sums.get(p) == want[p]["sha256"]]
    problems = ["%s: not a file of the subject" % os.path.relpath(p, to) for p in found if p not in want]
    problems += ["%s: SHA-256 %s, the manifest has %s" % (os.path.relpath(p, to), sums.get(p), want[p]["sha256"])
                 for p in found if p in want and p not in whole]
    return len(whole), len(want), parts, problems


def download_resumed(work, state, t):
    """16.3: "nopacs download" of the subject that holds the largest file,
    through a relay that passes 1 MiB a second, killed with signal 9 once
    that file's .part holds some of its bytes, then run again."""
    name = t["name"]
    by_subject, largest = {}, None
    for _, (_, e) in rk.archived(state, name, "main").items():
        by_subject.setdefault(e["key"].split("/")[1], []).append(e)
        if largest is None or e["length"] > largest["length"]:
            largest = e
    subject = largest["key"].split("/")[1]
    total = sum(e["length"] for e in by_subject[subject])
    to = os.path.join(state.dir, "download-" + name)
    keyfile = os.path.join(state.dir, "keys", "reader")
    relay = Slow(state["port"], 1 << 20)

    def command(port):
        return [state["bin"], "--server", "https://127.0.0.1:%d" % port, "--key-file", keyfile, "--ca-file", state["ca"],
                "download", name, subject, "--to", to, "--quiet"]

    large_part = os.path.join(to, *largest["key"].split("/")[1:]) + ".part"

    def part_bytes():
        return os.path.getsize(large_part) if os.path.exists(large_part) else 0

    env = {k: v for k, v in os.environ.items() if k not in ("NOPACS_SERVER", "NOPACS_KEY", "NOPACS_DATA_DIR")}
    first = subprocess.Popen(command(relay.addr), stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)
    deadline = time.time() + 120
    while time.time() < deadline and first.poll() is None and not part_bytes():
        time.sleep(0.02)
    interrupted = first.poll() is None
    if interrupted:
        first.send_signal(signal.SIGKILL)
    first.wait()
    passed = relay.passed
    relay.close()
    before, n, parts, _ = check_tree(state, name, to, subject)
    held_part = part_bytes()
    held_whole = sum(os.path.getsize(os.path.join(d, f)) for d, _, fs in os.walk(to) for f in fs if f.endswith(".dcm"))
    # Run again through a relay without a limit, which counts what the
    # second run fetches: what was missing, not the whole subject again.
    relay = Slow(state["port"], 1 << 40)
    second = subprocess.run(command(relay.addr), capture_output=True, text=True, timeout=300, env=env)
    fetched = relay.passed
    relay.close()
    missing = total - held_whole - held_part
    after, n, left, problems = check_tree(state, name, to, subject)
    ok = (interrupted and 0 < held_part < largest["length"] and second.returncode == 0 and after == n and not left and not problems
          and fetched < missing + (64 << 10))
    work.result("16.3 nopacs download of %s, killed midway and run again, leaves every file whole" % name, ok,
                "a subject of %d files, %d bytes, the largest file %d bytes, through a relay passing 1 MiB/s; killed by signal 9 "
                "after %d bytes from the server: %d whole, %d .part and record files, the largest file's .part holding %d bytes; "
                "run again: exit %d, %d bytes from the server "
                "for the %d missing ('%s'), %d of %d whole by sha256sum against the manifests, %d .part or record files left" % (
                    n, total, largest["length"], passed, before, len(parts), held_part, second.returncode, fetched, missing,
                    rk.last_line(second.stdout), after, n, len(left)),
                problems + work.redact(second.stdout + second.stderr).splitlines()[-6:] +
                ([] if interrupted else ["the first run ended (exit %s) before it could be stopped" % first.returncode]))


def cmd_downloads(work, args):
    state = rk.State(work)
    hdr = headers_file(state)
    for t in state.tenants():
        name = t["name"]
        cfg = {"base": "https://127.0.0.1:%d" % state["port"], "tenant": name, "headers": hdr,
               "manifests": [p for p, _ in rk.manifests(state, name, "main")]}
        path = os.path.join(state.dir, "download-%s.json" % name)
        with open(path, "w") as f:
            json.dump(cfg, f)
        rc, out, full = rk.run_check(work, sys.executable, "download-curl.py", [path], env={"CURL_CA_BUNDLE": state["ca"]})
        work.result("16.1 every file of %s (%s) downloaded with curl has its manifest's SHA-256" % (name, t["db"]), rc == 0,
                    "over TLS with the reader's key: whole (Content-Length, ETag, SHA-256 by hashlib), two ranges joined, "
                    "curl -C - after a cut, past the end 416, If-Range with another tag, HEAD; download-curl.py: %s" % rk.last_line(out),
                    full.splitlines()[-20:])

        seed = int.from_bytes(os.urandom(4), "big")
        rnd = random.Random(seed)
        problems, n = [], 0
        for _, (fpath, e) in sorted(rk.archived(state, name, "main").items()):
            with open(fpath, "rb") as f:
                data = f.read()
            url = "https://127.0.0.1:%d%s" % (state["port"], rk.instance_path(name, e["key"]))
            specs = []
            for _ in range(3):
                a = rnd.randrange(len(data))
                b = rnd.randrange(a, len(data))
                specs.append(("%d-%d" % (a, b), a, b))
            k = rnd.randrange(1, len(data))
            specs.append(("-%d" % k, len(data) - k, len(data) - 1))
            for spec, a, b in specs:
                n += 1
                status, cr, body = curl_range(state, hdr, url, spec, state.dir)
                if status != 206 or cr != "bytes %d-%d/%d" % (a, b, len(data)) or body != data[a:b + 1]:
                    problems.append("%s --range %s: %d %r, %d bytes, the file's are %s" % (
                        e["key"].split("/")[3], spec, status, cr, len(body), "equal" if body == data[a:b + 1] else "different"))
        work.result("16.2 random ranges of every file of %s with curl --range are the file's bytes" % name, not problems,
                    "%d ranges (three random and one suffix range a file, seed %d): each 206, its Content-Range, its bytes "
                    "equal to the archived file's" % (n, seed), problems)
        download_resumed(work, state, t)
    return 0


def cmd_client(work, args):
    state = rk.State(work)
    sys.path.insert(0, os.path.join(ROOT, "clients", "python", "src"))
    import nopacs  # the client under test, from the checkout
    from nopacs.v1 import catalog_pb2, data_pb2, metadata_pb2

    with open(state["ca"], "rb") as f:
        roots = f.read()
    with nopacs.Client("127.0.0.1:%d" % state["port"], state.key("reader"), root_certificates=roots) as client:
        for t in state.tenants():
            name = t["name"]
            # Everything the tenant holds: what storescu sent, and the study
            # section 17 stored by STOW-RS.
            held = dict(rk.archived(state, name, "main"), **rk.archived(state, name, "web"))
            files = {uid: pydicom.dcmread(p, stop_before_pixels=True) for uid, (p, _) in held.items()}

            # A search: the sessions of one modality, and the scans of each.
            want = {}
            for ds in files.values():
                if str(ds.get("Modality", "")) == "CT":
                    want.setdefault(str(ds.StudyInstanceUID), set()).add(str(ds.SeriesInstanceUID))
            got, problems = {}, []
            req = catalog_pb2.ListSessionsRequest(tenant=name, modality="CT", page_size=1)
            for se in client.pages(client.catalog.ListSessions, req, "sessions"):
                scans = client.pages(client.catalog.ListScans, catalog_pb2.ListScansRequest(
                    tenant=name, subject_label=se.subject_label, session_label=se.label, page_size=1), "scans")
                got[se.study_instance_uid] = {s.series_instance_uid for s in scans}
            work.result("18.1 the Python client searches %s: the CT sessions and their scans are pydicom's" % name,
                        got == want and bool(want), "ListSessions modality CT and ListScans of each, a page of one at a time: "
                        "%d studies, %d series; pydicom reads %d and %d" % (
                            len(got), sum(map(len, got.values())), len(want), sum(map(len, want.values()))), [repr(got), repr(want)])

            # A metadata read: every scan's headers, checked as section 15.
            entries = []
            for sc in client.pages(client.catalog.ListScans, catalog_pb2.ListScansRequest(tenant=name), "scans"):
                req = metadata_pb2.GetScanMetadataRequest(tenant=name, subject_label=sc.subject_label, session_label=sc.session_label,
                                                          scan_label=sc.label)
                for i in client.pages(client.metadata.GetScanMetadata, req, "instances"):
                    if i.sop_instance_uid in held:
                        entries.append({"file": held[i.sop_instance_uid][0], "id": i.sop_instance_uid, "json": i.dicom_json,
                                        "omitted": i.bulk_data_omitted})
            path = os.path.join(state.dir, "client-metadata-%s.json" % name)
            with open(path, "w") as f:
                json.dump(entries, f)
            rc, out, full = rk.run_check(work, sys.executable, "metadata-pydicom.py", [path])
            work.result("18.2 the Python client reads %s's metadata, equal to pydicom's" % name, rc == 0 and len(entries) == len(held),
                        "GetScanMetadata of every scan: %d of %d instances; metadata-pydicom.py: %s" % (len(entries), len(held), rk.last_line(out)),
                        full.splitlines()[-20:])

            # A download: every subject's files through DataService.ReadFiles.
            problems, n, nbytes = [], 0, 0
            for su in client.pages(client.catalog.ListSubjects, catalog_pb2.ListSubjectsRequest(tenant=name), "subjects"):
                current, buf = None, bytearray()

                def finish():
                    if current is None:
                        return
                    path, entry = held.get(current.sop_instance_uid, (None, None))
                    with open(path, "rb") as f:
                        disk = f.read()
                    digest = hashlib.sha256(bytes(buf)).hexdigest()
                    if not (digest == current.sha256 == entry["sha256"] and bytes(buf) == disk):
                        problems.append("%s: %d bytes, SHA-256 %s; announced %s, manifest %s" % (
                            current.sop_instance_uid, len(buf), digest, current.sha256, entry["sha256"]))

                for msg in client.data.ReadFiles(data_pb2.ReadFilesRequest(tenant=name, subject_label=su.label)):
                    part = msg.WhichOneof("part")
                    if part == "file":
                        finish()
                        current, buf = msg.file, bytearray()
                        n += 1
                    elif part == "chunk":
                        if msg.chunk.offset != len(buf):
                            problems.append("a chunk at %d after %d bytes" % (msg.chunk.offset, len(buf)))
                        buf += msg.chunk.data
                        nbytes += len(msg.chunk.data)
                finish()
            work.result("18.3 the Python client downloads every file of %s whole" % name, not problems and n == len(held),
                        "ReadFiles of each subject: %d of %d files, %d bytes, each equal to the archived file and to the SHA-256 "
                        "announced and in the manifest (hashlib)" % (n, len(held), nbytes), problems)
    return 0
