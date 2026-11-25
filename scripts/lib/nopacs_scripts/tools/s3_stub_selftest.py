"""The self-test of s3_stub.py: the proxy is put in front of a small S3-like
server of this file (objects in a dictionary, GET, HEAD, PUT, DELETE), a clock
the test moves, and each behavior the stub promises is read back over HTTP:
what an ordinary key does, the 403 InvalidObjectState of an archived one, its
HEAD headers before, during and after a restore, the one RestoreObject a
second request does not make again, the delayed completion, the expiry, the
answers for a key that is not archived, and a control port S3 clients do not
reach. Run by `s3_stub.py --self-test`.
"""

import collections
import http.client
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from nopacs_scripts.proof import Reporting
from nopacs_scripts.tools.s3_stub import ArchiveProxy, control


class Origin(Reporting):
    """The small S3-like server behind the proxy, and the results of the test."""

    def __init__(self):
        self.counts = collections.Counter()
        self.objects = {}
        origin = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def send(self, status, body=b""):
                self.send_response(status)
                self.send_header("Content-Length", str(len(body)))
                self.send_header("ETag", '"x"')
                self.end_headers()
                if self.command != "HEAD":
                    self.wfile.write(body)

            def do_PUT(self):
                n = int(self.headers.get("Content-Length", 0) or 0)
                origin.objects[self.path] = self.rfile.read(n)
                self.send(200)

            def do_GET(self):
                if self.path in origin.objects:
                    self.send(200, origin.objects[self.path])
                else:
                    self.send(404, b"<Error><Code>NoSuchKey</Code></Error>")

            def do_HEAD(self):
                body = origin.objects.get(self.path)
                self.send_response(200 if body is not None else 404)
                self.send_header("Content-Length", str(len(body or b"")))
                self.end_headers()

            def do_POST(self):
                n = int(self.headers.get("Content-Length", 0) or 0)
                self.rfile.read(n)
                if self.path.split("?")[0] in origin.objects:
                    self.send(403, b"<Error><Code>InvalidObjectState</Code></Error>")
                else:
                    self.send(404, b"<Error><Code>NoSuchKey</Code></Error>")

            def do_DELETE(self):
                origin.objects.pop(self.path, None)
                self.send(204)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def stop(self):
        self.server.shutdown()
        self.server.server_close()


def call(port, method, path, body=None, headers=None, chunked=False):
    """(status, headers lowercased, body) of one request."""
    c = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
    try:
        c.request(method, path, body=body, headers=headers or {}, encode_chunked=chunked)
        r = c.getresponse()
        data = r.read()
        return r.status, {k.lower(): v for k, v in r.getheaders()}, data
    finally:
        c.close()


def run():
    """Runs every check; 0 when all hold, 1 otherwise."""
    now = [1_800_000_000.0]
    origin = Origin()
    proxy = ArchiveProxy(origin.port, restore_delay=None, day_seconds=100, clock=lambda: now[0])
    try:
        body = b"DICM" + bytes(range(200))
        for k in ("/b/std.dcm", "/b/cold.dcm", "/b/late.dcm", "/b/deep.dcm"):
            call(proxy.port, "PUT", k, body)
        proxy.archive("/b/cold.dcm")
        proxy.archive("/b/late.dcm")
        proxy.archive("/b/deep.dcm", "DEEP_ARCHIVE")
        restore = b'<RestoreRequest xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Days>2</Days></RestoreRequest>'

        st, h, d = call(proxy.port, "GET", "/b/std.dcm")
        origin.result("an ordinary key is served as the S3 server serves it", st == 200 and d == body, "HTTP %d, %d bytes" % (st, len(d)))
        st, h, _ = call(proxy.port, "HEAD", "/b/std.dcm")
        origin.result("HEAD of an ordinary key has its length and no storage class or restore header",
                      st == 200 and h.get("content-length") == str(len(body)) and "x-amz-storage-class" not in h and "x-amz-restore" not in h,
                      "HTTP %d, headers %s" % (st, sorted(h)))

        st, h, d = call(proxy.port, "GET", "/b/cold.dcm")
        origin.result("GET of an archived key: 403 InvalidObjectState naming the storage class",
                      st == 403 and b"<Code>InvalidObjectState</Code>" in d and b"<StorageClass>GLACIER</StorageClass>" in d, "HTTP %d %s" % (st, d.decode()))
        st, h, _ = call(proxy.port, "HEAD", "/b/cold.dcm")
        origin.result("HEAD of an archived key: x-amz-storage-class GLACIER, its length, no x-amz-restore",
                      st == 200 and h.get("x-amz-storage-class") == "GLACIER" and h.get("content-length") == str(len(body)) and "x-amz-restore" not in h,
                      "HTTP %d, %s" % (st, {k: v for k, v in h.items() if k.startswith("x-amz") or k == "content-length"}))
        st, h, _ = call(proxy.port, "HEAD", "/b/deep.dcm")
        origin.result("a DEEP_ARCHIVE key says its class", h.get("x-amz-storage-class") == "DEEP_ARCHIVE", "x-amz-storage-class %s" % h.get("x-amz-storage-class"))

        st, _, _ = call(proxy.port, "POST", "/b/cold.dcm?restore", restore)
        st2, _, d2 = call(proxy.port, "POST", "/b/cold.dcm?restore", restore)
        _, h, _ = call(proxy.port, "HEAD", "/b/cold.dcm")
        st3, _, _ = call(proxy.port, "GET", "/b/cold.dcm")
        origin.result("RestoreObject starts a restore (202), a second is RestoreAlreadyInProgress (409), HEAD says ongoing-request true, GET is still refused",
                      st == 202 and st2 == 409 and b"RestoreAlreadyInProgress" in d2 and h.get("x-amz-restore") == 'ongoing-request="true"' and st3 == 403,
                      "statuses %d %d, x-amz-restore %s, GET %d" % (st, st2, h.get("x-amz-restore"), st3))
        origin.result("the log holds each RestoreObject with its status, one accepted",
                      proxy.count("RestoreObject", "/b/cold.dcm") == 2 and proxy.count("RestoreObject", "/b/cold.dcm", 202) == 1 and proxy.refused == 2,
                      "RestoreObject %d, accepted %d, refused GETs %d" % (proxy.count("RestoreObject"), proxy.count("RestoreObject", None, 202), proxy.refused))

        done = control(proxy.control_port, "POST", "/complete", {"key": "/b/cold.dcm"})
        st, h, d = call(proxy.port, "GET", "/b/cold.dcm")
        _, hh, _ = call(proxy.port, "HEAD", "/b/cold.dcm")
        m = re.fullmatch(r'ongoing-request="false", expiry-date="\w{3}, \d\d \w{3} \d{4} \d\d:\d\d:\d\d GMT"', hh.get("x-amz-restore", ""))
        origin.result("completed by the control port: GET serves the bytes, HEAD says ongoing-request false with an expiry date",
                      done == {"ok": True} and st == 200 and d == body and m is not None, "complete %s, GET %d (%d bytes), x-amz-restore %s" % (done, st, len(d), hh.get("x-amz-restore")))

        now[0] += 150
        st, _, _ = call(proxy.port, "GET", "/b/cold.dcm")
        origin.result("still served inside the 2-day stay (150 of 200 s)", st == 200, "HTTP %d" % st)
        now[0] += 100
        st, _, _ = call(proxy.port, "GET", "/b/cold.dcm")
        _, hh, _ = call(proxy.port, "HEAD", "/b/cold.dcm")
        origin.result("past the expiry GET is refused again and HEAD has no x-amz-restore", st == 403 and "x-amz-restore" not in hh,
                      "HTTP %d, x-amz-restore %s" % (st, hh.get("x-amz-restore")))

        proxy.restore_delay = 30
        call(proxy.port, "POST", "/b/late.dcm?restore", restore)
        now[0] += 10
        st, _, _ = call(proxy.port, "GET", "/b/late.dcm")
        now[0] += 25
        st2, _, d2 = call(proxy.port, "GET", "/b/late.dcm")
        origin.result("a restore with a delay of 30 s completes by the clock: refused at 10 s, served at 35 s", st == 403 and st2 == 200 and d2 == body,
                      "HTTP %d then %d" % (st, st2))

        call(proxy.port, "POST", "/b/cold.dcm?restore", restore)
        proxy.complete_restore("/b/cold.dcm")
        ended = control(proxy.control_port, "POST", "/expire", {"key": "/b/cold.dcm"})
        st, _, _ = call(proxy.port, "GET", "/b/cold.dcm")
        origin.result("an expire call ends the stay at once", ended == {"ok": True} and st == 403, "expire %s, GET %d" % (ended, st))

        st, _, d = call(proxy.port, "POST", "/b/std.dcm?restore", restore)
        st2, _, _ = call(proxy.port, "POST", "/b/nothing.dcm?restore", restore)
        origin.result("RestoreObject of an ordinary key and of a missing key get the S3 server's own answers (403, 404)", st == 403 and st2 == 404,
                      "HTTP %d and %d" % (st, st2))

        st, _, _ = call(proxy.port, "POST", "/archive", b'{"key": "/b/std.dcm"}')
        got = control(proxy.control_port, "GET", "/state")
        origin.result("the control calls are not on the S3 port, and the control port lists state",
                      st == 404 and "/b/cold.dcm" in got and got["/b/deep.dcm"]["class"] == "DEEP_ARCHIVE", "S3 port answered %d; state of %s" % (st, sorted(got)))
        control(proxy.control_port, "POST", "/archive", {"key": "/b/std.dcm"})
        st, _, _ = call(proxy.port, "GET", "/b/std.dcm")
        control(proxy.control_port, "POST", "/standard", {"key": "/b/std.dcm"})
        st2, _, _ = call(proxy.port, "GET", "/b/std.dcm")
        evs = control(proxy.control_port, "GET", "/events?op=GetObjectRefused&key=/b/std.dcm")
        origin.result("a key marked archived over the control port is refused, and served once marked standard; the events list the refusal",
                      st == 403 and st2 == 200 and len(evs) == 1, "GET %d then %d, %d refusal events" % (st, st2, len(evs)))

        st, _, _ = call(proxy.port, "PUT", "/b/chunked.dcm", iter([b"abc", b"def"]), {"Transfer-Encoding": "chunked"}, chunked=True)
        origin.result("a chunked PUT reaches the server whole", st == 200 and origin.objects.get("/b/chunked.dcm") == b"abcdef", "HTTP %d, stored %r" % (st, origin.objects.get("/b/chunked.dcm")))
        call(proxy.port, "DELETE", "/b/chunked.dcm")
        origin.result("a DELETE is passed on", "/b/chunked.dcm" not in origin.objects, "stored keys %s" % sorted(origin.objects))
    finally:
        proxy.stop()
        origin.stop()
    print("s3_stub self-test: %d ok, %d FAIL" % (origin.counts["ok"], origin.counts["FAIL"]))
    return 0 if origin.counts["FAIL"] == 0 else 1
