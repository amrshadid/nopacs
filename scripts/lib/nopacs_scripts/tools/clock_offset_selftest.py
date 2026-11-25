"""The self-test of clock_offset.py: the offset file is written, read back,
advanced and rewritten whole while another thread reads it, a malformed file
is refused, the environment names the file, and, when GO names a Go tool
(the environment variable GO, default "go"), the Go side's own tests of the
tagged clock run, which read the same forms of offset. Run by
`clock_offset.py --self-test`.
"""

import collections
import os
import shutil
import subprocess
import tempfile
import threading

from nopacs_scripts.paths import ROOT
from nopacs_scripts.proof import Reporting
from nopacs_scripts.report import Failure
from nopacs_scripts.tools import clock_offset as co


def run():
    """Runs every check; 0 when all hold, 1 otherwise."""
    t = Reporting()
    t.counts = collections.Counter()
    tmp = tempfile.mkdtemp(prefix="clock-offset-")
    try:
        path = os.path.join(tmp, "offset")
        t.result("a file that does not exist is an offset of zero", co.read_offset(path) == 0, "read %r" % co.read_offset(path))
        text = co.set_offset(path, days=400)
        with open(path) as f:
            body = f.read()
        t.result("400 days are written as whole seconds and read back", text == "34560000s" and body == "34560000s\n" and co.read_offset(path) == 400 * 86400,
                 "wrote %r, read %r" % (body, co.read_offset(path)))
        t.result("hours and seconds add to days", co.offset_text(days=1, hours=2, seconds=3) == "93603s", co.offset_text(days=1, hours=2, seconds=3))
        t.result("advance adds to what the file holds", co.advance(path, days=1, settle=0) == 401 * 86400 and co.read_offset(path) == 401 * 86400,
                 "read %r" % co.read_offset(path))
        for body in ("400d\n", "36h", "90m"):
            with open(path, "w") as f:
                f.write(body)
        t.result("the forms the server reads (d, h, m) are read here too", co.read_offset(path) == 90 * 60, "read %r" % co.read_offset(path))
        with open(path, "w") as f:
            f.write("tomorrow\n")
        try:
            co.read_offset(path)
            refused = False
        except Failure:
            refused = True
        t.result("a file that is not an offset is refused", refused, "refused: %s" % refused)

        bad = []
        done = threading.Event()

        def reader():
            while not done.is_set():
                try:
                    v = co.read_offset(path)
                except Failure:
                    bad.append("unreadable")
                    return
                if v not in (0, 1000 * 86400):
                    bad.append(v)

        co.set_offset(path)
        th = threading.Thread(target=reader)
        th.start()
        for i in range(300):
            co.set_offset(path, days=1000 if i % 2 else 0)
        done.set()
        th.join()
        t.result("a file rewritten again and again is never read half written", not bad and not [n for n in os.listdir(tmp) if n != "offset"],
                 "bad reads %s; files left %s" % (bad[:3], sorted(os.listdir(tmp))))

        shutil.rmtree(tmp)
        os.mkdir(tmp)
        env = co.server_env(os.path.join(tmp, "o"), {"PATH": "/bin", co.ENV_OFFSET: "5d"})
        t.result("the server's environment names the file, drops a fixed offset, and the file starts at zero",
                 env[co.ENV_FILE] == os.path.join(tmp, "o") and co.ENV_OFFSET not in env and co.read_offset(env[co.ENV_FILE]) == 0, "%s" % env)

        go = os.environ.get("GO", "go")
        try:
            r = subprocess.run([go, "test", "-count=1", "-tags", "clock", "./internal/clock"], cwd=ROOT, capture_output=True, text=True, timeout=170)
            t.result("the Go side's tests of the tagged clock pass (go test -tags clock ./internal/clock)", r.returncode == 0, (r.stdout + r.stderr).strip()[-300:])
        except (OSError, subprocess.TimeoutExpired) as e:
            t.skip("the Go side's tests of the tagged clock", "%s did not run: %s" % (go, e))
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    print("clock_offset self-test: %d ok, %d FAIL, %d skipped" % (t.counts["ok"], t.counts["FAIL"], t.counts["skip"]))
    return 0 if t.counts["FAIL"] == 0 else 1
