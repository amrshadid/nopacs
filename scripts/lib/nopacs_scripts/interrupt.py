"""What the interruption harness (scripts/prove/interrupt.py) uses that is not
a check: the server built with the build tag that kills it at a named durable
step, started with that step named and waited for until it dies there, and
the readers that run across each kill (curl whole downloads and If-Range
resumes, a DICOMweb retrieve and the ReadFiles stream read with Python's
standard library, the catalog read by SQLite itself). Nothing here imports
NoPACs' code.
"""

import base64
import hashlib
import http.client
import io
import json
import os
import shutil
import subprocess
import tempfile
import threading
import time

from pydicom import dcmread

from nopacs_scripts.db import query
from nopacs_scripts.dicom import sop_uids
from nopacs_scripts.paths import ROOT
from nopacs_scripts.read import frames, stream_frame
from nopacs_scripts.report import Failure
from nopacs_scripts.send import Peer, split_part10
from nopacs_scripts.whole import Proof, make_study

ENV = "NOPACS_INTERRUPT_AT"  # the variable the tagged server reads at its start
STATUS = 86  # the exit status of a server stopped at its step
PURGE_STEPS = ["purge-started", "purge-object", "purge-objects", "purged"]  # the steps of every kind's purge
LOST_PUT ="temporary file was removed before it was put in place"  # what the directory storage says of a put whose file someone removed


class Harness(Proof):
    """An installation whose server is the tagged build, started with a step
    named, killed there, and started again without one."""

    def __init__(self, args):
        super().__init__(args, prefix="nopacs-int.")
        self.armed = ""
        self.logs = 0
        # When the last armed server was seen dead, and when the server
        # last answered after a start: a reader's answer before the first
        # was read before the kill, one after the second after the restart.
        self.died_at = self.up_at = 0.0

    def build(self):
        """The command line as it is, the server with the build tag; both
        from the repository's root, whatever the caller's directory."""
        if self.args.binary:
            shutil.copy(self.args.binary, self.bin)
            shutil.copy(self.args.server_binary, self.sbin)
            return
        subprocess.run([self.args.go, "build", "-o", self.bin, "./cmd/nopacs"], check=True, cwd=ROOT)
        subprocess.run([self.args.go, "build", "-tags", "interrupt", "-o", self.sbin, "./cmd/nopacs-server"], check=True, cwd=ROOT)

    def log_path(self):
        return os.path.join(self.work, "serve-%02d.log" % self.logs)

    def serve(self, data=None, at=""):
        """Starts the server, with the step at named (none when empty), and
        waits until it answers; each start writes a log of its own."""
        data = data or self.data
        self.logs += 1
        self.armed = at
        env = {k: v for k, v in os.environ.items() if k not in ("NOPACS_SERVER", "NOPACS_KEY", ENV)}
        if at:
            env[ENV] = at
        log = open(self.log_path(), "w")
        self.server = subprocess.Popen([self.sbin, "--data-dir", data, "serve", "--listen", "127.0.0.1:%d" % self.api_port],
                                       stdout=log, stderr=log, env=env)
        for _ in range(300):
            if self.server.poll() is not None:
                if at and self.server.returncode == STATUS:
                    return  # work resumed at the start reached the step before the first answer: died() judges it
                raise Failure("the server exited at its start (status %d); see %s" % (self.server.returncode, self.log_path()))
            if self.run("whoami", data=data).returncode == 0:
                self.up_at = time.time()
                return
            time.sleep(0.1)
        raise Failure("the server did not answer")

    def arm(self, step):
        """Restarts the server as a person does, now told to die at step."""
        self.stop()
        self.serve(at=step)

    def died(self, timeout=90):
        """Waits for the armed server to end; returns (whether it ended at
        its step with STATUS and said so in its log, what was seen). A server
        still running at the timeout is killed and counts as not reached."""
        try:
            self.server.wait(timeout)
        except subprocess.TimeoutExpired:
            self.kill9()
            self.died_at = time.time()
            return False, "the step %s was not reached in %d s; killed" % (self.armed, timeout)
        self.died_at = time.time()
        with open(self.log_path(), errors="replace") as f:
            said = ("interrupt: stopped at %s" % self.armed) in f.read()
        code = self.server.returncode
        return code == STATUS and said, "exit %d, its log %s the step" % (code, "names" if said else "does not name")

    def restart(self):
        self.serve()

    def logged(self):
        """Every warning and error of every server log of this run, as
        (log name, message, reason) triples."""
        out = []
        for n in range(1, self.logs + 1):
            with open(os.path.join(self.work, "serve-%02d.log" % n), errors="replace") as f:
                for line in f:
                    try:
                        e = json.loads(line)
                    except ValueError:
                        continue
                    if e.get("level") in ("WARN", "ERROR"):
                        out.append(("serve-%02d.log" % n, e.get("msg", ""), e.get("error", e.get("reason", ""))))
        return out

    def lost_puts(self):
        """The warnings that say a put to a directory storage lost its
        temporary file before it was renamed into place: someone cleaned the
        storage's temporary directory meanwhile. Lines "log msg: reason"."""
        return ["%s %s: %s" % x for x in self.logged() if LOST_PUT in x[2]]

    def storage_failures(self):
        """Every other warning that names a storage failing to do what it
        was asked (a full disk, a read-only or failing device, a probe file
        gone): reported apart, attributed to nothing."""
        return ["%s %s: %s" % x for x in self.logged() if LOST_PUT not in x[2] and "storage" in x[2]]

    def warnings(self):
        """The warnings and errors the running server's log holds, as "level msg: error" lines."""
        out = []
        with open(self.log_path(), errors="replace") as f:
            for line in f:
                try:
                    e = json.loads(line)
                except ValueError:
                    continue
                if e.get("level") in ("WARN", "ERROR"):
                    out.append("server log %s %s: %s" % (e["level"], e.get("msg", ""), e.get("error", e.get("reason", ""))))
        return out

    # ---- what the storage and the catalog hold, read without NoPACs ----

    def records(self, cid, tenant="lab", storage="main"):
        """The state each correction record of cid on the storage says."""
        path = os.path.join(self.storage_root(tenant, storage), "meta", ".corrections", cid + ".json")
        if not os.path.exists(path):
            return None
        with open(path) as f:
            return json.load(f).get("state")

    def manifest_entry(self, row, uid, tenant="lab"):
        """The entry of uid in its scan's manifest on the storage, as JSON."""
        path = os.path.join(self.storage_root(tenant), "meta", row["subject"], row["session"], row["scan"], "manifest.json")
        with open(path) as f:
            for e in json.load(f)["files"]:
                if e["sop_instance_uid"] == uid:
                    return e
        return None

    def correction(self, cid):
        st, a = self.call("nopacs.v1.CorrectionService/GetCorrection", {"tenant": "lab", "id": cid})
        return a.get("correction", {}) if st == 200 else {}

    def corrections(self, kind=None):
        st, a = self.call("nopacs.v1.CorrectionService/ListCorrections", {"tenant": "lab", "pageSize": 500})
        out = a.get("corrections", []) if st == 200 else []
        return [c for c in out if not kind or c.get("kind") == "CORRECTION_KIND_" + kind.upper()]

    def wait_state(self, cid, states, timeout=150):
        """Waits until the correction is in one of states and its records say
        so; returns the correction as the API last answered it."""
        t0 = time.time()
        c = {}
        while time.time() < t0 + timeout:
            c = self.correction(cid)
            s = c.get("state", "").replace("CORRECTION_STATE_", "").lower()
            if s in states and self.records(cid) == s:
                break
            time.sleep(0.3)
        self.slow(cid, time.time() - t0)
        return c

    def slow(self, what, seconds):
        """Notes a recovery that took more than five seconds after the
        restart, with the warnings the server logged meanwhile: not a
        failed check, but what made it wait is worth reading."""
        if seconds > 5:
            self.note("(for the cause) %s reached its end state %.0f s after the restart; the server's log: %s"
                      % (what[-8:], seconds, " | ".join(self.warnings())[:600] or "no warning"))


# ---- reads, each answering what it got or None when the server was away ----

def sha(b):
    return hashlib.sha256(b).hexdigest()


CUT = "cut"  # an answer the server began, or a connection it took, then dropped without a whole answer


def away_or_cut(e):
    """None when the server was not there to take the connection; CUT when
    it took it and dropped it: by the kill, once per kill, or by a fault of
    its own, which the check counts against the number of kills."""
    return None if isinstance(e, ConnectionRefusedError) else CUT


def get(port, key, path, headers=None, timeout=30):
    """One GET with http.client; (status, headers by lower name, body),
    None when the server does not take the connection, CUT when it drops
    it."""
    c = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    try:
        c.request("GET", path, headers=dict(headers or {}, Authorization="Bearer " + key))
        r = c.getresponse()
        return r.status, {k.lower(): v for k, v in r.getheaders()}, r.read()
    except (OSError, http.client.HTTPException) as e:
        return away_or_cut(e)
    finally:
        c.close()


def curl(p, path, *extra):
    """curl against the server, into files of its own so readers in threads
    do not share them; (HTTP status, headers by lower name, body, curl's
    exit), the status 0 when nothing was answered."""
    fd, hdr = tempfile.mkstemp(dir=p.work, prefix="h.")
    os.close(fd)
    fd, out = tempfile.mkstemp(dir=p.work, prefix="b.")
    os.close(fd)
    try:
        r = subprocess.run(["curl", "-s", "-D", hdr, "-o", out, "-w", "%{http_code}", "-H", "Authorization: Bearer " + p.key, *extra,
                            "http://127.0.0.1:%d%s" % (p.api_port, path)], capture_output=True, text=True, timeout=120)
        headers = {}
        with open(hdr, "rb") as f:
            for line in f.read().decode("latin-1").splitlines()[1:]:
                if ":" in line:
                    k, v = line.split(":", 1)
                    headers[k.strip().lower()] = v.strip()
        with open(out, "rb") as f:
            body = f.read()
        return int(r.stdout.strip() or 0), headers, body, r.returncode
    finally:
        os.remove(hdr)
        os.remove(out)


def curl_range(p, path, first, last=None, if_range=None):
    """A Range request by curl, with If-Range when given (RFC 9110 13.1.5);
    (status, ETag, body, curl's exit) or None when the server was away."""
    rng = "%d-%s" % (first, "" if last is None else last)
    extra = ["-r", rng] + (["-H", "If-Range: " + if_range] if if_range else [])
    code, hdr, body, rc = curl(p, path, *extra)
    if code == 0:
        return None if rc == 7 else CUT
    return code, hdr.get("etag", ""), body, rc


def whole(p, path):
    """A whole download by curl: (status, ETag, body, curl's exit), None
    when the server was away (curl's exit 7), CUT when it took the
    connection and answered nothing; the exit tells a transfer cut short
    (18, 52, 56), which the client knows, from a wrong one."""
    code, hdr, body, rc = curl(p, path)
    if code == 0:
        return None if rc == 7 else CUT
    return code, hdr.get("etag", ""), body, rc


def parts(content_type, body):
    """The parts of a multipart/related body (RFC 2046 5.1), by its boundary."""
    boundary = None
    for x in content_type.split(";"):
        x = x.strip()
        if x.startswith("boundary="):
            boundary = x[len("boundary="):].strip('"')
    if not boundary:
        return []
    out = []
    for chunk in body.split(b"--" + boundary.encode())[1:]:
        if chunk.startswith(b"--"):
            break
        head, _, data = chunk.partition(b"\r\n\r\n")
        out.append(data[:-2] if data.endswith(b"\r\n") else data)
    return out


def wado(p, study, series, uid):
    """A DICOMweb WADO-RS retrieve of one instance (PS3.18 10.4): (status,
    [part bytes]) or None when the server was away."""
    got = get(p.api_port, p.key, "/tenants/lab/dicom-web/studies/%s/series/%s/instances/%s" % (study, series, uid),
              {"Accept": 'multipart/related; type="application/dicom"'})
    if got is None or got == CUT:
        return got
    st, hdr, body = got
    return st, parts(hdr.get("content-type", ""), body) if st == 200 else []


def qido_studies(p):
    """QIDO-RS of every study (PS3.18 10.6): the number answered, or None."""
    got = get(p.api_port, p.key, "/tenants/lab/dicom-web/studies?limit=1000", {"Accept": "application/dicom+json"})
    if got is None or got == CUT:
        return got
    if got[0] not in (200, 204):
        return "HTTP %d" % got[0]
    return len(json.loads(got[2] or b"[]"))


def read_files(p, subject):
    """DataService.ReadFiles of a subject over the Connect protocol, JSON
    framed: {SOP Instance UID: (announced SHA-256, bytes)}, or None when the
    server was away, or a string when the stream ended in an error."""
    c = http.client.HTTPConnection("127.0.0.1", p.api_port, timeout=60)
    try:
        c.request("POST", "/nopacs.v1.DataService/ReadFiles", stream_frame({"tenant": "lab", "subjectLabel": subject}),
                  {"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1", "Authorization": "Bearer " + p.key})
        r = c.getresponse()
        data = r.read()
        if r.status != 200:
            return "HTTP %d" % r.status
    except (OSError, http.client.HTTPException) as e:
        return away_or_cut(e)
    finally:
        c.close()
    out, current = {}, None
    for flags, msg in frames(data):
        if flags & 0x02:
            if msg.get("error"):
                return "stream error %s" % msg["error"].get("code")
            continue
        if "file" in msg:
            current = msg["file"]["sopInstanceUid"]
            out[current] = (msg["file"].get("sha256", ""), bytearray())
        elif "chunk" in msg and current:
            out[current][1].extend(base64.b64decode(msg["chunk"].get("data", "")))
    return {k: (s, bytes(b)) for k, (s, b) in out.items()}


def list_subjects(p):
    """CatalogService.ListSubjects in one page: (labels, total_count) or None."""
    try:
        st, a = p.call("nopacs.v1.CatalogService/ListSubjects", {"tenant": "lab", "pageSize": 500})
    except (OSError, http.client.HTTPException, ValueError) as e:
        return away_or_cut(e)
    if st != 200:
        return "HTTP %d %s" % (st, a.get("code", ""))
    return [s["label"] for s in a.get("subjects", [])], int(a.get("totalCount", -1))


def dicom_uid(b):
    """The SOP Instance UID pydicom reads in a Part 10 file's bytes, or an error's name."""
    try:
        return str(dcmread(io.BytesIO(b), stop_before_pixels=True).SOPInstanceUID)
    except Exception as e:  # noqa: BLE001 - any failure to read is what is reported
        return "unreadable: %s" % type(e).__name__


def labels_of(db, uids):
    """{subject label: number of the instances of uids under it}, read by SQLite from the catalog."""
    marks = ",".join("?" * len(uids))
    rows = query(db, "SELECT s.subject_label AS label, count(*) AS n FROM instances AS i JOIN scans AS s ON s.id = i.scan_id "
                     "WHERE i.sop_instance_uid IN (%s) GROUP BY 1" % marks, *uids)
    return {r["label"]: r["n"] for r in rows}


CURL_CUT = (18, 52, 56)  # curl's exits for a transfer the server ended early: the client knows it is short


def cut_out(answers):
    """The answers of a loop but those the server dropped, and how many it
    dropped. A kill drops at most the one request in flight of each reader;
    more than one per kill is a fault of the server's own."""
    kept = [(t, a) for t, a in answers if a != CUT and not (isinstance(a, tuple) and len(a) == 4 and a[3] in CURL_CUT)]
    return kept, len(answers) - len(kept)


def flips(seen):
    """How often the revision served changed between two reads in a row."""
    return sum(1 for a, b in zip(seen, seen[1:]) if a != b)


def order_problems(seen, old, new, published):
    """What is wrong with the revisions reads served, in the order they were
    read: before a publish only the old may be served; across one, the old
    until the first new and never the old again (a reader must never go
    back a revision)."""
    if not published:
        return ["a download served a revision never published"] if any(x != old for x in seen) else []
    first = seen.index(new) if new in seen else len(seen)
    back = sum(1 for x in seen[first:] if x == old)
    return ["the old revision served %d times after the new" % back] if back else []


def sides(answers, died_at, up_at):
    """How many answers were read before the kill, and how many after the restart."""
    return sum(1 for t, _ in answers if t < died_at), sum(1 for t, _ in answers if t > up_at)


def setup(p):
    """The installation of whole.py, without the scrub the server runs by
    itself: a scrub job running names every scan, and a rebuild replace is
    refused while a job not ended names a scan it changes."""
    p.setup()
    p.m("setting", "set", "scrub.interval", "0", "--tenant", "lab")
    p.held_before = 0


def studies(p, name, n, sources):
    """n studies, one subject each, sent as received by storescu; returns
    (files, SOP Instance UIDs) of each, in order."""
    out, paths = [], []
    for j in range(n):
        f = make_study(p.args.pydicom_corpus, os.path.join(p.work, "studies"), "%s^%d" % (name, j), "%s-%d" % (name[:3].upper(), j), sources)
        paths += f
        out.append((f, sop_uids(f)))
    got, said = p.storescu(p.raw_port, paths)
    held = p.wait_held(len(paths) + p.held_before)
    if got != len(paths) or held < len(paths) + p.held_before:
        raise Failure("the scene: %d of %d answered success, %d held" % (got, len(paths), held))
    p.held_before += len(paths)
    return out


def key_of(r, uid=None, rev=1):
    """The storage key of an instance's revision under the scan's labels in r."""
    base = "data/%s/%s/%s/%s" % (r["subject"], r["session"], r["scan"], uid or r["uid"])
    return base + (".dcm" if rev == 1 else ".r%d.dcm" % rev)


class KeepingPeer(Peer):
    """The sending proof's pynetdicom peer, keeping the data set bytes of
    every C-STORE it is sent, as encoded on the wire: {SOP Instance UID:
    [bytes, one per time it came]}."""

    def __init__(self, port):
        self.bytes = {}
        super().__init__(port)

    def store(self, event):
        with self.lock:
            self.bytes.setdefault(str(event.request.AffectedSOPInstanceUID), []).append(event.request.DataSet.getvalue())
        return super().store(event)


def data_set(path):
    """The data set bytes of a stored Part 10 file, after its meta group (PS3.10 7.1)."""
    with open(path, "rb") as f:
        return split_part10(f.read())[1]


class Loop:
    """A probe run again and again in a thread until stopped; each answer it
    returns is kept with its time, None (the server away) counted apart."""

    def __init__(self, probe, pause=0.02):
        self.probe, self.pause = probe, pause
        self.answers, self.away = [], 0
        self.halt = threading.Event()
        self.thread = threading.Thread(target=self.run, daemon=True)

    def run(self):
        while not self.halt.is_set():
            try:
                a = self.probe()
            except Exception as e:  # noqa: BLE001 - a probe's own failure is an answer to report
                a = ("probe failed", repr(e))
            if a is None:
                self.away += 1
            else:
                self.answers.append((time.time(), a))
            time.sleep(self.pause)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *exc):
        self.halt.set()
        self.thread.join(120)
