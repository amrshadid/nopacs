"""What the proof of keeping the archive whole (scripts/prove/whole.py) uses
that is not a check: the installation it builds and runs through the program's
own commands, studies made from the public corpus, dcmtk's storescu, the API
called by Python's standard library, the storage directory listed with
checksums by hashlib, and the tenant's SQLite catalog read by SQLite itself.
Nothing here imports NoPACs' code.
"""

import http.client
import json
import os
import shutil
import signal
import subprocess
import tempfile
import time

from pydicom import dcmread
from pydicom.uid import generate_uid

from nopacs_scripts.db import query
from nopacs_scripts.digest import sha256_file
from nopacs_scripts.net import free_ports
from nopacs_scripts.paths import PROVE
from nopacs_scripts.proc import build_binaries
from nopacs_scripts.proof import Programs, Reporting
from nopacs_scripts.report import Failure

DEFAULT_CORPUS = "/Users/amrshadid/Downloads/go-dicom/.vscode/refrance/pydicom-main/src/pydicom/data/test_files"
QUIET = "5s"  # the least quiet period a receiver allows


def listing(root):
    """{relative path: (size, SHA-256)} of every file under a storage
    directory, read by Python alone."""
    out = {}
    for d, _, names in os.walk(root):
        for n in names:
            p = os.path.join(d, n)
            out[os.path.relpath(p, root)] = (os.path.getsize(p), sha256_file(p))
    return out


# Each held instance with its labels and the key of its object on the storage:
# a row keeps its key only when it is not the layout's key of its scan and UID.
HELD = """SELECT i.sop_instance_uid AS uid, s.study_uid AS study, s.subject_label AS subject, s.session_label AS session,
    s.label AS scan, lower(hex(coalesce(i.archived_sha256, i.sha256))) AS sha256,
    CASE WHEN i.storage_key = '' THEN 'data/' || s.subject_label || '/' || s.session_label || '/' || s.label || '/'
        || i.sop_instance_uid || '.dcm' ELSE i.storage_key END AS storage_key
    FROM instances AS i JOIN scans AS s ON s.id = i.scan_id ORDER BY 1"""


def make_study(corpus, out_dir, name, pid, sources, series_per=1):
    """Writes one new study of the patient given, one file per source, each
    with a new SOP Instance UID; returns the file paths."""
    os.makedirs(out_dir, exist_ok=True)
    study = generate_uid()
    paths = []
    for s in range(series_per):
        series = generate_uid()
        for i, src in enumerate(sources):
            ds = dcmread(os.path.join(corpus, src))
            ds.StudyInstanceUID, ds.SeriesInstanceUID = study, series
            ds.SOPInstanceUID = ds.file_meta.MediaStorageSOPInstanceUID = generate_uid()
            ds.PatientName, ds.PatientID = name, pid
            ds.InstanceNumber = i + 1
            ds.SeriesNumber = s + 1
            p = os.path.join(out_dir, "%s-%s-%d-%d.dcm" % (pid, study[-8:], s, i))
            ds.save_as(p, write_like_original=False)
            paths.append(p)
    return paths


class Proof(Reporting, Programs):
    """The run: its work directory, the server it runs, and its results."""

    def __init__(self, args, prefix="nopacs-whole."):
        self.args = args
        # A short directory: the local socket's path must fit the 104 bytes a Unix socket address holds on macOS.
        self.work = tempfile.mkdtemp(prefix=prefix, dir="/tmp")
        os.chmod(self.work, 0o700)
        self.data = os.path.join(self.work, "data")
        self.bin = os.path.join(self.work, "nopacs")
        self.sbin = os.path.join(self.work, "nopacs-server")
        self.roots = os.path.join(self.work, "roots", "fast")
        self.server = None
        self.api_port, self.raw_port, self.deid_port = free_ports(3)
        self.counts = {"ok": 0, "FAIL": 0}
        self.key = ""

    # ---- the program ----

    def build(self):
        if self.args.binary:
            shutil.copy(self.args.binary, self.bin)
            shutil.copy(self.args.server_binary, self.sbin)
        else:
            build_binaries(self.args.go, self.bin, self.sbin)

    def run(self, *args, data=None):
        return subprocess.run([self.exe(*args), "--data-dir", data or self.data, *args], capture_output=True, text=True, timeout=170,
                              env={k: v for k, v in os.environ.items() if k not in ("NOPACS_SERVER", "NOPACS_KEY")})

    def m(self, *args):
        p = self.run(*args)
        if p.returncode != 0:
            raise Failure("nopacs %s: exit %d: %s" % (" ".join(args), p.returncode, p.stderr.strip()))
        return p.stdout

    def serve(self, data=None):
        data = data or self.data
        log = open(os.path.join(self.work, "serve.log"), "a")
        self.server = subprocess.Popen([self.sbin, "--data-dir", data, "serve", "--listen", "127.0.0.1:%d" % self.api_port],
                                       stdout=log, stderr=log)
        for _ in range(300):
            if self.server.poll() is not None:
                raise Failure("the server exited at its start; see serve.log")
            if self.run("whoami", data=data).returncode == 0:
                return
            time.sleep(0.1)
        raise Failure("the server did not answer")

    def kill9(self):
        self.server.send_signal(signal.SIGKILL)
        self.server.wait(60)

    # ---- the API over HTTP on the loopback, by Python's standard library ----

    def call(self, procedure, body, key=None):
        """One unary call in the Connect protocol, JSON both ways; returns (HTTP status, answer)."""
        c = http.client.HTTPConnection("127.0.0.1", self.api_port, timeout=120)
        try:
            c.request("POST", "/" + procedure, json.dumps(body), {"Content-Type": "application/json", "Connect-Protocol-Version": "1",
                                                                  "Authorization": "Bearer " + (key or self.key)})
            r = c.getresponse()
            data = r.read()
            return r.status, json.loads(data or b"{}")
        finally:
            c.close()

    def curl(self, path, *extra, key=None):
        """curl against the server; returns (HTTP status, response headers, body bytes)."""
        hdr, body = os.path.join(self.work, "curl.hdr"), os.path.join(self.work, "curl.body")
        p = subprocess.run(["curl", "-s", "-D", hdr, "-o", body, "-w", "%{http_code}", "-H", "Authorization: Bearer " + (key or self.key),
                            *extra, "http://127.0.0.1:%d%s" % (self.api_port, path)], capture_output=True, text=True, timeout=120)
        code = int(p.stdout.strip() or 0)
        with open(hdr, "rb") as f:
            headers = {}
            for line in f.read().decode("latin-1").splitlines()[1:]:
                if ":" in line:
                    k, v = line.split(":", 1)
                    headers[k.strip().lower()] = v.strip()
        with open(body, "rb") as f:
            data = f.read()
        return code, headers, data, p.returncode

    # ---- the installation ----

    def setup(self, tenant="lab"):
        """init, serve, a key with admin on the tenant, a directory root, the
        tenant with an as-received receiver (raw) and a de-identifying one (deid)."""
        os.makedirs(self.roots)
        self.build()
        self.m("init")
        self.serve()
        self.key = self.m("key", "create", "--holder", "whole proof", "--no-expiry").splitlines()[0].strip()
        self.key_id = self.key.split("_")[1]
        self.m("setting", "set", "storage.directory-roots", "fast=" + self.roots)
        self.m("tenant", "create", tenant)
        self.m("setting", "set", "receiver.ports", "%d,%d" % (self.raw_port, self.deid_port), "--tenant", tenant)
        self.m("setting", "set", "receiver.allow-as-received", "true", "--tenant", tenant)
        self.m("grant", "set", self.key_id, "--role", "admin", "--tenant", tenant)
        self.m("storage", "add", tenant, "main", "--type", "dir", "--root", "fast", "--path", "main")
        doc = os.path.join(self.work, "basic.json")
        with open(doc, "w") as f:
            f.write('{"bases": ["annex-e/basic:2026d"]}')
        self.m("profile", "create", tenant, "basic", "--document", doc, "--note", "the basic profile")
        self.m("receiver", "add", tenant, "raw", "--storage", "main", "--port", str(self.raw_port), "--aet", "ARCHIVE", "--allow-aet", "STORESCU",
               "--quiet-period", QUIET, "--as-received", "--enabled")
        self.m("receiver", "add", tenant, "deid", "--storage", "main", "--port", str(self.deid_port), "--aet", "ARCHIVE", "--allow-aet", "STORESCU",
               "--quiet-period", QUIET, "--profile", "basic@1", "--enabled")

    def storage_root(self, tenant="lab", storage="main"):
        return os.path.join(self.roots, tenant, storage)

    def catalog(self, tenant="lab", data=None):
        return os.path.join(data or self.data, "tenants", tenant + ".db")

    def held(self, tenant="lab", data=None):
        return query(self.catalog(tenant, data), HELD)

    # ---- sending, with dcmtk ----

    def storescu(self, port, files):
        """Sends files with dcmtk's storescu; returns how many were answered success."""
        exe = os.path.join(self.args.dcmtk, "storescu") if self.args.dcmtk else "storescu"
        p = subprocess.run([exe, "-v", "-aet", "STORESCU", "-aec", "ARCHIVE", "127.0.0.1", str(port), *files],
                           capture_output=True, text=True, timeout=170)
        out = p.stdout + p.stderr
        return out.count("Received Store Response (Success)"), out

    def wait_held(self, n, tenant="lab", timeout=120):
        """Waits until the catalog holds n instances; returns how many it holds."""
        deadline = time.time() + timeout
        got = 0
        while time.time() < deadline:
            got = len(self.held(tenant))
            if got >= n:
                time.sleep(0.3)
                return len(self.held(tenant))
            time.sleep(0.3)
        return got

    # ---- jobs, by the API ----

    def job(self, tenant, kind, body, timeout=150):
        """Makes a job of the kind (scrub, rebuild) and waits for it to end;
        returns the job as the API shows it."""
        st, a = self.call("nopacs.v1.JobService/CreateJob", {"tenant": tenant, kind: body})
        if st != 200:
            raise Failure("CreateJob %s: HTTP %d %s" % (kind, st, a))
        jid = a["job"]["id"]
        deadline = time.time() + timeout
        while time.time() < deadline:
            st, a = self.call("nopacs.v1.JobService/GetJob", {"tenant": tenant, "id": jid})
            if a["job"]["state"] in ("JOB_STATE_DONE", "JOB_STATE_PARTIAL", "JOB_STATE_FAILED", "JOB_STATE_CANCELED"):
                return a["job"]
            time.sleep(0.3)
        raise Failure("job %s did not end" % jid)

    def findings(self, tenant="lab", **filters):
        """Every scrub finding that holds, with the kinds filtered."""
        out, token = [], ""
        while True:
            body = dict(filters, tenant=tenant, pageSize=500)
            if token:
                body["pageToken"] = token
            st, a = self.call("nopacs.v1.CorrectionService/ListScrubFindings", body)
            if st != 200:
                raise Failure("ListScrubFindings: HTTP %d %s" % (st, a))
            out += a.get("findings", [])
            token = a.get("nextPageToken", "")
            if not token:
                return out

    # ---- the catalog dump ----

    def dump(self, name, tenant="lab", data=None, times=False):
        """The tenant's catalog as catalog_dump.py writes it, without times (unless times) and without receptions; returns the file's path."""
        out = os.path.join(self.work, name)
        p = subprocess.run([self.args.python, os.path.join(PROVE, "catalog_dump.py"), "--sqlite", self.catalog(tenant, data), "--out", out,
                            "--skip-receptions"] + ([] if times else ["--skip-times"]), capture_output=True, text=True, timeout=120)
        if p.returncode != 0:
            raise Failure("catalog_dump.py: %s" % p.stderr.strip())
        return out
