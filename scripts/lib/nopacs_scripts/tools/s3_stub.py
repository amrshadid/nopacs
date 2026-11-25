"""An archive tier for the proofs: a proxy in front of a real S3 server that
makes chosen keys behave as S3 does for an object in an archive storage class
(GLACIER, DEEP_ARCHIVE), and a control port to drive it.

    s3_stub.py --upstream PORT [--port PORT] [--control-port PORT]
               [--restore-delay SECONDS] [--archive /bucket/key ...]
    s3_stub.py --self-test

Everything else is passed to the S3 server as it came, the Host header kept so
the request's signature still holds, so the server's own behavior (versions,
object lock, listings, errors) is what the caller meets. For a key marked
archived the proxy answers as S3 does (docs.aws.amazon.com/AmazonS3/latest/API):

  GET      403 InvalidObjectState (with the StorageClass), until a restore is
           complete and until its expiry; then the bytes.
  HEAD     the server's answer with x-amz-storage-class (GLACIER or
           DEEP_ARCHIVE) and, once a restore was requested, x-amz-restore:
           ongoing-request="true" while it runs, then
           ongoing-request="false", expiry-date="<HTTP date>".
  POST ?restore (RestoreObject)
           202 when a restore starts; 200 when the object is already restored
           (its expiry moves); 409 RestoreAlreadyInProgress while one runs.
           A key that is not marked archived gets the S3 server's own answer
           (InvalidObjectState for an ordinary object, 404 for a missing
           one). The body's Days says how long the
           restored copy stays (a Days is DAY_SECONDS long, so a proof can make
           it short).

A restore completes after the configured delay, or when the control port is
told to complete it. The control port is a separate loopback port, so an S3
client never reaches it. It answers JSON:

  POST /archive   {"key": "/bucket/key", "class": "GLACIER"}  mark archived
  POST /standard  {"key": ...}                                 mark standard again
  POST /complete  {"key": ...}                                 finish a running restore now
  POST /expire    {"key": ...}                                 end a restored copy's stay now
  POST /reset                                                  forget every mark and event
  GET  /state                                                  {key: {class, restore, ...}}
  GET  /events[?op=GetObject&key=/bucket/key]                  what happened, oldest first

The events are RestoreObject (every request and its status), GetObjectRefused
(a GET answered InvalidObjectState), and the control calls. Used in process,
ArchiveProxy has the same operations as methods. Python's standard library
only; nothing of NoPACs' is used.

Exit status of the command: 0 after a stop with Ctrl-C; 2 for a mistake in the
command line.
"""

import argparse
import email.utils
import http.client
import json
import threading
import time
import xml.etree.ElementTree as ET
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, unquote, urlsplit

ARCHIVE_CLASSES = ("GLACIER", "DEEP_ARCHIVE", "GLACIER_IR")
DAY_SECONDS = 86400
HOP = {"connection", "keep-alive", "transfer-encoding", "te", "trailer", "upgrade", "proxy-connection"}


def s3_error(code, message, extra=""):
    """The XML body S3 puts on an error."""
    return ('<?xml version="1.0" encoding="UTF-8"?>\n<Error><Code>%s</Code><Message>%s</Message>%s</Error>' % (code, message, extra)).encode()


def norm(key):
    """A key as the proxy names it: "/bucket/key", unquoted."""
    key = unquote(key.split("?")[0])
    return key if key.startswith("/") else "/" + key


class ArchiveProxy:
    """The proxy and its control port, on loopback ports given (0 picks one).
    upstream_port is the S3 server's. restore_delay is the seconds a restore
    takes (None: it completes only when complete_restore is called);
    day_seconds is how long a Days of 1 lasts; clock is a function returning
    seconds, replaceable so a self-test moves time without waiting."""

    def __init__(self, upstream_port, port=0, control_port=0, restore_delay=None, day_seconds=DAY_SECONDS, clock=time.time):
        self.upstream_port = upstream_port
        self.restore_delay = restore_delay
        self.day_seconds = day_seconds
        self.clock = clock
        self.lock = threading.Lock()
        self.marks = {}
        self.log = []
        self.server = ThreadingHTTPServer(("127.0.0.1", port), self.s3_handler())
        self.control = ThreadingHTTPServer(("127.0.0.1", control_port), self.control_handler())
        self.port = self.server.server_address[1]
        self.control_port = self.control.server_address[1]
        for s in (self.server, self.control):
            threading.Thread(target=s.serve_forever, daemon=True).start()

    # ---- what a proof calls ----

    @property
    def endpoint(self):
        """The S3 endpoint to give the client."""
        return "http://127.0.0.1:%d" % self.port

    @property
    def refused(self):
        """How many GETs were answered InvalidObjectState."""
        return self.count("GetObjectRefused")

    def stop(self):
        """Ends both servers."""
        for s in (self.server, self.control):
            s.shutdown()
            s.server_close()

    def archive(self, key, storage_class="GLACIER"):
        """Marks key ("/bucket/key") as held in an archive class."""
        if storage_class not in ARCHIVE_CLASSES:
            raise ValueError("not an archive storage class: %s" % storage_class)
        with self.lock:
            self.marks[norm(key)] = {"class": storage_class, "restore": None}
            self.note("Archive", norm(key), 200)

    def standard(self, key):
        """Makes key an ordinary object again."""
        with self.lock:
            self.marks.pop(norm(key), None)
            self.note("Standard", norm(key), 200)

    def complete_restore(self, key):
        """Finishes the restore running on key now; False if none runs."""
        with self.lock:
            m = self.marks.get(norm(key))
            ok = bool(m and m["restore"] and not m["restore"]["done"])
            if ok:
                self.finish(m["restore"], self.clock())
            self.note("Complete", norm(key), 200 if ok else 404)
            return ok

    def expire_restore(self, key):
        """Ends the stay of a restored copy now; False if none was there."""
        with self.lock:
            m = self.marks.get(norm(key))
            ok = bool(m and m["restore"] and m["restore"]["done"])
            if ok:
                m["restore"] = None
            self.note("Expire", norm(key), 200 if ok else 404)
            return ok

    def reset(self):
        """Forgets every mark and event."""
        with self.lock:
            self.marks.clear()
            self.log.clear()

    def events(self, op=None, key=None):
        """The events so far, oldest first, as dicts with time, op, key, status."""
        with self.lock:
            return [dict(e) for e in self.log if (op is None or e["op"] == op) and (key is None or e["key"] == norm(key))]

    def count(self, op, key=None, status=None):
        """How many events of op (for key, with status) there are."""
        return len([e for e in self.events(op, key) if status is None or e["status"] == status])

    def state(self):
        """{key: {class, restore: None or {done, expires}}} as now."""
        with self.lock:
            out = {}
            for k, m in self.marks.items():
                self.settle(m)
                r = m["restore"]
                out[k] = {"class": m["class"], "restore": None if r is None else {"done": r["done"], "expires": r["expires"]}}
            return out

    # ---- the model; the lock is held ----

    def note(self, op, key, status):
        self.log.append({"time": self.clock(), "op": op, "key": key, "status": status})

    def finish(self, r, now):
        r["done"] = True
        r["expires"] = now + r["days"] * self.day_seconds

    def settle(self, m):
        """Moves a mark's restore along with the clock: a due one completes, an expired one ends."""
        r = m["restore"]
        if r is None:
            return
        now = self.clock()
        if not r["done"] and r["ready_at"] is not None and now >= r["ready_at"]:
            self.finish(r, r["ready_at"])
        if r["done"] and now >= r["expires"]:
            m["restore"] = None

    def restore_header(self, m):
        r = m["restore"]
        if r is None:
            return None
        if not r["done"]:
            return 'ongoing-request="true"'
        return 'ongoing-request="false", expiry-date="%s"' % email.utils.formatdate(r["expires"], usegmt=True)

    def request_restore(self, key, days):
        """The status and body of a RestoreObject on a key that exists."""
        m = self.marks.get(key)
        if m is None:
            return 403, s3_error("InvalidObjectState", "Restore is not allowed for the object's current storage class")
        self.settle(m)
        r = m["restore"]
        if r is None:
            now = self.clock()
            m["restore"] = {"done": False, "days": days, "expires": None,
                            "ready_at": None if self.restore_delay is None else now + self.restore_delay}
            if self.restore_delay == 0:
                self.finish(m["restore"], now)
            return 202, b""
        if r["done"]:
            r["days"], r["expires"] = days, self.clock() + days * self.day_seconds
            return 200, b""
        return 409, s3_error("RestoreAlreadyInProgress", "Object restore is already in progress")

    # ---- the S3 side ----

    def s3_handler(self):
        proxy = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def body(self):
                if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
                    out = b""
                    while True:
                        n = int(self.rfile.readline().split(b";")[0].strip() or b"0", 16)
                        if n == 0:
                            self.rfile.readline()
                            return out
                        out += self.rfile.read(n)
                        self.rfile.readline()
                n = int(self.headers.get("Content-Length", 0) or 0)
                return self.rfile.read(n) if n else b""

            def answer(self, status, body, content_type="application/xml", extra=()):
                self.send_response(status)
                if body:
                    self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(body)))
                for k, v in extra:
                    self.send_header(k, v)
                self.end_headers()
                if body and self.command != "HEAD":
                    self.wfile.write(body)

            def upstream(self, data):
                c = http.client.HTTPConnection("127.0.0.1", proxy.upstream_port, timeout=60)
                headers = {k: v for k, v in self.headers.items() if k.lower() not in HOP}
                if "chunked" in self.headers.get("Transfer-Encoding", "").lower():
                    headers["Content-Length"] = str(len(data))
                try:
                    c.request(self.command, self.path, body=data if data or self.command in ("PUT", "POST") else None, headers=headers)
                    r = c.getresponse()
                    return r.status, r.getheaders(), r.read()
                finally:
                    c.close()

            def relay(self, extra=(), skip=()):
                """Passes the request on and the answer back; the answer's headers
                named in skip are dropped, those in extra added."""
                data = self.body()
                return self.send_upstream(self.upstream(data), extra, skip)

            def send_upstream(self, got, extra=(), skip=()):
                status, headers, answer = got
                self.send_response(status)
                for k, v in headers:
                    if k.lower() not in HOP and k.lower() != "content-length" and k.lower() not in skip:
                        self.send_header(k, v)
                if self.command == "HEAD":
                    length = dict((k.lower(), v) for k, v in headers).get("content-length", "0")
                else:
                    length = str(len(answer))
                self.send_header("Content-Length", length)
                for k, v in extra:
                    self.send_header(k, v)
                self.end_headers()
                if self.command != "HEAD":
                    self.wfile.write(answer)

            def mark_headers(self, m):
                extra = [("x-amz-storage-class", m["class"])]
                h = proxy.restore_header(m)
                if h:
                    extra.append(("x-amz-restore", h))
                return extra

            def do_GET(self):
                key = norm(self.path)
                with proxy.lock:
                    m = proxy.marks.get(key)
                    if m is not None:
                        proxy.settle(m)
                    if m is not None and (m["restore"] is None or not m["restore"]["done"]):
                        proxy.note("GetObjectRefused", key, 403)
                        refused = m["class"]
                    else:
                        refused = None
                        extra = self.mark_headers(m) if m is not None else ()
                self.body()
                if refused:
                    self.answer(403, s3_error("InvalidObjectState", "The operation is not valid for the object's storage class",
                                              "<StorageClass>%s</StorageClass>" % refused))
                    return
                self.send_upstream(self.upstream(b""), extra, ("x-amz-storage-class", "x-amz-restore"))

            def do_HEAD(self):
                key = norm(self.path)
                with proxy.lock:
                    m = proxy.marks.get(key)
                    if m is not None:
                        proxy.settle(m)
                    extra = self.mark_headers(m) if m is not None else ()
                self.body()
                self.send_upstream(self.upstream(b""), extra, ("x-amz-storage-class", "x-amz-restore"))

            def do_POST(self):
                if "restore" in parse_qs(urlsplit(self.path).query, keep_blank_values=True):
                    key = norm(self.path)
                    with proxy.lock:
                        marked = key in proxy.marks
                    if not marked:
                        # An object that is not archived: the server's own answer (a
                        # missing key is 404, an ordinary object 403 InvalidObjectState).
                        status, headers, answer = self.upstream(self.body())
                        with proxy.lock:
                            proxy.note("RestoreObject", key, status)
                        self.send_upstream((status, headers, answer))
                        return
                    data = self.body()
                    days = 1
                    try:
                        for el in ET.fromstring(data).iter():
                            if el.tag.split("}")[-1] == "Days":
                                days = int(el.text)
                    except (ET.ParseError, TypeError, ValueError):
                        days = 1
                    with proxy.lock:
                        status, body = proxy.request_restore(key, days)
                        proxy.note("RestoreObject", key, status)
                    self.answer(status, body)
                    return
                self.relay()

            def do_PUT(self):
                self.relay()

            def do_DELETE(self):
                self.relay()

        return Handler

    # ---- the control side ----

    def control_handler(self):
        proxy = self

        class Control(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def reply(self, status, obj):
                body = json.dumps(obj).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self):
                u = urlsplit(self.path)
                q = {k: v[0] for k, v in parse_qs(u.query).items()}
                if u.path == "/state":
                    self.reply(200, proxy.state())
                elif u.path == "/events":
                    self.reply(200, proxy.events(q.get("op"), q.get("key")))
                else:
                    self.reply(404, {"error": "no such control path"})

            def do_POST(self):
                n = int(self.headers.get("Content-Length", 0) or 0)
                try:
                    req = json.loads(self.rfile.read(n) or b"{}")
                except ValueError:
                    self.reply(400, {"error": "the body is not JSON"})
                    return
                path = urlsplit(self.path).path
                try:
                    if path == "/archive":
                        proxy.archive(req["key"], req.get("class", "GLACIER"))
                        self.reply(200, {"ok": True})
                    elif path == "/standard":
                        proxy.standard(req["key"])
                        self.reply(200, {"ok": True})
                    elif path == "/complete":
                        self.reply(200, {"ok": proxy.complete_restore(req["key"])})
                    elif path == "/expire":
                        self.reply(200, {"ok": proxy.expire_restore(req["key"])})
                    elif path == "/reset":
                        proxy.reset()
                        self.reply(200, {"ok": True})
                    else:
                        self.reply(404, {"error": "no such control path"})
                except (KeyError, ValueError) as e:
                    self.reply(400, {"error": "bad request: %s" % e})

        return Control


def control(port, method, path, body=None, timeout=30):
    """A call on the control port of a proxy another process runs: the JSON
    it answers."""
    c = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    try:
        data = json.dumps(body).encode() if body is not None else None
        c.request(method, path, body=data, headers={"Content-Type": "application/json"} if data else {})
        r = c.getresponse()
        return json.loads(r.read() or b"null")
    finally:
        c.close()


def main(argv):
    p = argparse.ArgumentParser(description="An archive tier in front of an S3 server; see this tool's docstring.")
    p.add_argument("--upstream", type=int, help="the S3 server's loopback port")
    p.add_argument("--port", type=int, default=0, help="the port to serve S3 on (default: any free one)")
    p.add_argument("--control-port", type=int, default=0, help="the port of the control calls (default: any free one)")
    p.add_argument("--restore-delay", type=float, help="seconds a restore takes (default: only a control call completes it)")
    p.add_argument("--day-seconds", type=float, default=DAY_SECONDS, help="how long a Days of 1 lasts")
    p.add_argument("--archive", action="append", default=[], help="a key (/bucket/key) to mark archived; repeatable")
    p.add_argument("--self-test", action="store_true", help="check the proxy against a small S3 server of its own")
    args = p.parse_args(argv)
    if args.self_test:
        from nopacs_scripts.tools import s3_stub_selftest
        return s3_stub_selftest.run()
    if not args.upstream:
        p.error("--upstream is required")
    proxy = ArchiveProxy(args.upstream, args.port, args.control_port, args.restore_delay, args.day_seconds)
    for k in args.archive:
        proxy.archive(k)
    print("s3_stub: S3 on port %d, control on port %d, upstream %d" % (proxy.port, proxy.control_port, args.upstream), flush=True)
    try:
        while True:
            time.sleep(3600)
    except KeyboardInterrupt:
        pass
    finally:
        proxy.stop()
    return 0
