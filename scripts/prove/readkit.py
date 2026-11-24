"""What the reading sections of the proof share (scripts/prove/reading.py).

The reading installation the proof builds is described by a state file in
its data directory, written once it is set up and read by every later
section: the server's address, the tenants and their storages, the files
sent, and where each key is kept. Every request here is made by a tool that
is not NoPACs: Python's http.client in the Connect protocol (JSON both ways,
https://connectrpc.com/docs/protocol), curl, sqlite3, and the binary's own
command line only to set the installation up and to download as a person
does. Nothing imports NoPACs' code but the Python client, in its own section.
"""

import glob
import hashlib
import http.client
import json
import os
import socket
import ssl
import struct
import subprocess
import time


ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
CORPUS = os.path.join(ROOT, "internal", "receive", "testdata")
CHECK = os.path.join(ROOT, "scripts", "check")

# The public files sent: one CT study of two series, an MR image, and three
# multi-frame or compressed images (an enhanced segmentation, RLE with two
# frames, JPEG baseline with three frames in several fragments each). None
# shares a SOP Instance UID with another (MR_small's compressed copies do,
# and are left out).
SENT = ["ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm", "ct5n-3023.dcm",
        "ct5n-3353.dcm", "MR_small.dcm", "liver_1frame.dcm", "SC_rgb_rle_2frame.dcm", "jpeg_3frame_nobot.dcm"]

# The least quiet period a receiver takes: a reception is complete this long
# after its last file.
QUIET = "5s"


class State:
    """The reading installation, as the state file holds it."""

    def __init__(self, work):
        self.work = work
        self.dir = os.path.join(work.path, "rd")
        self.path = os.path.join(self.dir, "state.json")
        self.d = {}
        if os.path.exists(self.path):
            with open(self.path) as f:
                self.d = json.load(f)

    def save(self):
        with open(self.path, "w") as f:
            json.dump(self.d, f, indent=1)

    def __getitem__(self, k):
        return self.d[k]

    def key(self, name):
        with open(os.path.join(self.dir, "keys", name)) as f:
            return f.read().strip()

    def key_id(self, name):
        return self.key(name).split("_")[1]

    def tenants(self):
        return self.d.get("tenants", [])

    def storage_dir(self, tenant, storage):
        """A directory storage's root: <directory root>/<tenant>/<path>."""
        return os.path.join(self.d["roots"], tenant, storage)


def cli(state, *args, env=None, timeout=120):
    """The binary's own command on the reading installation's local socket;
    returns (exit status, stdout, stderr)."""
    e = {k: v for k, v in os.environ.items() if k not in ("NOPACS_SERVER", "NOPACS_KEY")}
    e.update(env or {})
    p = subprocess.run([state["bin"], "--data-dir", state["data"], *args], capture_output=True, text=True, timeout=timeout, env=e)
    return p.returncode, p.stdout, p.stderr


def cli_ok(state, *args, env=None):
    rc, out, err = cli(state, *args, env=env)
    if rc != 0:
        raise RuntimeError("nopacs %s: exit %d: %s" % (" ".join(args), rc, err.strip()))
    return out


def https(state):
    ctx = ssl.create_default_context(cafile=state["ca"])
    return http.client.HTTPSConnection("127.0.0.1", state["port"], context=ctx, timeout=60)


def request(state, method, path, key=None, body=None, headers=None):
    """One HTTPS request; returns (status, headers by lower-case name, body)."""
    h = dict(headers or {})
    if key:
        h["Authorization"] = "Bearer " + key
    c = https(state)
    try:
        c.request(method, path, body, h)
        r = c.getresponse()
        data = r.read()
        return r.status, {k.lower(): v for k, v in r.getheaders()}, data
    finally:
        c.close()


def call(state, procedure, body, key):
    """One unary call in the Connect protocol, JSON both ways; returns
    (status, answer, request id)."""
    status, h, data = request(state, "POST", "/" + procedure, key, json.dumps(body),
                              {"Content-Type": "application/json", "Connect-Protocol-Version": "1"})
    try:
        answer = json.loads(data or b"{}")
    except ValueError:
        answer = {"undecodable": data[:200].decode("latin-1")}
    return status, answer, h.get("nopacs-request-id", "")


def must(state, procedure, body, key):
    status, answer, _ = call(state, procedure, body, key)
    if status != 200:
        raise RuntimeError("%s: HTTP %d %s" % (procedure, status, answer))
    return answer


def pages(state, procedure, body, key, field, page_size):
    """Every item of a list call, page after page by next_page_token; also
    the number of pages it took."""
    out, token, n = [], "", 0
    while True:
        b = dict(body, pageSize=page_size)
        if token:
            b["pageToken"] = token
        answer = must(state, procedure, b, key)
        n += 1
        out += answer.get(field, [])
        token = answer.get("nextPageToken", "")
        if not token:
            return out, n
        if n > 10000:
            raise RuntimeError("%s: more than 10000 pages" % procedure)


def stream_frame(body):
    """A Connect streaming request body: one message in its envelope."""
    payload = json.dumps(body).encode()
    return b"\0" + struct.pack(">I", len(payload)) + payload


def frames(data):
    """The messages of a Connect stream's body: (flags, message) pairs."""
    out, i = [], 0
    while i + 5 <= len(data):
        flags, n = data[i], struct.unpack(">I", data[i + 1:i + 5])[0]
        out.append((flags, json.loads(data[i + 5:i + 5 + n] or b"{}")))
        i += 5 + n
    return out


class UnixHTTP(http.client.HTTPConnection):
    """HTTP on the server's local socket, which needs no key."""

    def __init__(self, path, timeout=60):
        super().__init__("local", timeout=timeout)
        self.sock_path = path

    def connect(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(self.timeout)
        s.connect(self.sock_path)
        self.sock = s


def watch_until(state, want, timeout=60):
    """Reads the installation's events from the first kept, on the local
    socket, until want(event) holds for one or the time runs out; returns
    (the event or None, every event read)."""
    c = UnixHTTP(os.path.join(state["data"], "run", "nopacs.sock"), timeout=timeout)
    c.request("POST", "/nopacs.v1.MonitorService/Watch", stream_frame({"scope": {"installation": {}}, "resume": True}),
              {"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"})
    r = c.getresponse()
    seen, deadline = [], time.time() + timeout
    try:
        while time.time() < deadline:
            head = r.read(5)
            if len(head) < 5:
                break
            n = struct.unpack(">I", head[1:])[0]
            msg = json.loads(r.read(n) or b"{}")
            if head[0] & 2:
                break
            ev = msg.get("event")
            if ev:
                seen.append(ev)
                if want(ev):
                    return ev, seen
    except (socket.timeout, TimeoutError):
        pass
    finally:
        c.close()
    return None, seen


def manifests(state, tenant, storage):
    """Every scan manifest of a directory storage, as the storage holds it."""
    out = []
    for p in sorted(glob.glob(os.path.join(state.storage_dir(tenant, storage), "meta", "**", "manifest.json"), recursive=True)):
        with open(p) as f:
            out.append((p, json.load(f)))
    return out


def archived(state, tenant, storage):
    """Each archived file of a storage by SOP Instance UID: its path on the
    storage and its manifest entry."""
    out = {}
    root = state.storage_dir(tenant, storage)
    for _, m in manifests(state, tenant, storage):
        for e in m["files"]:
            out[e["sop_instance_uid"]] = (os.path.join(root, e["key"]), e)
    return out


def sha256_file(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def sha256_tool(paths):
    """The SHA-256 of each file by the system's own tool (sha256sum, or
    shasum -a 256 where there is none), by path."""
    cmd = ["sha256sum"] if subprocess.run(["sh", "-c", "command -v sha256sum"], capture_output=True).returncode == 0 else ["shasum", "-a", "256"]
    out = {}
    if not paths:
        return out
    p = subprocess.run(cmd + list(paths), capture_output=True, text=True, check=True)
    for line in p.stdout.splitlines():
        digest, name = line.split(None, 1)
        out[name.lstrip("*")] = digest
    return out


def run_check(work, python, script, args, env=None, timeout=900):
    """Runs one of scripts/check/*.py; returns (exit status, what it printed
    on standard output, and that with its standard error after it)."""
    e = dict(os.environ)
    e.update(env or {})
    p = subprocess.run([python, os.path.join(CHECK, script), *args], capture_output=True, text=True, env=e, timeout=timeout)
    return p.returncode, work.redact(p.stdout), work.redact(p.stdout + p.stderr)


def last_line(text):
    lines = [x for x in text.strip().splitlines() if x.strip()]
    return lines[-1] if lines else "(no output)"


def instance_path(tenant, key):
    """The download route of an archived file, from its key on the storage
    (data/<subject>/<session>/<scan>/<SOP Instance UID>.dcm)."""
    _, subject, session, scan, name = key.split("/")
    return "/tenants/%s/instances/%s/%s/%s/%s" % (tenant, subject, session, scan, name[:-len(".dcm")])


def receptions(state, tenant, receiver):
    """The receptions of a receiver, as "nopacs reception list" answers them,
    and a line that says what they are."""
    rc, out, err = cli(state, "--json", "reception", "list", tenant, "--receiver", receiver, "--all")
    if rc != 0:
        return None, err.strip()
    rs = json.loads(out).get("receptions", [])
    n = sum(int(r["counts"]["filesArchived"]) for r in rs)
    steps = sorted({r["step"].replace("RECEPTION_STEP_", "").lower() for r in rs})
    return rs, "%d receptions %s, %d files archived" % (len(rs), steps, n)


def wait_receptions(state, tenant, receiver, files, timeout=120):
    """Waits until every reception of the receiver has ended done and they
    archived files together; returns (whether they did, what was seen)."""
    deadline, seen = time.time() + timeout, ""
    while time.time() < deadline:
        rs, seen = receptions(state, tenant, receiver)
        if rs:
            n = sum(int(r["counts"]["filesArchived"]) for r in rs)
            if all(r["step"] == "RECEPTION_STEP_DONE" for r in rs) and n >= files:
                return True, seen
            if any(r["step"] == "RECEPTION_STEP_FAILED" for r in rs):
                return False, seen
        time.sleep(0.5)
    return False, seen
