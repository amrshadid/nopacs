#!/usr/bin/env python3
"""The parts of scripts/prove/run.sh that are easier to say in Python.

Each subcommand is one step of the proof, run by scripts/prove/run.sh against the
real binary it started; none imports anything of NoPACs'. They share the
script's work directory:

  results.tsv  one line per named check: name, ok / FAIL / skipped, numbers
  ledger.tsv   one line per request the proof made to the main server: its
               request id (from the response header, or "-" when the client
               does not show it), method, key id ("local" for the local
               operator), the outcome the audit log must hold, the tenant it
               named ("*" when not checked), and which client sent it
  secrets      every secret the proof made, one per line, so that no line
               printed here can show one (a secret is shown as its length and
               the first 8 hex digits of its SHA-256)

Subcommands:

  refusals  the refusal matrix of sections 2 and 4: every method of the
            contract, read from the proto files, with no key and with each kind
            of bad key, through grpcurl, Python's grpcio and curl over TLS; the
            answers compared byte for byte within each client. The services
            api/not-served.txt names are checked for what the server promises
            of a service it does not serve yet: the same refusal as any method
            to a caller without a valid key, UNIMPLEMENTED to one with a key,
            nothing done, and each request audited
  audit     section 8: the audit logs read through the API and reconciled
            against the ledger in both directions
  tui       section 10: the interactive program under a pseudo-terminal, with
            the terminal emulator of scripts/check/tui.py
  json      section 10: every --json document parsed
  metrics   section 8: /metrics parsed by the Prometheus client; no audit entry
            dropped or failed

Needs grpcio and prometheus-client (scripts/requirements/check-server.txt) for
refusals and metrics; the others use the standard library only.
"""

import argparse
import datetime
import hashlib
import importlib.util
import json
import os
import re
import secrets as pysecrets
import subprocess
import sys

UNAUTH_MESSAGE = "the request carries no valid key"
DENIED_MESSAGE = "the key may not do this"
NOT_FOUND_MESSAGE = "no tenant has this name"
# What the server answers an authenticated caller for a procedure no handler
# serves (internal/server).
NO_SERVICE_MESSAGE = "no such method"

BASE58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

# Methods that never change anything: what the interactive program may call.
READ_PREFIXES = ("Get", "List", "WhoAmI")

# The audit entries the server writes by itself, not for one request
# (internal/registry and servecmd): known and accounted for by name. Any other
# method that does not start with "/" or "GET " is unexpected, among them
# access.AuditWriter, the notice that entries were dropped.
INIT_METHOD = "nopacs init"
STARTED_METHOD = "nopacs.registry/CreateTenantStarted"
STATE_METHOD = "nopacs.registry/SetTenantState"


class Work:
    """The shared files of the work directory."""

    def __init__(self, path):
        self.path = path
        self.failed = 0

    def secrets(self):
        try:
            with open(os.path.join(self.path, "secrets")) as f:
                return [s for s in f.read().split("\n") if s]
        except FileNotFoundError:
            return []

    def redact(self, text):
        """Replace every known secret by its length and fingerprint, longest
        first, so a secret that contains another is replaced whole."""
        for s in sorted(self.secrets(), key=len, reverse=True):
            if s in text:
                fp = hashlib.sha256(s.encode()).hexdigest()[:8]
                text = text.replace(s, "<secret, %d bytes, sha256 %s...>" % (len(s), fp))
        return text

    def result(self, name, ok, detail, evidence=None, status=None):
        """Record and print one named check."""
        status = status or ("ok" if ok else "FAIL")
        detail = self.redact(detail).replace("\t", " ").replace("\n", " ")
        print("%-4s %s: %s" % (status, name, detail), flush=True)
        if status == "FAIL":
            self.failed += 1
            for line in (evidence or [])[:20]:
                print("     | " + self.redact(str(line)), flush=True)
        with open(os.path.join(self.path, "results.tsv"), "a") as f:
            f.write("%s\t%s\t%s\n" % (name, status, detail))

    def ledger(self, rid, method, key_id, outcome, tenant, source):
        with open(os.path.join(self.path, "ledger.tsv"), "a") as f:
            f.write("\t".join([rid or "-", method, key_id, outcome, tenant, source]) + "\n")


def outcome_of(code):
    """The audit outcome the contract gives an answer's code (audit.proto,
    AuditOutcome): done for OK, refused for UNAUTHENTICATED and
    PERMISSION_DENIED, failed for every other."""
    code = code.lower()
    if code in ("ok", "200"):
        return "done"
    if code in ("unauthenticated", "permission_denied", "permissiondenied", "401", "403"):
        return "refused"
    return "failed"


# ---------------------------------------------------------------------------
# The contract, read from the proto files.


# REQUEST_OF maps "service/method" to the name of its request message, FIELD1
# maps a message's name to the type of its field number 1 (None when it has
# none); both are filled by contract_methods, and OPEN_METHODS holds the
# "service/method" of the methods whose Role line says "none": open to any
# authenticated caller, grant or not.
REQUEST_OF = {}
FIELD1 = {}
OPEN_METHODS = set()


def block(text, start):
    """The text from start, just after an opening brace, to its closing one."""
    depth, i = 1, start
    while depth and i < len(text):
        depth += {"{": 1, "}": -1}.get(text[i], 0)
        i += 1
    return text[start:i]


def contract_methods(proto_dir):
    """Every (service, method, proto file, streaming) of the contract, in file
    order. It also records each method's request type, the type of the first
    field of each message, and the methods open to any authenticated caller."""
    out = []
    pkg_dir = os.path.join(proto_dir, "nopacs", "v1")
    for name in sorted(os.listdir(pkg_dir)):
        if not name.endswith(".proto"):
            continue
        with open(os.path.join(pkg_dir, name)) as f:
            raw_text = f.read()
        text = re.sub(r"//[^\n]*", "", raw_text)
        open_names = set()
        for msg in re.finditer(r"\bmessage\s+(\w+)\s*\{", text):
            body = block(text, msg.end())
            f1 = re.search(r"(?:\b(repeated)\s+)?\b([\w.]+)\s+\w+\s*=\s*1\s*[;\[]", body)
            FIELD1[msg.group(1)] = ((f1.group(1) + " " if f1.group(1) else "") + f1.group(2)) if f1 else None
        # A method is open when the comment right above it says "Role: none;".
        for rpc in re.finditer(r"((?:[ \t]*//[^\n]*\n)+)[ \t]*rpc\s+(\w+)\s*\(", raw_text):
            if re.search(r"Role:\s*none\s*;", rpc.group(1)):
                open_names.add(rpc.group(2))
        for svc in re.finditer(r"service\s+(\w+)\s*\{", text):
            for rpc in re.finditer(r"\brpc\s+(\w+)\s*\(\s*(\w+)\s*\)\s*returns\s*\((\s*stream\b)?", block(text, svc.end())):
                key = "nopacs.v1.%s/%s" % (svc.group(1), rpc.group(1))
                REQUEST_OF[key] = rpc.group(2)
                if rpc.group(1) in open_names:
                    OPEN_METHODS.add(key)
                out.append(("nopacs.v1." + svc.group(1), rpc.group(1), "nopacs/v1/" + name, bool(rpc.group(3))))
    return out


def not_served(path, methods):
    """The lines of the list at path, and the problems with it: a line that is
    neither a full service name nor a full service name, "/" and a method's
    name, a line given twice, a name no proto file defines."""
    names, problems = [], []
    defined = {m[0] for m in methods} | {"%s/%s" % (m[0], m[1]) for m in methods}
    with open(path) as f:
        for n, line in enumerate(f, 1):
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            if not re.fullmatch(r"[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*\.[A-Z][A-Za-z0-9]*(/[A-Z][A-Za-z0-9]*)?", line) or line in names:
                problems.append("line %d: %r is not a full service or method name given once" % (n, line))
                continue
            names.append(line)
            if line not in defined:
                problems.append("%s is listed as not served, and no proto file defines it" % line)
    return names, problems


def listed(method, names):
    """Whether the list's lines name a method (service, name, file, streaming):
    its service whole, or the method itself."""
    return method[0] in names or "%s/%s" % (method[0], method[1]) in names


def ns_body(svc, method, tenant):
    """A request to a method not served yet, as JSON and as protobuf, built
    from its request message's first field: a string is the tenant's name, a
    Scope is the scope that names it, anything else (no field, a list, a
    number) is sent empty, which grpcurl and the server both accept."""
    first = FIELD1.get(REQUEST_OF.get("%s/%s" % (svc, method)))
    if first == "string":
        return {"tenant": tenant}, pb_string(1, tenant)
    if first == "Scope":
        return {"scope": {"tenant": tenant}}, pb_string(1, pb_string(1, tenant))
    return {}, b""


def db_state(data_dir, tenant):
    """What a change could alter in the master and in a SQLite tenant's
    database: every row of every table, except the master's audit log and
    keys (a refusal's entry and a key's last use are written by any request)
    and the tenant's audit log. Read only."""
    import sqlite3

    out = {}
    for label, path, skip in (("master", os.path.join(data_dir, "master.db"), ("audit", "api_keys")),
                              (tenant, os.path.join(data_dir, "tenants", tenant + ".db"), ("audit",))):
        con = sqlite3.connect("file:%s?mode=ro" % path, uri=True)
        try:
            tables = [r[0] for r in con.execute("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")]
            for t in tables:
                if t in skip:
                    continue
                rows = con.execute('SELECT * FROM "%s"' % t.replace('"', '""')).fetchall()
                out["%s.%s" % (label, t)] = hashlib.sha256(repr(sorted(map(repr, rows))).encode()).hexdigest()
        finally:
            con.close()
    return out


def pb_string(field, value):
    """A protobuf length-delimited field: a string or an embedded message."""
    data = value.encode() if isinstance(value, str) else value
    assert len(data) < 128
    return bytes([field << 3 | 2, len(data)]) + data


# The requests that name a tenant, used to compare "no grant on this tenant"
# with "no such tenant": (service, method, proto, JSON body, protobuf body).
def tenant_requests(tenant):
    scope = pb_string(1, pb_string(1, tenant))
    return [
        ("nopacs.v1.TenantService", "GetTenant", "nopacs/v1/tenant.proto", {"name": tenant}, pb_string(1, tenant)),
        ("nopacs.v1.SettingsService", "ListSettings", "nopacs/v1/settings.proto", {"scope": {"tenant": tenant}}, scope),
        ("nopacs.v1.AuditService", "ListAuditEntries", "nopacs/v1/audit.proto", {"scope": {"tenant": tenant}}, scope),
        ("nopacs.v1.StorageService", "ListStorages", "nopacs/v1/storage.proto", {"tenant": tenant}, pb_string(1, tenant)),
        ("nopacs.v1.ReceiverService", "ListReceivers", "nopacs/v1/receiver.proto", {"tenant": tenant}, pb_string(1, tenant)),
        ("nopacs.v1.ReceptionService", "ListReceptions", "nopacs/v1/reception.proto", {"tenant": tenant}, pb_string(1, tenant)),
        ("nopacs.v1.QuarantineService", "ListQuarantinedFiles", "nopacs/v1/quarantine.proto", {"tenant": tenant}, pb_string(1, tenant)),
    ]


# ---------------------------------------------------------------------------
# The three clients. Each call returns (code, message, normalized answer,
# request id); the normalized answer is everything the client received
# except the request id and the date, so two refusals can be compared byte for
# byte.


class Clients:
    def __init__(self, work, args):
        self.work = work
        self.args = args
        self.proto = args.proto
        with open(args.ca, "rb") as f:
            ca = f.read()
        import grpc  # imported here: the audit, tui and json steps do not need it

        self.grpc = grpc
        self.channel = grpc.secure_channel(args.addr, grpc.ssl_channel_credentials(root_certificates=ca))
        self.headers = os.path.join(work.path, "auth.headers")

    def close(self):
        self.channel.close()

    def grpcurl(self, service, method, proto, body, key):
        # The key reaches grpcurl through its environment and -expand-headers,
        # never as an argument, which every user of the machine could read.
        env = dict(os.environ)
        cmd = [self.args.grpcurl, "-import-path", self.proto, "-import-path", STANDIN_PROTO, "-proto", proto, "-cacert", self.args.ca, "-v"]
        if key is not None:
            env["NOPACS_PROOF_AUTH"] = key
            cmd += ["-expand-headers", "-H", "authorization: ${NOPACS_PROOF_AUTH}"]
        cmd += ["-d", json.dumps(body), self.args.addr, "%s/%s" % (service, method)]
        p = subprocess.run(cmd, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)
        out = p.stdout.decode("utf-8", "replace")
        # The method's description, which grpcurl prints from the proto file,
        # comes before what the server sent; only the latter is compared.
        start = out.find("Response headers received:")
        answer = out[start:] if start >= 0 else out
        rid = ""
        kept = []
        for line in answer.split("\n"):
            low = line.lower()
            if low.startswith("nopacs-request-id:"):
                rid = line.split(":", 1)[1].strip()
                continue
            if low.startswith("date:"):
                continue
            kept.append(line)
        m = re.search(r"Code: (\w+)\s+Message: ([^\n]*)", answer)
        if start < 0 and p.returncode != 0 and not m:
            # grpcurl refused to build the request: nothing reached the server.
            return "NOTSENT", out.strip()[:200], "not sent\n" + out, ""
        code = m.group(1) if m else ("OK" if p.returncode == 0 else "exit %d" % p.returncode)
        message = m.group(2).strip() if m else ""
        norm = "exit=%d\n%s" % (p.returncode, "\n".join(kept))
        return code, message, norm, rid

    def python(self, service, method, raw, key):
        grpc = self.grpc
        call = self.channel.unary_unary("/%s/%s" % (service, method), request_serializer=None, response_deserializer=None)
        md = [("authorization", key)] if key is not None else []
        answer = b""
        try:
            answer, rpc = call.with_call(raw, metadata=md, timeout=30)
            code, message, init, trail = grpc.StatusCode.OK, "", rpc.initial_metadata(), rpc.trailing_metadata()
        except grpc.RpcError as e:
            code, message, init, trail = e.code(), e.details() or "", e.initial_metadata() or (), e.trailing_metadata() or ()
        rid = ""
        for k, v in list(init) + list(trail):
            if k == "nopacs-request-id":
                rid = v
        keep = lambda m: sorted((k, v) for k, v in m if k not in ("nopacs-request-id", "date"))
        norm = "%s|%s|headers=%r|trailers=%r|body=%r" % (code.name, message, keep(init), keep(trail), answer)
        return code.name, message, norm, rid

    def python_stream(self, service, method, key):
        """A server-streaming call by Python, read to its end: the code and
        message it ended with, and the request id."""
        grpc = self.grpc
        call = self.channel.unary_stream("/%s/%s" % (service, method), request_serializer=None, response_deserializer=None)
        md = [("authorization", key)] if key is not None else []
        stream = call(b"", metadata=md, timeout=30)
        code, message = grpc.StatusCode.OK, ""
        try:
            for _ in stream:
                break
            stream.cancel()
        except grpc.RpcError as e:
            code, message = e.code(), e.details() or ""
        rid = ""
        try:
            for k, v in list(stream.initial_metadata() or ()) + list(stream.trailing_metadata() or ()):
                if k == "nopacs-request-id":
                    rid = v
        except grpc.RpcError:
            pass
        return code.name, message, rid

    def curl(self, path, body, key):
        # The key goes in a header file only the script's user can read.
        hdr = []
        if key is not None:
            fd = os.open(self.headers, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(fd, "w") as f:
                f.write("Authorization: %s\n" % key)
            hdr = ["-H", "@" + self.headers]
        out_body = os.path.join(self.work.path, "curl.body")
        out_head = os.path.join(self.work.path, "curl.head")
        cmd = ["curl", "-sS", "--cacert", self.args.ca, "-o", out_body, "-D", out_head, "-w", "%{http_code}"] + hdr
        url = "https://%s%s" % (self.args.addr, path)
        if body is not None:
            cmd += ["-H", "Content-Type: application/json", "-d", json.dumps(body)]
        p = subprocess.run(cmd + [url], stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
        if key is not None:
            os.unlink(self.headers)
        status = p.stdout.decode().strip()
        with open(out_body, "rb") as f:
            data = f.read()
        with open(out_head, "rb") as f:
            head = f.read().decode("latin-1")
        rid = ""
        kept = []
        for line in head.split("\r\n"):
            low = line.lower()
            if low.startswith("nopacs-request-id:"):
                rid = line.split(":", 1)[1].strip()
                continue
            if low.startswith("date:"):
                continue
            kept.append(line)
        code, message = status, ""
        try:
            doc = json.loads(data)
            code, message = doc.get("code", status), doc.get("message", "")
        except ValueError:
            pass
        if status == "200":
            code = "ok"
        norm = "%s\n%s\n%s" % (status, "\n".join(kept), data.decode("utf-8", "replace"))
        return code, message, norm, rid


    def curl_raw(self, path, method, ctype, body, key, http1):
        """One request by curl with any HTTP method, content type and raw
        body, over HTTP/1.1 or HTTP/2 (both over TLS), the path sent as it
        is (--path-as-is, so a percent-encoded or doubled slash reaches the
        server unchanged). Returns the status, the answer normalized as in
        curl() (all headers but the date and the request id, sorted, and the
        body), the request id, and the answer's headers."""
        hdr = []
        if key is not None:
            fd = os.open(self.headers, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(fd, "w") as f:
                f.write("Authorization: %s\n" % key)
            hdr = ["-H", "@" + self.headers]
        out_body = os.path.join(self.work.path, "curl.body")
        out_head = os.path.join(self.work.path, "curl.head")
        in_body = os.path.join(self.work.path, "curl.in")
        cmd = ["curl", "-sS", "--path-as-is", "--cacert", self.args.ca, "-o", out_body, "-D", out_head, "-w", "%{http_code}",
               "--http1.1" if http1 else "--http2"] + hdr
        if method == "HEAD":
            cmd += ["--head"]
        else:
            cmd += ["-X", method]
        if ctype:
            cmd += ["-H", "Content-Type: " + ctype]
        if body is not None and method != "HEAD":
            with open(in_body, "wb") as f:
                f.write(body)
            cmd += ["--data-binary", "@" + in_body]
        url = "https://%s%s" % (self.args.addr, path)
        p = subprocess.run(cmd + [url], stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
        if key is not None:
            os.unlink(self.headers)
        status = p.stdout.decode().strip()
        data = b""
        if os.path.exists(out_body):
            with open(out_body, "rb") as f:
                data = f.read()
            os.unlink(out_body)
        if method == "HEAD":
            # A HEAD answer has no body (RFC 9110 9.3.2); curl writes the
            # headers, request id and date included, where the body would go.
            data = b""
        with open(out_head, "rb") as f:
            head = f.read().decode("latin-1")
        rid, kept, headers = "", [], {}
        for line in head.split("\r\n")[1:]:
            if not line:
                continue
            name, _, value = line.partition(":")
            low = name.lower()
            headers[low] = value.strip()
            if low == "nopacs-request-id":
                rid = value.strip()
                continue
            if low == "date":
                continue
            kept.append("%s: %s" % (low, value.strip()))
        kept.sort()
        norm = "%s %s\n%s\n%r" % (head.split("\r\n", 1)[0].split(" ", 1)[0], status, "\n".join(kept), data)
        return status, norm, rid, headers


def key_of(keys, name):
    return keys[name]["key"], keys[name]["id"]


def bad_keys(keys):
    """The six ways of carrying no valid key, with the key id the audit
    entry must hold for each (AuditEntry.key_id: read from the key even when
    it is refused; empty when there is none to read)."""
    unknown_id = "".join(pysecrets.choice(BASE58) for _ in range(12))
    real_secret = keys["nogrant"]["key"].split("_", 2)[2]
    wrong = "".join(pysecrets.choice(BASE58) for _ in range(len(real_secret)))
    unknown = "nopacs_%s_%s" % (unknown_id, "".join(pysecrets.choice(BASE58) for _ in range(len(real_secret))))
    wrong_secret = "nopacs_%s_%s" % (keys["nogrant"]["id"], wrong)
    return [
        ("no key", None, ""),
        ("malformed key", "Bearer not-a-key", ""),
        ("unknown key id", "Bearer " + unknown, unknown_id),
        ("wrong secret", "Bearer " + wrong_secret, keys["nogrant"]["id"]),
        ("expired key", "Bearer " + keys["expired"]["key"], keys["expired"]["id"]),
        ("revoked key", "Bearer " + keys["revoked"]["key"], keys["revoked"]["id"]),
    ]


# STANDIN is the service that stands for "a method not served" once every
# service of the contract is served: defined by a proto file outside the
# contract (scripts/prove/testdata/notserved), so grpcurl can call it by name, and
# served by no server.
STANDIN = ("nopacs.v1.NotServedService", "Nothing", "nopacs/v1/notserved.proto", False)
STANDIN_PROTO = os.path.join(os.path.dirname(os.path.abspath(__file__)), "testdata", "notserved")


def cmd_refusals(work, args):
    every = contract_methods(args.proto)
    names, problems = not_served(args.not_served, every)
    served = [x[:3] for x in every if not listed(x, names)]
    # A server-streaming method is called the streaming way, apart
    # (streaming): called as a unary method by curl it is refused for its
    # content type before any check, which says nothing about the check.
    methods = [x[:3] for x in every if not listed(x, names) and not x[3]]
    streaming = [x[:3] for x in every if not listed(x, names) and x[3]]
    unserved = [x[:3] for x in every if listed(x, names)] or [STANDIN[:3]]
    cl = Clients(work, args)
    try:
        if args.mode == "walk":
            work.result("2.5 the services and methods not served yet: named in api/not-served.txt, each defined by a proto file",
                        not problems,
                        "%d methods in the proto files: %d served, of them %d streaming; %d service(s) or method(s) not served: %s"
                        % (len(every), len(served), len(streaming), len(names),
                           ", ".join(n.split(".")[-1] for n in names) or "none, so a service outside the contract stands for one ("
                           + STANDIN[0] + ")"),
                        problems)
            walk(work, cl, served, unserved)
        else:
            with open(os.path.join(work.path, "keys.json")) as f:
                keys = json.load(f)
            matrix(work, cl, methods, unserved, keys, args)
            streaming_refusals(work, cl, streaming, keys)
    finally:
        cl.close()
    return 1 if work.failed else 0


def streaming_refusals(work, cl, streaming, keys):
    """Section 4, the server-streaming methods, each called the streaming way
    by each client (grpcurl reads the stream; Python with unary_stream; curl
    in the Connect streaming protocol, whose error is the end-of-stream
    message): with no key, unauthenticated; with a key with no grant,
    permission denied; the message of every other method each time."""
    nogrant_key, nogrant_id = key_of(keys, "nogrant")
    ev, n = [], 0
    for svc, m, proto in streaming:
        for label, auth, key_id, code, message in (("no key", None, "", "unauthenticated", UNAUTH_MESSAGE),
                                                   ("no grant", "Bearer " + nogrant_key, nogrant_id, "permission_denied", DENIED_MESSAGE)):
            got = []
            c, msg, norm, rid = cl.grpcurl(svc, m, proto, {}, auth)
            work.ledger(rid, "/%s/%s" % (svc, m), key_id, outcome_of(c), "*", "grpcurl streaming " + label)
            got.append(("grpcurl", c.lower().replace("_", ""), msg, rid))
            c, msg, rid = cl.python_stream(svc, m, auth)
            work.ledger(rid, "/%s/%s" % (svc, m), key_id, outcome_of(c), "*", "python streaming " + label)
            got.append(("python", c.lower().replace("_", ""), msg, rid))
            status, norm, rid, headers = cl.curl_raw("/%s/%s" % (svc, m), "POST", "application/connect+json", b"\0\0\0\0\x02{}", auth, False)
            work.ledger(rid or "-", "/%s/%s" % (svc, m), key_id, outcome_of(code), "*", "curl streaming " + label)
            ok_curl = ('"code":"%s"' % code) in norm and message in norm
            got.append(("curl", code.replace("_", "") if ok_curl else status, message if ok_curl else norm, rid))
            n += 3
            for client, c, msg, rid in got:
                if c != code.replace("_", "") or msg != message or not rid:
                    ev.append("%s %s %s: %s %r request-id=%r" % (m, label, client, c, msg, rid))
    work.result("4.15 a streaming method refuses as every other method does, called the streaming way", bool(streaming) and not ev,
                "%d streaming method(s) x 2 kinds (no key, no grant) x 3 clients: %d requests, Unauthenticated %r and PermissionDenied %r"
                % (len(streaming), n, UNAUTH_MESSAGE, DENIED_MESSAGE), ev)


def walk(work, cl, methods, unserved):
    """Section 2: every served method of the contract with no key, by
    grpcurl, and /metrics by curl: all refused, with one answer; then every
    method not served yet, which must get that very answer."""
    answers, evidence = {}, []
    for svc, m, proto in methods:
        code, msg, norm, rid = cl.grpcurl(svc, m, proto, {}, None)
        work.ledger(rid, "/%s/%s" % (svc, m), "", outcome_of(code), "*", "grpcurl walk")
        answers.setdefault((code, msg, norm), []).append(m)
        if code != "Unauthenticated" or msg != UNAUTH_MESSAGE or not rid:
            evidence.append("%s/%s: %s %r request-id=%r" % (svc, m, code, msg, rid))
    ok = len(answers) == 1 and not evidence
    not_refused = len(evidence)
    if len(answers) > 1:
        evidence += ["answer %d, for %s:\n%s" % (i, ",".join(v), k[2]) for i, (k, v) in enumerate(answers.items())]
    work.result("2.3 no key over TLS: every method refused, one answer (grpcurl)", ok,
                "%d served methods read from the proto files (%d more not served, 2.6), %d distinct answer(s), %d not Unauthenticated %r"
                % (len(methods), len(unserved), len(answers), not_refused, UNAUTH_MESSAGE),
                evidence)
    # A caller without a key must not learn which services exist: a method
    # not served yet is refused with the answer of a served one, byte for
    # byte, and audited as a refusal under the method it asked for.
    served_answers = set(k[2] for k in answers)
    ns, ev = {}, []
    for svc, m, proto in unserved:
        code, msg, norm, rid = cl.grpcurl(svc, m, proto, {}, None)
        work.ledger(rid, "/%s/%s" % (svc, m), "", outcome_of(code), "*", "grpcurl walk not served")
        ns.setdefault(norm, []).append(m)
        if code != "Unauthenticated" or msg != UNAUTH_MESSAGE or not rid:
            ev.append("%s/%s: %s %r request-id=%r" % (svc, m, code, msg, rid))
    same = set(ns) == served_answers and len(served_answers) == 1
    if not same:
        ev += ["not served, answer %d, for %s:\n%s" % (i, ",".join(v), k) for i, (k, v) in enumerate(ns.items())]
    work.result("2.6 no key over TLS: a method not served yet gets the refusal of a served one, byte for byte (grpcurl)",
                same and not ev,
                "%d methods not served: %d distinct answer(s), %s the served methods' answer"
                % (len(unserved), len(ns), "the same as" if same else "not the same as"), ev)
    code, msg, norm, rid = cl.curl("/metrics", None, None)
    work.ledger(rid, "GET /metrics", "", outcome_of(code), "*", "curl walk")
    work.result("2.4 no key over TLS: /metrics refused (curl)", code == "unauthenticated" and msg == UNAUTH_MESSAGE and bool(rid),
                "%s %r" % (code, msg), [norm])
    raw_walk(work, cl, methods, unserved)


# The ways of asking that the raw comparisons send: each content type the
# server's refusal is chosen by, with the body of an empty message in it.
RAW_PROTOCOLS = [
    ("gRPC", "application/grpc", b"\0\0\0\0\0"),
    ("gRPC-Web", "application/grpc-web+proto", b"\0\0\0\0\0"),
    ("Connect streaming", "application/connect+proto", b"\0\0\0\0\0"),
    ("Connect", "application/json", b"{}"),
    ("plain", "", None),
]


def raw_walk(work, cl, methods, unserved):
    """Section 2, by curl with no key: the answer must not depend on what
    the path names, over HTTP/1.1 as over HTTP/2, for a POST and for any
    other HTTP method, in every protocol. For each (HTTP version, HTTP
    method, content type) the same request goes to a served method, a
    method not served yet, a name in no contract and a percent-encoded path
    to the served method; all four answers must be the same bytes but the
    date and the request id, and a refusal. Each is audited as refused, under
    the method asked for, or "/" for a path that is not the name of one."""
    served = "/%s/%s" % methods[0][:2]
    paths = [(served, served), ("/acme.v9.Nothing/Do", "/acme.v9.Nothing/Do"),
             (served.replace("/", "%2F", 2).replace("%2F", "/", 1), "/")]
    if unserved:
        paths.insert(1, ("/%s/%s" % unserved[0][:2], "/%s/%s" % unserved[0][:2]))
    groups, ev, n = {}, [], 0
    for http1 in (True, False):
        for verb in ("POST", "GET", "PUT", "OPTIONS", "HEAD"):
            for pname, ctype, body in RAW_PROTOCOLS:
                if verb != "POST" and pname not in ("gRPC-Web", "Connect", "plain"):
                    continue
                key = ("HTTP/1.1" if http1 else "HTTP/2", verb, pname)
                for path, audited in paths:
                    status, norm, rid, headers = cl.curl_raw(path, verb, ctype, body, None, http1)
                    n += 1
                    work.ledger(rid or "-", audited, "", "refused", "*", "curl raw walk")
                    groups.setdefault(key, {}).setdefault(norm, []).append(path)
                    # gRPC carries the status in trailers (which curl adds to
                    # the headers it writes), gRPC-Web in its body's trailer
                    # frame, Connect streaming in its end-of-stream message.
                    text = norm.lower()
                    # A HEAD answer has no body, so gRPC-Web's carries no
                    # status: WWW-Authenticate and the comparison stand for it.
                    refused = status == "401" or "grpc-status: 16" in text or verb == "HEAD" or (
                        pname == "Connect streaming" and '"code":"unauthenticated"' in text)
                    if not rid or not refused or "bearer" not in headers.get("www-authenticate", "").lower():
                        ev.append("%s %s %s %s: %s request-id=%r" % (key + (path, status, rid)))
    for key, answers in groups.items():
        if len(answers) != 1:
            ev.append("%s %s %s: %d different answers" % (key + (len(answers),)))
            ev += ["  %r: %s" % (k, ", ".join(v)) for k, v in answers.items()]
    work.result("2.7 no key, HTTP/1.1 and HTTP/2, any HTTP method: served, not served and malformed paths answered alike (curl)",
                not ev,
                "%d requests: %d (HTTP version, method, content type) cases x %d paths (%s), one refusal per case"
                % (n, len(groups), len(paths), ", ".join(p for p, _ in paths)), ev)


def matrix(work, cl, methods, unserved, keys, args):
    """Section 4: each kind of refusal by each client, compared; then the
    methods not served yet, with no or a bad key and with a valid one."""
    bad = bad_keys(keys)
    nogrant_key, nogrant_id = key_of(keys, "nogrant")
    reader_key, reader_id = key_of(keys, "reader")
    admin_key, admin_id = key_of(keys, "admin")
    clients = ("grpcurl", "python", "curl")
    seen = {c: {} for c in clients}  # client -> kind -> {norm: [labels]}
    agree = {}  # (kind) -> {(client): (code, message)}
    total = 0

    def send(client, svc, m, proto, jbody, raw, auth, key_id, kind, tenant="*"):
        nonlocal total
        total += 1
        if client == "grpcurl":
            code, msg, norm, rid = cl.grpcurl(svc, m, proto, jbody, auth)
        elif client == "python":
            code, msg, norm, rid = cl.python(svc, m, raw, auth)
        else:
            code, msg, norm, rid = cl.curl("/%s/%s" % (svc, m), jbody, auth)
        if code != "NOTSENT":
            work.ledger(rid, "/%s/%s" % (svc, m), key_id, outcome_of(code), tenant, client + " " + kind)
        seen[client].setdefault(kind, {}).setdefault(norm, []).append("%s %s" % (m, kind))
        agree.setdefault(kind, {}).setdefault(client, set()).add((code.lower().replace("_", ""), msg))
        return code, msg, rid

    for client in clients:
        for svc, m, proto in methods:
            for label, auth, key_id in bad:
                send(client, svc, m, proto, {}, b"", auth, key_id, "unauthenticated")
            # A key with no grant is answered by WhoAmI, which any
            # authenticated caller may ask, ListTenants, which lists the
            # tenants the key holds a grant on (none), and every other method
            # whose Role line says "none", read from the proto files.
            if "%s/%s" % (svc, m) in OPEN_METHODS and m != "WhoAmI":
                send(client, svc, m, proto, {}, b"", "Bearer " + nogrant_key, nogrant_id, "open-nogrant")
            elif m == "WhoAmI":
                send(client, svc, m, proto, {}, b"", "Bearer " + nogrant_key, nogrant_id, "whoami-nogrant")
            elif m == "ListTenants":
                send(client, svc, m, proto, {}, b"", "Bearer " + nogrant_key, nogrant_id, "list-nogrant")
            else:
                send(client, svc, m, proto, {}, b"", "Bearer " + nogrant_key, nogrant_id, "permission denied")
        if client == "curl":
            for label, auth, key_id in bad:
                code, msg, norm, rid = cl.curl("/metrics", None, auth)
                total += 1
                work.ledger(rid, "GET /metrics", key_id, outcome_of(code), "*", "curl metrics " + label)
                seen[client].setdefault("metrics", {}).setdefault(norm, []).append("metrics " + label)
        for svc, m, proto, jbody, raw in tenant_requests(args.pg_tenant):
            send(client, svc, m, proto, jbody, raw, "Bearer " + reader_key, reader_id, "tenant-denied", args.pg_tenant)
        for svc, m, proto, jbody, raw in tenant_requests(args.missing_tenant):
            send(client, svc, m, proto, jbody, raw, "Bearer " + reader_key, reader_id, "tenant-denied", args.missing_tenant)
        svc, m, proto, jbody, raw = tenant_requests(args.missing_tenant)[0]
        send(client, svc, m, proto, jbody, raw, "Bearer " + admin_key, admin_id, "not-found", args.missing_tenant)

    # The methods not served yet. With no key or a bad one, the refusal of a
    # served method, audited as refused. With a valid key, UNIMPLEMENTED and
    # nothing done, audited as failed, with the detail that the method is not
    # served. Both in the installation's log, under the method asked for.
    before = db_state(args.data_dir, args.lite_tenant)
    for client in clients:
        for svc, m, proto in unserved:
            for label, auth, key_id in bad:
                send(client, svc, m, proto, {}, b"", auth, key_id, "ns-unauthenticated")
            jbody, raw = ns_body(svc, m, args.lite_tenant)
            for name in ("admin", "nogrant"):
                k, kid = key_of(keys, name)
                send(client, svc, m, proto, jbody, raw, "Bearer " + k, kid, "ns-unimplemented")
    after = db_state(args.data_dir, args.lite_tenant)

    want = {"unauthenticated": UNAUTH_MESSAGE, "permission denied": DENIED_MESSAGE, "tenant-denied": DENIED_MESSAGE,
            "not-found": NOT_FOUND_MESSAGE, "whoami-nogrant": "", "list-nogrant": "", "open-nogrant": "",
            "ns-unauthenticated": UNAUTH_MESSAGE, "ns-unimplemented": NO_SERVICE_MESSAGE}
    want_code = {"unauthenticated": "unauthenticated", "permission denied": "permissiondenied", "tenant-denied": "permissiondenied",
                 "not-found": "notfound", "whoami-nogrant": "ok", "list-nogrant": "ok", "open-nogrant": "ok",
                 "ns-unauthenticated": "unauthenticated", "ns-unimplemented": "unimplemented"}
    n_methods = len(methods)
    n_open = len([1 for svc, m, _ in methods if "%s/%s" % (svc, m) in OPEN_METHODS and m != "WhoAmI"])
    names = {"grpcurl": "4.1", "python": "4.2", "curl": "4.3"}
    for client in clients:
        kinds = seen[client]
        u = kinds.get("unauthenticated", {})
        codes = agree["unauthenticated"][client]
        ok = len(u) == 1 and codes == {(want_code["unauthenticated"], UNAUTH_MESSAGE)}
        detail = "%d served methods x %d kinds (%s): %d distinct answer(s)" % (n_methods, len(bad), ", ".join(b[0] for b in bad), len(u))
        if client == "curl":
            mt = kinds.get("metrics", {})
            ok = ok and len(mt) == 1
            detail += "; /metrics x %d kinds: %d distinct" % (len(bad), len(mt))
        work.result("%s unauthenticated, one answer (%s)" % (names[client], client), ok, detail,
                    ["%r: %s" % (k, ", ".join(v[:4])) for k, v in list(u.items())[:6]] + ["codes %r" % codes])

    def kind_check(name, kind, detail, extra_ok=True):
        evidence, ok = [], extra_ok
        for client in clients:
            group = seen[client].get(kind, {})
            codes = agree.get(kind, {}).get(client, set())
            if len(group) != 1 or codes != {(want_code[kind], want[kind])}:
                ok = False
                evidence.append("%s: %d distinct answers, codes %r" % (client, len(group), codes))
                evidence += ["  %r: %s" % (k, ", ".join(v[:6])) for k, v in group.items()]
        work.result(name, ok, detail, evidence)

    kind_check("4.4 a key with no grant: one permission-denied answer", "permission denied",
               "%d served methods x 3 clients, one answer per client: PermissionDenied %r" % (n_methods - 2 - n_open, DENIED_MESSAGE))
    open_ok = n_open > 0
    open_ev = []
    for client in clients:
        codes = agree.get("open-nogrant", {}).get(client, set())
        if codes != {("ok", "")}:
            open_ok = False
            open_ev.append("%s: codes %r" % (client, codes))
    work.result("4.16 a key with no grant may call the methods open to any authenticated caller", open_ok,
                "%d methods whose Role line is none (%s) answered OK by 3 clients"
                % (n_open, ", ".join(sorted(k.split("/")[1] for k in OPEN_METHODS if not k.endswith("/WhoAmI")))), open_ev)
    kind_check("4.5 a key with no grant may still ask who it is", "whoami-nogrant", "WhoAmI answered OK to the key with no grant, 3 clients")
    # The list must hold no tenant: an empty message, whatever the client.
    empty = {"grpcurl": "Response contents:\n{}\n", "python": "|body=b''", "curl": "\n{}"}
    listed = [c + ": " + n for c in clients for n in seen[c].get("list-nogrant", {}) if empty[c] not in n]
    kind_check("4.6 a key with no grant lists no tenant", "list-nogrant",
               "ListTenants answered OK with no tenant (2 exist), 3 clients", extra_ok=not listed)
    if listed:
        print("     | " + "\n     | ".join(listed))
    kind_check("4.7 no grant on a tenant = no such tenant, byte for byte", "tenant-denied",
               "reader of one tenant asks for %s (exists) and %s (does not): %s x 3 clients, one answer per client"
               % (args.pg_tenant, args.missing_tenant, ", ".join(r[1] for r in tenant_requests(args.missing_tenant))))
    # The denial for a tenant must also be the very answer of "no grant" on
    # the same method (GetTenant), not merely the same code.
    same = True
    ev = []
    for client in clients:
        a = set(seen[client].get("tenant-denied", {}))
        b = set()
        for norm, labels in seen[client].get("permission denied", {}).items():
            if any(l.startswith("GetTenant ") for l in labels):
                b.add(norm)
        if not a or a != b:
            same = False
            ev.append("%s: tenant denial %r, no-grant GetTenant %r" % (client, a, b))
    work.result("4.8 the tenant denial is the no-grant answer itself", same, "GetTenant compared within each client", ev)
    kind_check("4.9 a caller who may list tenants is told not found", "not-found",
               "admin of the installation asks for %s: NotFound %r, 3 clients" % (args.missing_tenant, NOT_FOUND_MESSAGE))
    # The methods not served yet, each client against its own answers to the
    # served methods: a caller without a valid key gets the same bytes.
    ev = []
    for client in clients:
        a = set(seen[client].get("ns-unauthenticated", {}))
        b = set(seen[client].get("unauthenticated", {}))
        if a != b or len(b) != 1:
            ev.append("%s: %d answer(s) to the methods not served, %d to the served ones, %s"
                      % (client, len(a), len(b), "the same" if a == b else "different"))
            ev += ["  not served %r: %s" % (k, ", ".join(v[:4])) for k, v in seen[client].get("ns-unauthenticated", {}).items() if k not in b]
    work.result("4.11 no or a bad key: a method not served yet gets the refusal of a served one, byte for byte", not ev,
                "%d methods not served x %d kinds x 3 clients, compared with each client's answer to the %d served methods"
                % (len(unserved), len(bad), n_methods), ev)
    # With a valid key: UNIMPLEMENTED, one answer byte for byte within each
    # client, whatever the method and the key.
    kind_check("4.12 a valid key: a method not served yet is UNIMPLEMENTED, one answer", "ns-unimplemented",
               "%d methods not served x 2 keys (admin of the installation, no grant) x 3 clients, one answer per client: Unimplemented %r"
               % (len(unserved), NO_SERVICE_MESSAGE), extra_ok=bool(unserved))
    # With a valid key over HTTP/1.1 and HTTP/2, by curl in each protocol: a
    # method not served yet is UNIMPLEMENTED, never an HTTP status of the
    # transport (505), and audited as failed with served=false.
    ev, n = [], 0
    if unserved:
        k, kid = key_of(keys, "nogrant")
        path = "/%s/%s" % unserved[0][:2]
        for http1 in (True, False):
            for pname, ctype, body in RAW_PROTOCOLS[:4]:
                status, norm, rid, headers = cl.curl_raw(path, "POST", ctype, body, "Bearer " + k, http1)
                n += 1
                work.ledger(rid or "-", path, kid, "failed", "*", "curl raw not served")
                text = norm.lower()
                ok = ("grpc-status: 12" in text and "grpc-message: " + NO_SERVICE_MESSAGE in text) or \
                     ('"code":"unimplemented"' in text and NO_SERVICE_MESSAGE in text)
                if not ok or not rid:
                    ev.append("%s %s: %s %s" % ("HTTP/1.1" if http1 else "HTTP/2", pname, status, norm))
    work.result("4.14 a valid key, HTTP/1.1 and HTTP/2: a method not served yet is UNIMPLEMENTED in every protocol (curl)",
                bool(unserved) and not ev, "%d requests: gRPC, gRPC-Web, Connect streaming, Connect x 2 HTTP versions" % n, ev)
    changed = sorted(k for k in set(before) | set(after) if before.get(k) != after.get(k))
    work.result("4.13 a method not served yet does nothing", not changed,
                "every row of the master and of tenant %s, read before and after the %d requests with a valid key naming it: %s"
                % (args.lite_tenant, len(unserved) * 2 * 3, "unchanged" if not changed else "changed: " + ", ".join(changed)),
                changed)
    # Across clients: one code and one message for each kind of refusal.
    ev = []
    for kind, per in agree.items():
        allv = set()
        for v in per.values():
            allv |= v
        if len(allv) != 1:
            ev.append("%s: %r" % (kind, per))
    work.result("4.10 the three clients get the same code and message", not ev, "%d requests in all, %d kinds" % (total, len(agree)), ev)


# ---------------------------------------------------------------------------
# Section 8: the audit logs, read through the API, reconciled with the ledger.


def parse_time(s):
    """An RFC 3339 time as the API writes it, to seconds since the epoch."""
    s = s.replace("Z", "+00:00")
    m = re.match(r"(.*T\d\d:\d\d:\d\d)(\.\d+)?(.*)", s)
    frac = (m.group(2) or ".0")[:7]
    t = datetime.datetime.strptime(m.group(1) + frac + m.group(3).replace(":", ""), "%Y-%m-%dT%H:%M:%S.%f%z")
    return t.timestamp()


def socket_post(sock, path, body):
    p = subprocess.run(["curl", "-sS", "--fail-with-body", "--unix-socket", sock, "-H", "Content-Type: application/json",
                        "-D", "-", "-d", json.dumps(body), "http://local" + path], stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
    if p.returncode != 0:
        raise SystemExit("prove: %s answered %s %s" % (path, p.stdout.decode()[-400:], p.stderr.decode()))
    head, _, data = p.stdout.decode().partition("\r\n\r\n")
    rid = ""
    for line in head.split("\r\n"):
        if line.lower().startswith("nopacs-request-id:"):
            rid = line.split(":", 1)[1].strip()
    return json.loads(data), rid


def read_logs(sock, out_path):
    """Every entry of the installation's log and of each tenant's log, through
    the API as the local operator. Returns the entries, each with "log" set,
    and the request ids of the reads made here."""
    reads = []
    tenants, rid = socket_post(sock, "/nopacs.v1.TenantService/ListTenants", {"pageSize": 1000})
    reads.append(("ListTenants", rid))
    entries = []
    scopes = [("installation", {"installation": {}})] + [(t["name"], {"tenant": t["name"]}) for t in tenants.get("tenants", [])]
    for log, scope in scopes:
        token = ""
        while True:
            page, rid = socket_post(sock, "/nopacs.v1.AuditService/ListAuditEntries", {"scope": scope, "pageSize": 1000, "pageToken": token})
            reads.append(("ListAuditEntries", rid))
            for e in page.get("entries", []):
                e["log"] = log
                entries.append(e)
            token = page.get("nextPageToken", "")
            if not token:
                break
    with open(out_path, "w") as f:
        json.dump(entries, f)
    return entries, reads


def cmd_audit(work, args):
    entries, reads = read_logs(args.sock, os.path.join(work.path, "audit.json"))
    with open(os.path.join(work.path, "ledger.tsv")) as f:
        ledger = [line.rstrip("\n").split("\t") for line in f if line.strip()]
    tui = None
    if os.path.exists(os.path.join(work.path, "tui.window")):
        with open(os.path.join(work.path, "tui.window")) as f:
            tui = [float(x) for x in f.read().split()]
    cutoff = args.cutoff

    def system(e):
        m = e.get("method", "")
        return not (m.startswith("/") or m.startswith("GET "))

    def caller(e):
        return "local" if e.get("localUserId") else e.get("keyId", "")

    by_rid = {}
    for e in entries:
        if not system(e) and e.get("requestId"):
            by_rid.setdefault(e["requestId"], []).append(e)
    used = set()
    missing, wrong, misplaced, unaudited_ok, not_served = [], [], [], 0, 0
    no_rid = []
    found = found_rid = 0
    for row in ledger:
        rid, method, key_id, outcome, tenant, source = row[:6]
        if outcome == "none":
            # A request the contract does not audit (the health check): it
            # must not be in the log.
            if rid in by_rid:
                wrong.append("%s %s is in the log: %r" % (source, method, by_rid[rid]))
            else:
                unaudited_ok += 1
            continue
        if rid == "-":
            no_rid.append(row)
            continue
        cands = by_rid.get(rid, [])
        if len(cands) != 1:
            missing.append("%s %s request-id %s: %d entries" % (source, method, rid, len(cands)))
            continue
        e = cands[0]
        used.add(id(e))
        found += 1
        found_rid += 1
        problems = []
        if e.get("method") != method:
            problems.append("method %r" % e.get("method"))
        if e.get("outcome", "").replace("AUDIT_OUTCOME_", "").lower() != outcome:
            problems.append("outcome %r, want %s" % (e.get("outcome"), outcome))
        if caller(e) != key_id:
            problems.append("caller %r, want %r" % (caller(e), key_id))
        if tenant != "*" and e.get("tenant", "") != tenant:
            problems.append("tenant %r, want %r" % (e.get("tenant", ""), tenant))
        if problems:
            wrong.append("%s %s %s: %s" % (source, method, rid, "; ".join(problems)))
        # A refusal is always in the installation's log: a caller who was
        # refused must not be able to cause a write in a tenant's database.
        if outcome == "refused" and e["log"] != "installation":
            misplaced.append("%s %s %s refused, in the log of %s" % (source, method, rid, e["log"]))
        # A request to a method not served yet concerns no tenant it could
        # be recorded in: the installation's log, and, when it was allowed,
        # the detail that the method is not served.
        if " ns-" in source or "not served" in source:
            not_served += 1
            if e["log"] != "installation":
                misplaced.append("%s %s %s, in the log of %s" % (source, method, rid, e["log"]))
            if outcome == "failed" and e.get("details", {}).get("served") != "false":
                wrong.append("%s %s %s: details %r, want served=false" % (source, method, rid, e.get("details")))
    # The commands of the command line show a request id only on an error:
    # their requests are matched by what they were and when (to the second,
    # with a second's margin), never with an entry of the interactive
    # program's window, which is accounted for below.
    def in_tui(e):
        return tui is not None and tui[0] <= parse_time(e["time"]) <= tui[1]

    rest = [e for e in entries if id(e) not in used and not system(e) and not in_tui(e)]
    for row in no_rid:
        rid, method, key_id, outcome, tenant, source = row[:6]
        t0, t1 = (float(x) for x in row[6].split()) if len(row) > 6 and row[6] else (0, 1e12)
        match = None
        for e in rest:
            if id(e) in used:
                continue
            if not (t0 - 1 <= parse_time(e["time"]) <= t1 + 1):
                continue
            if (e.get("method") == method and caller(e) == key_id
                    and e.get("outcome", "").replace("AUDIT_OUTCOME_", "").lower() == outcome
                    and (tenant == "*" or e.get("tenant", "") == tenant)):
                match = e
                break
        if match is None:
            missing.append("%s %s (%s, %s): no entry" % (source, method, key_id, outcome))
        else:
            used.add(id(match))
            found += 1
    expected = len([r for r in ledger if r[3] != "none"])
    work.result("8.1 every request the proof made is in the audit log, with its outcome",
                not missing and not wrong and not misplaced,
                "%d of %d found (%d by request id, %d command line requests by what they were; %d to methods not served yet, refused or failed, "
                "in the installation's log); %d health checks not audited, as the contract says"
                % (found, expected, found_rid, found - found_rid, not_served, unaudited_ok),
                missing[:8] + wrong[:8] + misplaced[:8])

    # The other direction: nothing in the logs that the proof did not do.
    started = set()
    for e in entries:
        if id(e) in used and e.get("method") == "/nopacs.v1.TenantService/CreateTenant":
            started.add(e.get("requestId"))
    unexpected, counts = [], {"init": 0, "started": 0, "state": 0, "tui": 0, "after the stop": 0}
    states = []
    tui_lists = 0
    # What init writes (the installation's id, its key check) comes before
    # the first request of all: an "init" entry after that is not init's.
    first_request = min([parse_time(e["time"]) for e in entries if not system(e)] or [0])
    for e in entries:
        if id(e) in used:
            continue
        m = e.get("method", "")
        t = parse_time(e["time"])
        if m == INIT_METHOD and t < first_request:
            counts["init"] += 1
        elif m == STARTED_METHOD and e.get("requestId") in started:
            counts["started"] += 1
        elif m == STATE_METHOD:
            counts["state"] += 1
            d = e.get("details", {})
            states.append("%s %s>%s" % (e.get("tenant"), d.get("old_state", "?"), d.get("new_state", "?")))
        elif system(e):
            unexpected.append(e)
        elif t > cutoff and caller(e) == "local" and m.rsplit("/", 1)[-1] in ("ListAuditEntries", "ListTenants"):
            counts["after the stop"] += 1
        elif tui and tui[0] <= t <= tui[1] and caller(e) == "local" and m.rsplit("/", 1)[-1].startswith(READ_PREFIXES) \
                and e.get("outcome") == "AUDIT_OUTCOME_DONE":
            counts["tui"] += 1
            tui_lists += m.endswith("/ListTenants")
        else:
            unexpected.append(e)
    ok = not unexpected and counts["init"] >= 1 and (tui is None or tui_lists > 0)
    ev = [json.dumps({k: v for k, v in e.items() if k != "id"}) for e in unexpected[:10]]
    if counts["init"] < 1:
        ev.append("no entry written by init")
    if tui is not None and not tui_lists:
        ev.append("no ListTenants by the interactive program in its window")
    work.result("8.2 nothing in the audit log that the proof did not do", ok,
                "%d entries in %d log(s); beside the proof's requests: %d by init, %d creation-started, %d state changes (%s), %d by the interactive program (%d ListTenants), %d reads after the stop; %d unexpected"
                % (len(entries), len(set(e["log"] for e in entries)), counts["init"], counts["started"], counts["state"],
                   ", ".join(states) or "none", counts["tui"], tui_lists, counts["after the stop"], len(unexpected)), ev)
    return 1 if work.failed else 0


# ---------------------------------------------------------------------------
# Section 10.


def cmd_tui(work, args):
    here = os.path.dirname(os.path.abspath(__file__))
    # No __pycache__ left in the checkout by the import below.
    sys.dont_write_bytecode = True
    spec = importlib.util.spec_from_file_location("check_tui", os.path.join(here, "..", "check", "tui.py"))
    tui = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(tui)
    for var in ("NOPACS_SERVER", "NOPACS_KEY", "NOPACS_CA_FILE"):
        os.environ.pop(var, None)
    start = datetime.datetime.now().timestamp()
    s = tui.Session(args.binary, args.data_dir, cols=120, rows=40)
    seen = {"local operator": s.wait_for("local operator")}
    s.press("1")
    for name in args.tenant:
        seen[name] = s.wait_for(name)
    s.press("q")
    code = s.finish()
    end = datetime.datetime.now().timestamp()
    with open(os.path.join(work.path, "tui.window"), "w") as f:
        # The server and this script read the same clock, so the window is
        # exact; run.sh leaves two seconds before it free of other
        # requests, as the command line's are matched to the second.
        f.write("%f %f\n" % (start, end))
    restored = s.restored()
    ok = all(seen.values()) and code == 0 and restored
    work.result("10.2 the interactive program lists the tenants and quits cleanly", ok,
                "under a pseudo-terminal, 120x40: header %s, %s listed, q exits %r, terminal restored %s"
                % ("shown" if seen["local operator"] else "MISSING", " and ".join("%s %s" % (n, "yes" if seen[n] else "NO") for n in args.tenant), code, restored),
                [s.screen.text()])
    return 1 if work.failed else 0


def cmd_json(work, args):
    evidence, n = [], 0
    for spec in args.doc:
        path, field = spec.rsplit(":", 1)
        n += 1
        try:
            with open(path) as f:
                doc = json.load(f)
        except ValueError as e:
            evidence.append("%s: does not parse: %s" % (os.path.basename(path), e))
            continue
        if doc.get("schemaVersion") != 1 or not isinstance(doc.get(field), list):
            evidence.append("%s: schemaVersion %r, %s %r" % (os.path.basename(path), doc.get("schemaVersion"), field, type(doc.get(field)).__name__))
    work.result("10.1 every list command's --json parses", not evidence,
                "%d documents parsed by Python's json, each with schemaVersion 1 and its list" % n, evidence)
    return 1 if work.failed else 0


def cmd_metrics(work, args):
    from prometheus_client.parser import text_string_to_metric_families

    with open(args.file) as f:
        families = {fam.name: fam for fam in text_string_to_metric_families(f.read())}
    want_zero = ["nopacs_audit_entries_dropped_refusals", "nopacs_audit_entries_dropped_records",
                 "nopacs_audit_entries_dropped_writes", "nopacs_audit_entries_failed", "nopacs_audit_entries_notices"]
    values, evidence = {}, []
    for name in want_zero + ["nopacs_audit_entries_written"]:
        fam = families.get(name)
        if fam is None or not fam.samples:
            evidence.append("no family " + name)
            continue
        values[name] = sum(s.value for s in fam.samples)
    for name in want_zero:
        if values.get(name, 0) != 0:
            evidence.append("%s = %g" % (name, values[name]))
    written = values.get("nopacs_audit_entries_written", 0)
    if written <= 0:
        evidence.append("no entry written by the batching writer")
    work.result("8.3 the server's metrics count no dropped or failed audit entry", not evidence,
                "/metrics parsed by prometheus_client: written %d, dropped 0+0+0, failed 0, notices 0" % written
                if not evidence else "see evidence", evidence)
    return 1 if work.failed else 0


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--work", required=True)
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("refusals")
    r.add_argument("--mode", choices=["walk", "matrix"], required=True)
    r.add_argument("--proto", required=True)
    r.add_argument("--addr", required=True)
    r.add_argument("--ca", required=True)
    r.add_argument("--grpcurl", required=True)
    r.add_argument("--pg-tenant", default="")
    r.add_argument("--missing-tenant", default="")
    r.add_argument("--not-served", required=True, help="the list of services not served yet, api/not-served.txt")
    r.add_argument("--data-dir", default="", help="the main server's data directory, read by 4.13")
    r.add_argument("--lite-tenant", default="", help="a tenant on SQLite, named by the requests of 4.13")
    a = sub.add_parser("audit")
    a.add_argument("--sock", required=True)
    a.add_argument("--cutoff", type=float, required=True)
    t = sub.add_parser("tui")
    t.add_argument("--binary", required=True)
    t.add_argument("--data-dir", required=True)
    t.add_argument("--tenant", action="append", default=[])
    j = sub.add_parser("json")
    j.add_argument("doc", nargs="+", help="PATH:FIELD, FIELD naming the list the document must hold")
    m = sub.add_parser("metrics")
    m.add_argument("--file", required=True)
    args = ap.parse_args()
    work = Work(args.work)
    fn = {"refusals": cmd_refusals, "audit": cmd_audit, "tui": cmd_tui, "json": cmd_json, "metrics": cmd_metrics}[args.cmd]
    sys.exit(fn(work, args))


if __name__ == "__main__":
    main()
