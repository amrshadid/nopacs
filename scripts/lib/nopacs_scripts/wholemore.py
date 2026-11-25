"""What the later groups of the proof of keeping the archive whole
(scripts/prove/whole.py) use that is not a check: an S3 server in a
container of its own, read by boto3; a second tenant on the same
installation; a DICOM peer of pynetdicom; the event feed and the ReadFiles
stream read over the Connect protocol by Python's standard library; the scan
sidecars read from a directory storage; and dcmtk's storescu with the
arguments a group chooses. Nothing here imports NoPACs' code.
"""

import base64
import http.client
import importlib.util
import json
import os
import secrets
import struct
import subprocess
import time

from nopacs_scripts.net import free_port
from nopacs_scripts.read import UnixHTTP, frames, stream_frame
from nopacs_scripts.report import Failure

# The S3 server the proof runs: Silo, the maintained fork of MinIO, at the
# digest the repository's S3 tests pin, so a run here meets the server they
# meet.
SILO_IMAGE = "pgsty/silo@sha256:c20d112390b9a9ce8c11b8af95ad35bcaa62f4759663c09c936cd8d2b511f329"
SECRET_ENV = "NOPACS_PROVE_S3_SECRET"  # the variable the command line reads a storage's secret from


# ---- an S3 server in a container ----

class S3Server:
    """Silo in a container named for this run, on a loopback port of its
    own, with credentials made for this run; removed by stop."""

    def __init__(self, docker, image):
        self.docker, self.image = docker, image
        self.port = free_port()
        self.name = "nopacs-whole-s3-%d-%s" % (os.getpid(), secrets.token_hex(3))
        self.user = "prove" + secrets.token_hex(4)
        self.secret = secrets.token_hex(16)
        self.started = False

    @property
    def endpoint(self):
        return "http://127.0.0.1:%d" % self.port

    def start(self, timeout=90):
        if importlib.util.find_spec("boto3") is None:
            raise Failure("this group reads the S3 server with boto3, which this Python lacks (scripts/requirements/prove-whole.txt)")
        p = subprocess.run([self.docker, "run", "-d", "--name", self.name, "-p", "127.0.0.1:%d:9000" % self.port,
                            "-e", "MINIO_ROOT_USER=" + self.user, "-e", "MINIO_ROOT_PASSWORD=" + self.secret, self.image, "server", "/data"],
                           capture_output=True, text=True, timeout=300)
        if p.returncode != 0:
            raise Failure("the S3 server's container did not start: %s" % p.stderr.strip()[-300:])
        self.started = True
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                self.client().list_buckets()
                return
            except Exception:  # noqa: BLE001 - the server is not up yet, whatever it answers
                time.sleep(1)
        raise Failure("the S3 server did not answer in %d s: %s" % (timeout, self.logs()))

    def logs(self):
        p = subprocess.run([self.docker, "logs", "--tail", "20", self.name], capture_output=True, text=True, timeout=60)
        return (p.stdout + p.stderr).strip()[-600:]

    def stop(self):
        if self.started:
            subprocess.run([self.docker, "rm", "-f", self.name], capture_output=True, timeout=120)
            self.started = False

    def client(self):
        import boto3
        from botocore.config import Config
        return boto3.client("s3", endpoint_url=self.endpoint, aws_access_key_id=self.user, aws_secret_access_key=self.secret,
                            region_name="us-east-1", config=Config(s3={"addressing_style": "path"}, retries={"max_attempts": 2}))


def versions_of(client, bucket, key):
    """Every version and delete marker boto3 lists of exactly key:
    [(version id, "version" or "marker", is latest)]."""
    out = []
    kw = {"Bucket": bucket, "Prefix": key}
    while True:
        page = client.list_object_versions(**kw)
        out += [(v["VersionId"], "version", v["IsLatest"]) for v in page.get("Versions", []) if v["Key"] == key]
        out += [(m["VersionId"], "marker", m["IsLatest"]) for m in page.get("DeleteMarkers", []) if m["Key"] == key]
        if not page.get("IsTruncated"):
            return out
        kw.update(KeyMarker=page["NextKeyMarker"], VersionIdMarker=page["NextVersionIdMarker"])


def keys_under(client, bucket, prefix):
    """Every key with any version or delete marker under prefix, by boto3."""
    out = set()
    kw = {"Bucket": bucket, "Prefix": prefix}
    while True:
        page = client.list_object_versions(**kw)
        out |= {v["Key"] for v in page.get("Versions", []) + page.get("DeleteMarkers", [])}
        if not page.get("IsTruncated"):
            return out
        kw.update(KeyMarker=page["NextKeyMarker"], VersionIdMarker=page["NextVersionIdMarker"])


def add_s3_storage(p, tenant, name, server, bucket):
    """A storage of the tenant on a bucket of the S3 server, its secret
    given to the command line in the environment."""
    os.environ[SECRET_ENV] = server.secret
    p.m("storage", "add", tenant, name, "--type", "s3", "--endpoint", server.endpoint, "--region", "us-east-1", "--bucket", bucket,
        "--access-key-id", server.user, "--credential-env", SECRET_ENV, "--path-style")


# ---- receivers, tenants and sending ----

def receiver(p, tenant, name, storage, port, *extra):
    """A DICOM receiver of the tenant on the port given, as received unless
    extra names a profile; the port is added to the tenant's allowed ports."""
    # The ports Proof.setup allowed, and every one added since.
    have = getattr(p, "allowed_ports", {}).get(tenant) or ([p.raw_port, p.deid_port] if tenant == "lab" else [])
    have = have + [port]
    p.allowed_ports = dict(getattr(p, "allowed_ports", {}), **{tenant: have})
    p.m("setting", "set", "receiver.ports", ",".join(str(x) for x in have), "--tenant", tenant)
    p.m("receiver", "add", tenant, name, "--storage", storage, "--port", str(port), "--aet", "ARCHIVE", "--allow-aet", "STORESCU",
        "--quiet-period", "5s", *(extra or ("--as-received",)), "--enabled")


def second_tenant(p, tenant, port):
    """Another tenant of the installation: a directory storage, the basic
    profile, a de-identifying receiver on port, and the proof's key admin
    on it."""
    p.m("tenant", "create", tenant)
    p.allowed_ports = dict(getattr(p, "allowed_ports", {}), **{tenant: [port]})
    p.m("setting", "set", "receiver.ports", str(port), "--tenant", tenant)
    p.m("grant", "set", p.key_id, "--role", "admin", "--tenant", tenant)
    p.m("storage", "add", tenant, "main", "--type", "dir", "--root", "fast", "--path", "main")
    p.m("profile", "create", tenant, "basic", "--document", os.path.join(p.work, "basic.json"), "--note", "the basic profile")
    p.m("receiver", "add", tenant, "deid", "--storage", "main", "--port", str(port), "--aet", "ARCHIVE", "--allow-aet", "STORESCU",
        "--quiet-period", "5s", "--profile", "basic@1", "--enabled")


def storescu(p, port, files, *extra):
    """dcmtk's storescu with the extra arguments given; (answered success, output)."""
    exe = os.path.join(p.args.dcmtk, "storescu") if p.args.dcmtk else "storescu"
    r = subprocess.run([exe, "-v", *extra, "-aet", "STORESCU", "-aec", "ARCHIVE", "127.0.0.1", str(port), *files],
                       capture_output=True, text=True, timeout=170)
    out = r.stdout + r.stderr
    return out.count("Received Store Response (Success)"), out


def dcmtk(p, name):
    return os.path.join(p.args.dcmtk, name) if p.args.dcmtk else name


def wait_job(p, tenant, jid, timeout=150):
    """Waits for a job to end; returns it as the API shows it."""
    deadline = time.time() + timeout
    a = {}
    while time.time() < deadline:
        st, a = p.call("nopacs.v1.JobService/GetJob", {"tenant": tenant, "id": jid})
        if st == 200 and a["job"]["state"] in ("JOB_STATE_DONE", "JOB_STATE_PARTIAL", "JOB_STATE_FAILED", "JOB_STATE_CANCELED"):
            return a["job"]
        time.sleep(0.3)
    raise Failure("job %s did not end: %s" % (jid, json.dumps(a)[:300]))


def failures(p, tenant, jid):
    """Every failure a job lists."""
    out, token = [], ""
    while True:
        body = {"tenant": tenant, "id": jid, "pageSize": 500}
        if token:
            body["pageToken"] = token
        st, a = p.call("nopacs.v1.JobService/ListJobFailures", body)
        if st != 200:
            raise Failure("ListJobFailures: HTTP %d %s" % (st, a))
        out += a.get("failures", [])
        token = a.get("nextPageToken", "")
        if not token:
            return out


def server_errors(p):
    """The error lines of the server's log (serve.log), as the reason a
    check failed is read there."""
    try:
        with open(os.path.join(p.work, "serve.log"), errors="replace") as f:
            return [x.strip() for x in f if '"level":"ERROR"' in x]
    except OSError:
        return []


# ---- streams over the Connect protocol ----

def stream(p, procedure, body, timeout=60, limit=0, key=None, local=False):
    """One server-streaming call, JSON framed: (HTTP status, [messages],
    the end-of-stream message). With limit, stops reading after that many
    messages (a stream that does not end by itself). local calls on the
    server's own socket, as the local operator, with no key."""
    if local:
        c = UnixHTTP(os.path.join(p.data, "run", "nopacs.sock"), timeout=timeout)
        headers = {"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"}
    else:
        c = http.client.HTTPConnection("127.0.0.1", p.api_port, timeout=timeout)
        headers = {"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1", "Authorization": "Bearer " + (key or p.key)}
    msgs, end = [], {}
    try:
        c.request("POST", "/" + procedure, stream_frame(body), headers)
        r = c.getresponse()
        if r.status != 200:
            return r.status, [], json.loads(r.read() or b"{}")
        while True:
            head = r.read(5)
            if len(head) < 5:
                break
            n = struct.unpack(">I", head[1:])[0]
            msg = json.loads(r.read(n) or b"{}")
            if head[0] & 0x02:
                end = msg
                break
            msgs.append(msg)
            if limit and len(msgs) >= limit:
                break
    except (OSError, http.client.HTTPException) as e:
        end = {"cut": repr(e)}
    finally:
        c.close()
    return 200, msgs, end


def error_text(err):
    """An error of the Connect protocol as text: its code, its message and
    the bytes of its details decoded, where the reason of an ErrorInfo is
    written (google.rpc.ErrorInfo)."""
    if not err:
        return ""
    out = [str(err.get("code", "")), str(err.get("message", ""))]
    for d in err.get("details", []):
        v = d.get("value", "")
        try:
            out.append(base64.b64decode(v + "=" * (-len(v) % 4)).decode("latin-1"))
        except (ValueError, TypeError):
            out.append(str(v))
        if d.get("debug"):
            out.append(json.dumps(d["debug"]))
    return " ".join(out)


def read_files_raw(p, body, timeout=120):
    """DataService.ReadFiles as the bytes came: (HTTP status, {SOP Instance
    UID: (announced SHA-256, bytes read)}, the error the stream ended with
    or None, the number of chunks read before it)."""
    c = http.client.HTTPConnection("127.0.0.1", p.api_port, timeout=timeout)
    try:
        c.request("POST", "/nopacs.v1.DataService/ReadFiles", stream_frame(dict(body)),
                  {"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1", "Authorization": "Bearer " + p.key})
        r = c.getresponse()
        try:
            data = r.read()
        except http.client.IncompleteRead as e:
            data = e.partial
        status = r.status
    finally:
        c.close()
    if status != 200:
        return status, {}, {"message": data[:300].decode("latin-1")}, 0
    out, current, err, chunks = {}, None, None, 0
    for flags, msg in frames(data):
        if flags & 0x02:
            err = msg.get("error")
            continue
        if "file" in msg:
            current = msg["file"]["sopInstanceUid"]
            out[current] = (msg["file"].get("sha256", ""), bytearray())
        elif "chunk" in msg and current:
            chunks += 1
            out[current][1].extend(base64.b64decode(msg["chunk"].get("data", "")))
    return status, {k: (s, bytes(b)) for k, (s, b) in out.items()}, err, chunks


# ---- the scan sidecars on a directory storage ----

def sidecar_lines(root):
    """{SOP Instance UID: [line, ...]} of every scan sidecar under a
    directory storage's meta/ tree, each line as JSON; a sidecar is any file
    there that is not a manifest and whose first line says a version."""
    out = {}
    for d, _, names in os.walk(os.path.join(root, "meta")):
        for n in names:
            if n == "manifest.json" or d.endswith(".corrections"):
                continue
            path = os.path.join(d, n)
            with open(path, "rb") as f:
                text = f.read()
            for line in text.splitlines():
                line = line.strip().rstrip(b",")
                if not line.startswith(b'{"sop_instance_uid"'):
                    continue
                try:
                    e = json.loads(line)
                except ValueError:
                    continue
                out.setdefault(e["sop_instance_uid"], []).append(e)
    return out
