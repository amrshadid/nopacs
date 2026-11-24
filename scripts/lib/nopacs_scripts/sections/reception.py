"""The proof of reception, from outside the binary.

It builds and runs the real program, sets it up with its own command line,
and then checks what reception did only with tools that are not NoPACs:
dcmtk (storescu, echoscu, dcmdump, dcmconv) and pynetdicom send; pydicom
and dcmdump read what was archived; hashlib checks every manifest entry;
Python's sqlite3 reads the tenant's catalog; the status and the event
stream are read over HTTPS in the Connect protocol with Python's standard
library alone. With --s3, a tenant archives to a bucket, and each object is
read back with an S3 GET signed (Signature Version 4) by the standard
library, not by NoPACs' client. With --postgres and --psql, a tenant's
catalog is a Postgres database, read by psql. Each check prints one line, "ok",
"FAIL" or "skip", with what it saw, and the run ends with the counts; it
exits 1 when one failed.

  reception.py --go GO --dcmtk DIR [--crash-rounds N] [--s3 URL]     (make prove-reception)

URL is http://KEY:SECRET@HOST:PORT/BUCKET, as make s3-up prints it; the
bucket is made if it is not there. Without --s3 the S3 checks are skipped.
--postgres is the address of a Postgres server whose login may make
databases (postgres://USER:PASSWORD@HOST:PORT/DB), and --psql the command
that runs psql against it, such as "docker exec -i CONTAINER psql -U
postgres"; the proof makes a database of its own and drops it at the end.
Without them the Postgres checks are skipped.

Needs: Go, dcmtk, openssl, and a Python with pydicom and pynetdicom
(scripts/requirements/prove-reception.txt).
"""

import argparse
import datetime
import hashlib
import hmac
import http.client
import io
import json
import os
import shlex
import socket
import sqlite3
import ssl
import struct
import subprocess
import tempfile
import time
import urllib.parse

from pydicom import dcmread
from pydicom.uid import generate_uid
from pynetdicom import AE

from nopacs_scripts.net import free_ports
from nopacs_scripts.paths import ROOT
from nopacs_scripts.proc import build_binaries
from nopacs_scripts.proof import Programs, Reporting
from nopacs_scripts.report import Failure

CORPUS = os.path.join(ROOT, "internal", "receive", "testdata")
CT = ["ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm", "ct5n-3023.dcm", "ct5n-3353.dcm"]
QUIET = 5  # seconds, the least a receiver allows; every receiver of the proof is quiet after it


class Proof(Reporting, Programs):
    """The run: its work directory, the server it runs, and its results."""

    def __init__(self, args):
        self.args = args
        self.work = tempfile.mkdtemp(prefix="nopacs-prove-reception.")
        os.chmod(self.work, 0o700)
        self.data = os.path.join(self.work, "data")
        self.bin = os.path.join(self.work, "nopacs")
        self.sbin = os.path.join(self.work, "nopacs-server")
        self.server = None
        self.counts = {"ok": 0, "FAIL": 0, "skip": 0}
        self.s3 = None
        self.s3_tenants = {}  # tenant -> the key prefix of its bucket storage
        self.pg_tenants = {}  # tenant -> the name of its Postgres database
        self.pg_made = ""  # the database the proof made, dropped at the end

    # ---- the program ----

    def m(self, *args, check=True):
        """Runs the command line on the local socket and returns its output."""
        p = subprocess.run([self.exe(*args), "--data-dir", self.data, *args], capture_output=True, text=True, timeout=120)
        if check and p.returncode != 0:
            raise Failure("nopacs %s: exit %d: %s" % (" ".join(args), p.returncode, p.stderr.strip()))
        return p.stdout

    def serve(self):
        log = open(os.path.join(self.work, "serve.log"), "a")
        self.server = subprocess.Popen([self.sbin, "--data-dir", self.data, "serve", "--listen", "127.0.0.1:%d" % self.api_port,
                                        "--tls-cert", self.cert, "--tls-key", self.key_file], stdout=log, stderr=log)
        for _ in range(300):
            if self.server.poll() is not None:
                raise Failure("the server exited at its start; see serve.log")
            if self.m("whoami", check=False):
                return
            time.sleep(0.1)
        raise Failure("the server did not answer")

    def kill(self):
        """Ends the server with signal 9, as a crash does."""
        self.server.kill()
        self.server.wait()

    # ---- outside tools ----

    def dcmtk(self, name):
        return os.path.join(self.args.dcmtk, name) if self.args.dcmtk else name

    def storescu(self, port, files, aet="STORESCU"):
        """Sends files with dcmtk's storescu; returns how many were answered success."""
        p = subprocess.run([self.dcmtk("storescu"), "-v", "-aet", aet, "-aec", "ARCHIVE", "127.0.0.1", str(port), *files],
                           capture_output=True, text=True, timeout=300)
        out = p.stdout + p.stderr
        return out.count("Received Store Response (Success)"), out

    def echoscu(self, port, aet):
        p = subprocess.run([self.dcmtk("echoscu"), "-v", "-aet", aet, "-aec", "ARCHIVE", "127.0.0.1", str(port)],
                           capture_output=True, text=True, timeout=60)
        return p.returncode, p.stdout + p.stderr

    def pynetdicom_store(self, port, files, aet="PYNETDICOM", on_each=None):
        """Sends files with pynetdicom, each from its path as the file holds
        it; returns each status (None for one not answered)."""
        ae = AE(ae_title=aet)
        ae.acse_timeout = ae.dimse_timeout = ae.network_timeout = 30
        contexts = []
        for f in files:
            meta = dcmread(f, stop_before_pixels=True)
            pair = (str(meta.SOPClassUID), str(meta.file_meta.TransferSyntaxUID))
            if pair not in contexts:
                contexts.append(pair)
                ae.add_requested_context(*pair)
        assoc = ae.associate("127.0.0.1", port, ae_title="ARCHIVE")
        statuses = []
        if not assoc.is_established:
            return statuses
        try:
            for i, f in enumerate(files):
                st = assoc.send_c_store(f)
                statuses.append(st.Status if "Status" in st else None)
                if on_each:
                    on_each(i)
            assoc.release()
        except Exception:  # the server went away mid association: a crash
            statuses.append(None)
        return statuses

    def catalog(self, tenant):
        """The tenant's database, read only, by SQLite itself."""
        db = sqlite3.connect("file:%s?mode=ro" % os.path.join(self.data, "tenants", tenant + ".db"), uri=True, timeout=30)
        db.row_factory = sqlite3.Row
        return db

    def psql(self, db, sql):
        """Runs one statement with psql in the database db; returns what it
        printed, unaligned and without headers."""
        p = subprocess.run(shlex.split(self.args.psql) + ["-d", db, "-v", "ON_ERROR_STOP=1", "-tAq", "-c", sql],
                           capture_output=True, text=True, timeout=120)
        if p.returncode != 0:
            raise Failure("psql: %s" % p.stderr.strip())
        return p.stdout.strip()

    def query(self, tenant, sql, *params):
        """Rows of the tenant's catalog, read by SQLite itself, or by psql
        for a tenant on Postgres (each row a JSON object, so both answer
        rows by column name)."""
        if tenant in self.pg_tenants:
            parts = sql.split("?")
            assert len(parts) == len(params) + 1, sql
            text = parts[0]
            for value, rest in zip(params, parts[1:]):
                text += "'" + str(value).replace("'", "''") + "'" + rest
            return json.loads(self.psql(self.pg_tenants[tenant], "SELECT coalesce(json_agg(t), '[]') FROM (%s) t" % text))
        db = self.catalog(tenant)
        try:
            return db.execute(sql, params).fetchall()
        finally:
            db.close()

    # ---- the API over HTTPS, by Python's standard library ----

    def connect(self):
        ctx = ssl.create_default_context(cafile=self.cert)
        return http.client.HTTPSConnection("127.0.0.1", self.api_port, context=ctx, timeout=60)

    def call(self, procedure, body):
        """One unary call in the Connect protocol, JSON both ways."""
        c = self.connect()
        c.request("POST", "/" + procedure, json.dumps(body), {"Content-Type": "application/json", "Connect-Protocol-Version": "1",
                                                              "Authorization": "Bearer " + self.api_key})
        r = c.getresponse()
        data = json.loads(r.read() or b"{}")
        c.close()
        if r.status != 200:
            raise Failure("%s: HTTP %d %s" % (procedure, r.status, data))
        return data

    def watch(self, body):
        """Opens a stream of events in the Connect streaming protocol and
        returns the connection and its response, unread."""
        c = self.connect()
        payload = json.dumps(body).encode()
        c.request("POST", "/nopacs.v1.MonitorService/Watch", b"\0" + struct.pack(">I", len(payload)) + payload,
                  {"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1", "Authorization": "Bearer " + self.api_key})
        return c, c.getresponse()


class S3:
    """The few S3 calls the proof needs, signed with Signature Version 4
    (AWS's published algorithm) by hand, so what is read back does not pass
    through the client that wrote it."""

    def __init__(self, url, region="us-east-1"):
        u = urllib.parse.urlsplit(url)
        self.endpoint = "%s://%s:%d" % (u.scheme, u.hostname, u.port)
        self.host, self.port = u.hostname, u.port
        self.key, self.secret = urllib.parse.unquote(u.username), urllib.parse.unquote(u.password)
        self.bucket = u.path.strip("/")
        self.region = region

    def request(self, method, key=""):
        path = "/" + self.bucket + ("/" + urllib.parse.quote(key, safe="/~") if key else "")
        now = datetime.datetime.now(datetime.timezone.utc)
        amz, day = now.strftime("%Y%m%dT%H%M%SZ"), now.strftime("%Y%m%d")
        empty = hashlib.sha256(b"").hexdigest()
        host = "%s:%d" % (self.host, self.port)
        headers = {"host": host, "x-amz-content-sha256": empty, "x-amz-date": amz}
        signed = ";".join(sorted(headers))
        canonical = "\n".join([method, path, "", "".join("%s:%s\n" % (k, headers[k]) for k in sorted(headers)), signed, empty])
        scope = "%s/%s/s3/aws4_request" % (day, self.region)
        to_sign = "\n".join(["AWS4-HMAC-SHA256", amz, scope, hashlib.sha256(canonical.encode()).hexdigest()])
        k = ("AWS4" + self.secret).encode()
        for part in (day, self.region, "s3", "aws4_request"):
            k = hmac.new(k, part.encode(), hashlib.sha256).digest()
        sig = hmac.new(k, to_sign.encode(), hashlib.sha256).hexdigest()
        headers["Authorization"] = "AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s" % (self.key, scope, signed, sig)
        c = http.client.HTTPConnection(self.host, self.port, timeout=60)
        c.request(method, path, headers=headers)
        resp = c.getresponse()
        body = resp.read()
        c.close()
        return resp.status, body

    def make_bucket(self):
        status, body = self.request("PUT")
        if status not in (200, 409):
            raise Failure("S3: making bucket %s: HTTP %d %s" % (self.bucket, status, body[:200]))

    def get(self, key):
        status, body = self.request("GET", key)
        if status != 200:
            raise RuntimeError("S3 GET %s: HTTP %d" % (key, status))
        return body



# The catalog's held instances as the proof reads them: each with its key
# on its storage, which a row keeps only when it is not the layout's key of
# its scan and UID; its archived checksum, which a row keeps only when it
# is not the received one; and its scan's study.
HELD = """(SELECT i.sop_instance_uid, s.study_uid,
    CASE WHEN i.storage_key = '' THEN 'data/' || s.subject_label || '/' || s.session_label || '/' || s.label || '/'
        || i.sop_instance_uid || '.dcm' ELSE i.storage_key END AS storage_key,
    coalesce(i.archived_sha256, i.sha256) AS archived_sha256
    FROM instances AS i JOIN scans AS s ON s.id = i.scan_id) AS held"""

def frames(resp):
    """The messages of a Connect stream, until its end."""
    while True:
        head = resp.read(5)
        if len(head) < 5:
            return
        flags, n = head[0], struct.unpack(">I", head[1:])[0]
        msg = json.loads(resp.read(n) or b"{}")
        if flags & 2:
            if msg.get("error"):
                raise RuntimeError(msg["error"])
            return
        yield msg


def data_set(path):
    """The bytes of a Part 10 file after its meta group: the data set as it
    was sent (PS3.10 7.1; the meta group's length is its first element)."""
    with open(path, "rb") as f:
        b = f.read()
    group = struct.unpack("<I", b[140:144])[0]
    return b[144 + group:]


def study_copy(p, sources, n, truncate=None):
    """Writes n files made from sources (cycling) as one new study with one
    new series, each with a new SOP Instance UID, and returns their paths;
    the file at index truncate is cut inside its pixel data, as a sender
    that died mid file would leave it."""
    os.makedirs(os.path.join(p.work, "studies"), exist_ok=True)
    out = tempfile.mkdtemp(dir=os.path.join(p.work, "studies"))
    study, series = generate_uid(), generate_uid()
    paths = []
    for i in range(n):
        ds = dcmread(os.path.join(CORPUS, sources[i % len(sources)]))
        ds.StudyInstanceUID, ds.SeriesInstanceUID = study, series
        ds.SOPInstanceUID = ds.file_meta.MediaStorageSOPInstanceUID = generate_uid()
        ds.InstanceNumber = i + 1
        path = os.path.join(out, "%02d.dcm" % i)
        ds.save_as(path, write_like_original=False)
        if i == truncate:
            size = os.path.getsize(path)
            with open(path, "r+b") as f:
                f.truncate(size - max(size // 4, 64))
        paths.append(path)
    return study, paths


def wait_ended(p, tenant, study, n, timeout=120):
    """Waits until n receptions of study have ended, and none is in progress."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        rows = p.query(tenant, "SELECT state, ended FROM receptions WHERE study_uid = ?", study)
        if len(rows) >= n and all(r["ended"] for r in rows) and sum(1 for r in rows if r["ended"]) >= n:
            return rows
        time.sleep(0.2)
    return p.query(tenant, "SELECT state, ended FROM receptions WHERE study_uid = ?", study)


def storage_dir(p, tenant):
    return os.path.join(p.work, "roots", "fast", tenant, "main")


def read_object(p, tenant, key):
    """An archived object's bytes, read from where its tenant keeps them:
    the directory, or the bucket by a GET signed here."""
    if tenant in p.s3_tenants:
        return p.s3.get(p.s3_tenants[tenant] + key)
    with open(os.path.join(storage_dir(p, tenant), key), "rb") as f:
        return f.read()


def manifests_of(p, tenant, study):
    """Every manifest of the study's scans, read from the storage's meta tree."""
    rows = p.query(tenant, """SELECT subjects.label AS subject, sessions.label AS session, scans.label AS scan
        FROM scans JOIN sessions ON sessions.id = scans.session_id JOIN subjects ON subjects.id = sessions.subject_id
        WHERE sessions.study_uid = ?""", study)
    out = []
    for r in rows:
        out.append(json.loads(read_object(p, tenant, "/".join(["meta", r["subject"], r["session"], r["scan"], "manifest.json"]))))
    return out


def setup_tenant(p, tenant, port, callers=("STORESCU", "PYNETDICOM"), storage=None, create=()):
    p.m("tenant", "create", tenant, *create)
    p.m("setting", "set", "receiver.ports", str(port), "--tenant", tenant)
    p.m("setting", "set", "receiver.allow-as-received", "true", "--tenant", tenant)
    p.m("storage", "add", tenant, "main", *(storage or ["--type", "dir", "--root", "fast", "--path", "main"]))
    args = ["receiver", "add", tenant, "scanner", "--storage", "main", "--port", str(port), "--aet", "ARCHIVE",
            "--quiet-period", "%ds" % QUIET, "--enabled"]
    for c in callers:
        args += ["--allow-aet", c]
    p.m(*args)
    wait_up(p, tenant, "scanner")
    p.m("grant", "set", p.api_key_id, "--role", "reader", "--tenant", tenant)


def wait_up(p, tenant, receiver, want="RECEIVER_STATE_UP", timeout=30):
    deadline = time.time() + timeout
    while time.time() < deadline:
        r = json.loads(p.m("receiver", "show", tenant, receiver, "--json"))
        if r.get("receiver", r).get("health", {}).get("state") == want:
            return True
        time.sleep(0.1)
    return False


# ---- the sections ----

def arrives(p, port):
    """A study sent with storescu is archived as subject, session and scan;
    each file is read by dcmdump and pydicom and its data set is the one
    sent; every manifest entry's checksum is the object's."""
    sent = {}
    os.makedirs(os.path.join(p.work, "sent"), exist_ok=True)
    for name in CT:
        # dcmtk encodes a data set again as it sends it: what it sends is
        # the file as its own dcmconv writes it.
        out = os.path.join(p.work, "sent", name)
        subprocess.run([p.dcmtk("dcmconv"), os.path.join(CORPUS, name), out], check=True, capture_output=True)
        sent[str(dcmread(out, stop_before_pixels=True).SOPInstanceUID)] = data_set(out)
    n, out = p.storescu(port, [os.path.join(CORPUS, f) for f in CT])
    study = str(dcmread(os.path.join(CORPUS, CT[0]), stop_before_pixels=True).StudyInstanceUID)
    rows = wait_ended(p, "lab", study, 1)
    held = p.query("lab", "SELECT sop_instance_uid, storage_key FROM " + HELD + " WHERE study_uid = ?", study)
    p.result("1.1 a study sent with storescu is archived", n == 7 and [r["state"] for r in rows] == ["done"] and len(held) == 7,
             "%d of 7 answered success; receptions %s; %d instances held" % (n, [r["state"] for r in rows], len(held)), [out[-400:]])
    bad = []
    for r in held:
        path = os.path.join(storage_dir(p, "lab"), r["storage_key"])
        d = subprocess.run([p.dcmtk("dcmdump"), path], capture_output=True, text=True)
        if d.returncode != 0 or r["sop_instance_uid"] not in d.stdout:
            bad.append("dcmdump %s: exit %d" % (r["storage_key"], d.returncode))
        if str(dcmread(path).SOPInstanceUID) != r["sop_instance_uid"] or data_set(path) != sent.get(r["sop_instance_uid"]):
            bad.append("pydicom %s: the data set is not the one sent" % r["storage_key"])
    p.result("1.2 each archived file is read by dcmdump and pydicom, its data set byte for byte the one sent", not bad,
             "%d files" % len(held), bad)
    entries, bad = 0, []
    for man in manifests_of(p, "lab", study):
        for e in man["files"]:
            entries += 1
            with open(os.path.join(storage_dir(p, "lab"), e["key"]), "rb") as f:
                if hashlib.sha256(f.read()).hexdigest() != e["sha256"]:
                    bad.append(e["key"])
    p.result("1.3 every manifest entry's checksum is the SHA-256 of its object", entries == 7 and not bad,
             "%d entries, %d differ" % (entries, len(bad)), bad)


def two_parts(p, port):
    """A study sent in two parts, a gap longer than the quiet period between
    them: one session, two arrivals, the manifest holding both parts. Then
    the whole sent again: answered success, counted duplicate, nothing
    archived twice."""
    study, files = study_copy(p, CT, 6)
    first = p.pynetdicom_store(port, files[:3])
    wait_ended(p, "lab", study, 1)
    second = p.pynetdicom_store(port, files[3:])
    rows = wait_ended(p, "lab", study, 2)
    arrivals = sorted(r["arrival"] for r in p.query("lab", "SELECT arrival FROM receptions WHERE study_uid = ?", study))
    sessions = p.query("lab", "SELECT count(*) AS n FROM sessions WHERE study_uid = ?", study)[0]["n"]
    entries = sum(len(m["files"]) for m in manifests_of(p, "lab", study))
    p.result("2.1 two associations, the quiet period between them: one session, two arrivals, one manifest of both",
             first == [0] * 3 and second == [0] * 3 and sessions == 1 and arrivals == [1, 2] and entries == 6,
             "statuses %s %s; %d session(s); arrivals %s; %d manifest entries" % (first, second, sessions, arrivals, entries),
             [dict(r) for r in rows])
    again = p.pynetdicom_store(port, files)
    wait_ended(p, "lab", study, 3)
    last = p.query("lab", "SELECT files_duplicate, files_archived, state FROM receptions WHERE study_uid = ? AND arrival = 3", study)
    held = p.query("lab", "SELECT count(*) AS n, count(DISTINCT sop_instance_uid) AS d FROM " + HELD + " WHERE study_uid = ?", study)[0]
    ok = again == [0] * 6 and last and last[0]["files_duplicate"] == 6 and last[0]["files_archived"] == 0 and (held["n"], held["d"]) == (6, 6)
    p.result("2.2 the study sent again: answered success, counted duplicate, nothing archived twice", ok,
             "statuses %s; the third reception %s; %d instances, %d distinct" % (again, [dict(r) for r in last], held["n"], held["d"]))
    return study, files


def corrected(p, port, study, files):
    """One instance sent again with an attribute changed under its UID: the
    first copy kept, the second held in quarantine with both checksums."""
    ds = dcmread(files[0])
    uid = str(ds.SOPInstanceUID)
    ds.PatientComments = "corrected after the first send"
    path = os.path.join(p.work, "corrected.dcm")
    ds.save_as(path, write_like_original=False)
    with open(files[0], "rb") as f:
        first_sum = hashlib.sha256(f.read()).hexdigest()
    st = p.pynetdicom_store(port, [path])
    wait_ended(p, "lab", study, 4)
    q = p.query("lab", "SELECT reason, hex(sha256) AS sum, hex(kept_sha256) AS kept FROM quarantine WHERE sop_instance_uid = ?", uid)
    held = p.query("lab", "SELECT hex(archived_sha256) AS sum FROM " + HELD + " WHERE sop_instance_uid = ?", uid)
    ok = st == [0] and len(q) == 1 and q[0]["reason"] == "different_file" and q[0]["sum"] and q[0]["kept"] and \
        q[0]["sum"] != q[0]["kept"] and len(held) == 1
    p.result("3.1 a corrected file under the same UID: the first kept, the second in quarantine with both checksums", ok,
             "status %s; quarantine %s; the first file's SHA-256 %s" % (st, [dict(r) for r in q], first_sum[:16]))


def broken(p, port):
    """A data set cut inside its pixel data, its length still declaring the
    whole, sent with pynetdicom beside two whole files of its study: held
    as not DICOM with its reason, the two archived. pynetdicom encodes what
    it sends, and pydicom reads a cut file as a shorter whole one, so the
    cut bytes are handed to pynetdicom at its encoding step: the broken
    file reaches the server as a sender that died mid file would leave it."""
    import pynetdicom.association as association
    study, files = study_copy(p, ["MR_small.dcm"], 3)
    whole = data_set(files[1])
    cut = whole[: len(whole) - max(len(whole) // 4, 64)]
    encode = association.encode
    def cut_second(ds, *args, **kwargs):
        if str(ds.SOPInstanceUID) == str(dcmread(files[1], stop_before_pixels=True).SOPInstanceUID):
            return cut
        return encode(ds, *args, **kwargs)
    not_dicom = "SELECT count(*) AS n FROM quarantine WHERE reason = 'not_dicom' AND content_kept"
    before = p.query("lab", not_dicom)[0]["n"]
    association.encode = cut_second
    try:
        ae = AE(ae_title="PYNETDICOM")
        ae.acse_timeout = ae.dimse_timeout = ae.network_timeout = 30
        meta = dcmread(files[0], stop_before_pixels=True)
        ae.add_requested_context(str(meta.SOPClassUID), str(meta.file_meta.TransferSyntaxUID))
        assoc = ae.associate("127.0.0.1", port, ae_title="ARCHIVE")
        st = []
        if assoc.is_established:
            for f in files:
                r = assoc.send_c_store(dcmread(f))
                st.append(r.Status if "Status" in r else None)
            assoc.release()
    finally:
        association.encode = encode
    wait_ended(p, "lab", study, 1)
    # Its header unread, the file belongs to no study and no reception: it
    # is held under its receiver alone, its bytes kept.
    held_now = p.query("lab", not_dicom)[0]["n"] - before
    held = p.query("lab", "SELECT count(*) AS n FROM " + HELD + " WHERE study_uid = ?", study)[0]["n"]
    p.result("4.1 a file cut short is held as not DICOM with its bytes, the rest of its study archived",
             st == [0, 0, 0] and held == 2 and held_now == 1,
             "statuses %s; %d archived; %d new not-DICOM record(s) with bytes kept" % (st, held, held_now))


def strangers(p, port, closed_port):
    """A caller not listed is refused, and a receiver that lists nobody
    admits nobody."""
    code, out = p.echoscu(port, "STRANGER")
    p.result("5.1 a calling AE title not listed gets no association", code != 0 and "Calling AE Title Not Recognized" in out,
             "echoscu as STRANGER: exit %d" % code, [out[-300:]])
    p.m("setting", "set", "receiver.ports", "%d,%d" % (port, closed_port), "--tenant", "lab")
    p.m("receiver", "add", "lab", "closed", "--storage", "main", "--port", str(closed_port), "--aet", "ARCHIVE", "--enabled")
    wait_up(p, "lab", "closed")
    code, out = p.echoscu(closed_port, "STORESCU")
    p.result("5.2 a receiver that lists no caller admits nobody", code != 0 and "Calling AE Title Not Recognized" in out,
             "echoscu as STORESCU to it: exit %d" % code, [out[-300:]])


def wait_receptions_ended(p, tenant, study, timeout=120):
    """Waits until the study has a reception and every one has ended."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        rows = p.query(tenant, "SELECT ended FROM receptions WHERE study_uid = ?", study)
        if rows and all(x["ended"] for x in rows):
            return True
        time.sleep(0.2)
    return False


def crashes(p, port, rounds, tenant="crash", whens=("receiving", "quiet", "archiving")):
    """The server killed with signal 9 during reception, during the quiet
    period and during archive, again and again. After each restart, before
    anything is sent again, every file acknowledged before the kill must be
    archived (or in quarantine) from that first delivery alone: that is what
    shows nothing acknowledged was lost. Only then are the files not
    acknowledged sent, and the study must end with each instance archived
    once, each read back from where it is kept."""
    bad, kills = [], 0
    for r in range(rounds):
        for when in whens:
            # A study of 40 files for the archiving round, whose archive
            # step lasts long enough on a fast disk to be caught midway.
            n = 40 if when == "archiving" else 7
            study, files = study_copy(p, CT, n)
            uid = [str(dcmread(f, stop_before_pixels=True).SOPInstanceUID) for f in files]
            if when == "receiving":
                def kill_after_three(i):
                    if i == 2:
                        p.kill()
                statuses = p.pynetdicom_store(port, files, on_each=kill_after_three)
            else:
                statuses = p.pynetdicom_store(port, files)
                if when == "archiving":
                    # Identified is the state its archive step runs in.
                    deadline = time.time() + 30
                    caught = False
                    while time.time() < deadline:
                        rows = p.query(tenant, "SELECT state FROM receptions WHERE study_uid = ?", study)
                        if rows and rows[0]["state"] == "identified":
                            caught = True
                            break
                        if rows and rows[0]["state"] not in ("receiving", "complete"):
                            break
                        time.sleep(0.005)
                    if not caught:
                        bad.append("round %d: the kill missed the archive step (state %s)" % (r + 1, rows[0]["state"] if rows else "none"))
            acked = [i for i, s in enumerate(statuses) if s == 0]
            p.kill()
            kills += 1
            p.serve()
            wait_up(p, tenant, "scanner")
            if not acked:
                bad.append("round %d, killed while %s: no file was acknowledged before the kill, so nothing was tested" % (r + 1, when))
                continue
            # The acknowledged files, from their first delivery only.
            wait_receptions_ended(p, tenant, study)
            have = {x["sop_instance_uid"] for x in p.query(tenant, "SELECT sop_instance_uid FROM " + HELD + " WHERE study_uid = ?", study)}
            quarantined = {x["sop_instance_uid"] for x in p.query(tenant, "SELECT sop_instance_uid FROM quarantine WHERE study_uid = ?", study)}
            lost = [uid[i] for i in acked if uid[i] not in have and uid[i] not in quarantined]
            # Then the rest, as a sender would send what it was not told was
            # received.
            rest = [files[i] for i in range(len(files)) if i not in acked]
            again = p.pynetdicom_store(port, rest) if rest else []
            if rest:
                wait_receptions_ended(p, tenant, study)
            held = p.query(tenant, "SELECT sop_instance_uid, storage_key FROM " + HELD + " WHERE study_uid = ?", study)
            claimed = p.query(tenant, "SELECT count(*) AS n FROM instance_claims WHERE study_uid = ?", study)[0]["n"]
            distinct = {x["sop_instance_uid"] for x in held}
            unreadable = []
            for x in held:
                try:
                    if dcmread(_bytes_file(read_object(p, tenant, x["storage_key"]))).SOPInstanceUID != x["sop_instance_uid"]:
                        unreadable.append(x["storage_key"])
                except Exception as e:  # noqa: BLE001 - any failure to read back is the finding
                    unreadable.append("%s: %s" % (x["storage_key"], e))
            if lost or len(held) != n or len(distinct) != n or claimed or unreadable or again != [0] * len(rest):
                bad.append("round %d, killed while %s: %d of %d acknowledged lost before any resend; then %d held (%d distinct), "
                           "%d claimed, %d not read back, the rest's statuses %s"
                           % (r + 1, when, len(lost), len(acked), len(held), len(distinct), claimed, len(unreadable), again))
    return bad, kills


def crash_proof(p, port, rounds):
    bad, kills = crashes(p, port, rounds)
    p.result("6.1 killed with signal 9 while receiving, in the quiet period and while archiving: nothing lost, nothing twice",
             not bad, "%d kills and restarts; what was acknowledged checked before anything was sent again" % kills, bad)


def _bytes_file(b):
    return io.BytesIO(b)


def on_s3(p, port):
    """A tenant whose storage is a bucket: a study sent with storescu is
    archived; each object, read back with a GET signed here, is read by
    pydicom and dcmdump with its data set the one sent, and every manifest
    entry's checksum is its object's; killed while archiving, nothing is
    lost or archived twice in the bucket either."""
    study, files = study_copy(p, CT, 7)
    sent = {str(dcmread(f, stop_before_pixels=True).SOPInstanceUID): data_set(f) for f in files}
    statuses = p.pynetdicom_store(port, files)
    rows = wait_ended(p, "cloud", study, 1)
    held = p.query("cloud", "SELECT sop_instance_uid, storage_key FROM " + HELD + " WHERE study_uid = ?", study)
    bad = []
    for r in held:
        try:
            body = read_object(p, "cloud", r["storage_key"])
        except Exception as e:  # noqa: BLE001
            bad.append(str(e))
            continue
        path = os.path.join(p.work, "s3-read.dcm")
        with open(path, "wb") as f:
            f.write(body)
        d = subprocess.run([p.dcmtk("dcmdump"), path], capture_output=True, text=True)
        if d.returncode != 0 or r["sop_instance_uid"] not in d.stdout:
            bad.append("dcmdump %s: exit %d" % (r["storage_key"], d.returncode))
        if str(dcmread(path).SOPInstanceUID) != r["sop_instance_uid"] or data_set(path) != sent.get(r["sop_instance_uid"]):
            bad.append("pydicom %s: the data set is not the one sent" % r["storage_key"])
    p.result("10.1 a study archived to an S3 bucket is read back by a signed GET, dcmdump and pydicom, byte for byte the one sent",
             statuses == [0] * 7 and [x["state"] for x in rows] == ["done"] and len(held) == 7 and not bad,
             "statuses %s; receptions %s; %d objects read back" % (statuses, [x["state"] for x in rows], len(held)), bad)
    entries, bad = 0, []
    for man in manifests_of(p, "cloud", study):
        for e in man["files"]:
            entries += 1
            if hashlib.sha256(read_object(p, "cloud", e["key"])).hexdigest() != e["sha256"]:
                bad.append(e["key"])
    p.result("10.2 every manifest entry in the bucket has the SHA-256 of its object", entries == 7 and not bad,
             "%d entries, %d differ" % (entries, len(bad)), bad)
    bad, kills = crashes(p, port, 1, tenant="cloud", whens=("receiving", "archiving"))
    p.result("10.3 killed with signal 9 while receiving and while archiving to S3: nothing lost, nothing twice, every object read back",
             not bad, "%d kills and restarts" % kills, bad)


def on_postgres(p, port):
    """A tenant whose catalog is a Postgres database: a study is archived,
    every file on the storage read by dcmdump with its manifest's checksums
    right, the status's counts are the catalog's as psql reads them, and a
    kill while archiving loses nothing and doubles nothing there."""
    study, files = study_copy(p, CT, 7)
    statuses = p.pynetdicom_store(port, files)
    rows = wait_ended(p, "pg", study, 1)
    held = p.query("pg", "SELECT sop_instance_uid, storage_key FROM " + HELD + " WHERE study_uid = ?", study)
    bad = []
    for r in held:
        d = subprocess.run([p.dcmtk("dcmdump"), os.path.join(storage_dir(p, "pg"), r["storage_key"])], capture_output=True, text=True)
        if d.returncode != 0 or r["sop_instance_uid"] not in d.stdout:
            bad.append("dcmdump %s: exit %d" % (r["storage_key"], d.returncode))
    for man in manifests_of(p, "pg", study):
        for e in man["files"]:
            if hashlib.sha256(read_object(p, "pg", e["key"])).hexdigest() != e["sha256"]:
                bad.append("manifest checksum %s" % e["key"])
    p.result("11.1 a study archived for a tenant on Postgres, its catalog read by psql, every file read by dcmdump and its checksum right",
             statuses == [0] * 7 and [x["state"] for x in rows] == ["done"] and len(held) == 7 and not bad,
             "statuses %s; receptions %s (psql); %d instances held (psql)" % (statuses, [x["state"] for x in rows], len(held)), bad)
    st = p.call("nopacs.v1.MonitorService/GetStatus", {"scope": {"tenant": "pg"}})["tenants"][0]
    c = st.get("counts", {})
    want = p.query("pg", """SELECT (SELECT count(*) FROM subjects) AS subjects, (SELECT count(*) FROM sessions) AS sessions,
        (SELECT count(*) FROM scans) AS scans, (SELECT count(*) FROM instances) AS instances""")[0]
    got = {k: int(c.get(k, 0)) for k in ("subjects", "sessions", "scans", "instances")}
    p.result("11.2 the status's counts are the Postgres catalog's, read by psql", got == want, "status %s; psql %s" % (got, want))
    bad, kills = crashes(p, port, 1, tenant="pg", whens=("receiving", "archiving"))
    p.result("11.3 killed with signal 9 while receiving and while archiving on Postgres: nothing lost, nothing twice",
             not bad, "%d kills and restarts" % kills, bad)


def released_after_raise(p, port):
    """A file whose header is longer than the receiver's bound is held as
    header_too_large with its bytes; the bound is raised and the file
    released with the program's own command; the server is killed at once
    and started again; the file must end archived, read back by dcmdump, and
    stay so through another restart."""
    p.m("setting", "set", "receiver.header-limit", "1MiB")
    time.sleep(2)
    study, files = study_copy(p, CT, 1)
    ds = dcmread(files[0])
    ds.add_new((0x0009, 0x0010), "LO", "PROVE")
    ds.add_new((0x0009, 0x1001), "OB", b"\0" * (2 << 20))
    ds.save_as(files[0], write_like_original=False)
    uid = str(ds.SOPInstanceUID)
    statuses = p.pynetdicom_store(port, files)
    deadline = time.time() + 30
    rows = []
    while time.time() < deadline and not rows:
        rows = p.query("lab", "SELECT id FROM quarantine WHERE reason = 'header_too_large' AND content_kept AND state = 'held'")
        time.sleep(0.2)
    if not rows:
        p.result("12.1 a file held for its header's size, released once the bound is raised, is archived through a kill",
                 False, "statuses %s; no header_too_large record held" % statuses)
        return
    qid = rows[0]["id"]
    p.m("setting", "set", "receiver.header-limit", "64MiB")
    time.sleep(2)
    out = p.m("quarantine", "release", "lab", qid, "--reason", "the header bound was raised")
    p.kill()
    p.serve()
    wait_up(p, "lab", "scanner")
    deadline = time.time() + 90
    held = []
    while time.time() < deadline and not held:
        held = p.query("lab", "SELECT storage_key FROM " + HELD + " WHERE sop_instance_uid = ?", uid)
        time.sleep(0.5)
    p.stop()
    p.serve()
    wait_up(p, "lab", "scanner")
    still = p.query("lab", "SELECT storage_key FROM " + HELD + " WHERE sop_instance_uid = ?", uid)
    ok_dump = False
    if still:
        d = subprocess.run([p.dcmtk("dcmdump"), os.path.join(storage_dir(p, "lab"), still[0]["storage_key"])], capture_output=True, text=True)
        ok_dump = d.returncode == 0 and uid in d.stdout
    p.result("12.1 a file held for its header's size, released once the bound is raised, is archived through a kill",
             statuses == [0] and len(held) == 1 and len(still) == 1 and ok_dump,
             "acknowledged %s; held as header_too_large; released (%s); after kill -9 and restart: %d archived; after another restart: %d, dcmdump %s"
             % (statuses, out.strip().splitlines()[0] if out.strip() else "no output", len(held), len(still), "reads it" if ok_dump else "fails"))


def nearly_full(p, port):
    """With every disk below the reserve, a sender is refused as transient;
    when space returns, it is served."""
    p.m("setting", "set", "storage.reserve", "100%")
    down = wait_up(p, "lab", "scanner", want="RECEIVER_STATE_DOWN", timeout=20)
    code, out = p.echoscu(port, "STORESCU")
    refused = code != 0 and "Association Rejected" in out and "Transient" in out
    p.m("setting", "set", "storage.reserve", "1%")
    # Space is looked at every 5 seconds; a round of the tenant's worker
    # busy with other work has been seen to delay it to 25 seconds.
    up_started = time.time()
    up = wait_up(p, "lab", "scanner", timeout=60)
    up_after = time.time() - up_started
    code2, out2 = p.echoscu(port, "STORESCU")
    p.result("7.1 nearly full: refused as transient, then served once space returns", down and refused and up and code2 == 0,
             "down %s; echoscu while full exit %d; up again %s, %.1f s after the reserve was lowered; echoscu after exit %d"
             % (down, code, up, up_after, code2), [out[-300:], out2[-200:]])


def numbers(p, cursor):
    """The status and the events, read over HTTPS by Python, give the
    counts the catalog holds, read by SQLite."""
    st = p.call("nopacs.v1.MonitorService/GetStatus", {"scope": {"tenant": "lab"}})["tenants"][0]
    c = st.get("counts", {})
    want = p.query("lab", """SELECT (SELECT count(*) FROM subjects) AS subjects, (SELECT count(*) FROM sessions) AS sessions,
        (SELECT count(*) FROM scans) AS scans, (SELECT count(*) FROM instances) AS instances,
        (SELECT count(*) FROM quarantine WHERE state = 'held') AS held""")[0]
    got = {k: int(c.get(k, 0)) for k in ("subjects", "sessions", "scans", "instances")}
    got["held"] = int(c.get("quarantinedFiles", 0))
    p.result("8.1 the status's counts are the catalog's, read by SQLite", got == dict(want), "status %s; sqlite3 %s" % (got, dict(want)))
    conn, resp = p.watch({"scope": {"tenant": "lab"}, "cursor": cursor})
    done, quarantined, seen = 0, 0, 0
    conn.sock.settimeout(5)
    try:
        for msg in frames(resp):
            e = msg.get("event")
            if not e:
                continue
            seen += 1
            if e.get("receptionStep", {}).get("step") == "RECEPTION_STEP_DONE":
                done += 1
            if "fileQuarantined" in e:
                quarantined += 1
    except (socket.timeout, TimeoutError):
        pass
    conn.close()
    want_done = p.query("lab", "SELECT count(*) AS n FROM receptions WHERE state = 'done'")[0]["n"]
    want_q = p.query("lab", "SELECT count(*) AS n FROM quarantine")[0]["n"]
    p.result("8.2 the events of the stream, from the first status's cursor, count what the catalog holds",
             done == want_done and quarantined == want_q,
             "%d events: %d receptions done (sqlite3 %d), %d files quarantined (sqlite3 %d)" % (seen, done, want_done, quarantined, want_q))


def rss_kib(pid):
    return int(subprocess.run(["ps", "-o", "rss=", "-p", str(pid)], capture_output=True, text=True).stdout.strip() or 0)


def slow_watchers(p, port):
    """Streams that stop reading cost the server no memory and do not slow a
    reception: four are opened and never read while a study is received."""
    def receive():
        study, files = study_copy(p, CT, 14)
        t0 = time.time()
        p.pynetdicom_store(port, files)
        wait_ended(p, "lab", study, 1)
        return time.time() - t0
    alone = receive()
    before = rss_kib(p.server.pid)
    stalled = [p.watch({"scope": {"tenant": "lab"}}) for _ in range(4)]
    with_them = receive()
    receive()
    after = rss_kib(p.server.pid)
    for conn, _ in stalled:
        conn.close()
    grew = (after - before) / 1024
    p.result("9.1 streams that stop reading cost no memory and do not slow a reception",
             grew < 64 and with_them < alone * 2 + 2,
             "a study of 14 files received in %.1f s alone, %.1f s beside 4 streams never read; the server's memory grew %.1f MiB"
             % (alone, with_them, grew))


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--go", default="go")
    ap.add_argument("--dcmtk", default="")
    ap.add_argument("--crash-rounds", type=int, default=2)
    ap.add_argument("--s3", default="", help="http://KEY:SECRET@HOST:PORT/BUCKET of an S3 server for the S3 checks")
    ap.add_argument("--postgres", default="", help="postgres://USER:PASSWORD@HOST:PORT/DB of a server the proof may make a database on")
    ap.add_argument("--psql", default="", help='the command that runs psql against that server, such as "docker exec -i CONTAINER psql -U postgres"')
    args = ap.parse_args(argv)
    p = Proof(args)
    try:
        build_binaries(args.go, p.bin, p.sbin)
        p.cert, p.key_file = os.path.join(p.work, "cert.pem"), os.path.join(p.work, "key.pem")
        subprocess.run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-days", "1",
                        "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1", "-keyout", p.key_file, "-out", p.cert],
                       check=True, capture_output=True)
        p.api_port, lab_port, closed_port, crash_port, cloud_port, pg_port = free_ports(6)
        p.m("init")
        p.serve()
        p.api_key = p.m("key", "create", "--holder", "reception proof", "--no-expiry").strip()
        p.api_key_id = p.api_key.split("_")[1]
        os.makedirs(os.path.join(p.work, "roots", "fast"))
        p.m("setting", "set", "storage.directory-roots", "fast=" + os.path.join(p.work, "roots", "fast"))
        setup_tenant(p, "lab", lab_port)
        setup_tenant(p, "crash", crash_port)
        if args.s3:
            p.s3 = S3(args.s3)
            p.s3.make_bucket()
            p.m("setting", "set", "storage.s3-endpoints", p.s3.endpoint + " plain-http private-address")
            prefix = "prove-%d/" % int(time.time() * 1000)
            os.environ["PROVE_S3_SECRET"] = p.s3.secret
            setup_tenant(p, "cloud", cloud_port, storage=["--type", "s3", "--endpoint", p.s3.endpoint, "--region", p.s3.region,
                                                          "--bucket", p.s3.bucket, "--prefix", prefix, "--path-style",
                                                          "--access-key-id", p.s3.key, "--credential-env", "PROVE_S3_SECRET"])
            p.s3_tenants["cloud"] = prefix
        if args.postgres and args.psql:
            db = "prove_reception_%d" % int(time.time() * 1000)
            p.psql("postgres", "CREATE DATABASE " + db)
            p.pg_made = db
            base = urllib.parse.urlsplit(args.postgres)
            os.environ["PROVE_PG_ADDRESS"] = urllib.parse.urlunsplit((base.scheme, base.netloc, "/" + db, base.query, ""))
            p.pg_tenants["pg"] = db
            setup_tenant(p, "pg", pg_port, create=["--db-env", "PROVE_PG_ADDRESS"])
        cursor = p.call("nopacs.v1.MonitorService/GetStatus", {"scope": {"tenant": "lab"}})["tenants"][0]["eventCursor"]
        arrives(p, lab_port)
        study, files = two_parts(p, lab_port)
        corrected(p, lab_port, study, files)
        broken(p, lab_port)
        strangers(p, lab_port, closed_port)
        crash_proof(p, crash_port, args.crash_rounds)
        if args.s3:
            on_s3(p, cloud_port)
        else:
            p.skip("10.1-10.3 a tenant on S3", "no --s3 given (make s3-up prints a URL)")
        if p.pg_tenants:
            on_postgres(p, pg_port)
        else:
            p.skip("11.1-11.3 a tenant on Postgres", "no --postgres and --psql given")
        released_after_raise(p, lab_port)
        nearly_full(p, lab_port)
        numbers(p, cursor)
        slow_watchers(p, lab_port)
    finally:
        p.stop()
        if p.pg_made:
            p.psql("postgres", "DROP DATABASE IF EXISTS %s WITH (FORCE)" % p.pg_made)
    print("%d ok, %d FAIL, %d skipped" % (p.counts["ok"], p.counts["FAIL"], p.counts["skip"]))
    if p.counts["FAIL"]:
        print("the work directory is kept: %s" % p.work)
        return 1
    subprocess.run(["rm", "-rf", p.work])

