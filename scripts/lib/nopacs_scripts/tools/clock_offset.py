"""Move the time of a NoPACs server built for testing, so a proof sees a purge
after thirty days or a record kept for ten years without waiting.

    clock_offset.py set FILE [--days D] [--hours H] [--seconds S]
    clock_offset.py show FILE
    clock_offset.py --self-test

The server is built with the build tag "clock" (build_server below, or
`go build -tags clock ./cmd/nopacs-server`) and started with the environment
variable NOPACS_CLOCK_FILE naming FILE (server_env below). It reads the offset
in FILE, a Go duration such as "34560000s" or a number of days such as "400d",
at most every 200 ms, and adds it to every point in time it records or decides
by; waits and the deadlines of connections stay on the system's time. No
restart and no signal is needed: set rewrites FILE and the server follows
within a second (advance waits for that). A server built without the tag does
not read the file and ignores it. A file that does not exist is an offset of
zero; FILE is written whole, through a staging file renamed over it, so the
server never reads half of it.

Exit status: 0 and a line on standard output; 2 for a mistake in the command
line or a FILE that cannot be read. Nothing of NoPACs' is used.
"""

import argparse
import os
import subprocess
import tempfile
import time

from nopacs_scripts.paths import ROOT
from nopacs_scripts.report import Failure

ENV_FILE = "NOPACS_CLOCK_FILE"  # the variable the tagged server reads the offset file's name from
ENV_OFFSET = "NOPACS_CLOCK_OFFSET"  # the variable that gives a fixed offset at the server's start
SETTLE = 0.5  # seconds: the server reads the file at most every 0.2 s, and a read is quick


def build_server(go, out, cwd=None):
    """Builds cmd/nopacs-server with the build tag "clock" to the path out,
    with the Go tool named go; raises CalledProcessError when the build fails."""
    subprocess.run([go, "build", "-tags", "clock", "-o", out, os.path.join(ROOT, "cmd", "nopacs-server")], check=True, cwd=cwd)


def offset_text(days=0, hours=0, seconds=0):
    """The offset in the form the server reads: whole seconds, "<n>s"."""
    return "%ds" % round(days * 86400 + hours * 3600 + seconds)


def read_offset(path):
    """The offset in seconds that path holds (0 when it does not exist), for
    the forms "<n>s", "<n>h", "<n>m" and "<n>d"."""
    try:
        with open(path) as f:
            text = f.read().strip()
    except FileNotFoundError:
        return 0.0
    units = {"s": 1, "m": 60, "h": 3600, "d": 86400}
    try:
        return float(text[:-1]) * units[text[-1]]
    except (KeyError, ValueError, IndexError):
        raise Failure("clock_offset: %s holds %r, which is not an offset such as 34560000s or 400d" % (path, text))


def set_offset(path, days=0, hours=0, seconds=0):
    """Writes the offset to path whole (a staging file renamed over it) and
    returns the text written; the server follows within SETTLE seconds."""
    text = offset_text(days, hours, seconds)
    fd, stage = tempfile.mkstemp(prefix=os.path.basename(path) + ".", dir=os.path.dirname(os.path.abspath(path)))
    try:
        with os.fdopen(fd, "w") as f:
            f.write(text + "\n")
        os.replace(stage, path)
    except BaseException:
        if os.path.exists(stage):
            os.unlink(stage)
        raise
    return text


def advance(path, days=0, hours=0, seconds=0, settle=SETTLE):
    """Moves the offset by the amount given (added to the one in path), then
    waits settle seconds so the server has read it; returns the new offset in
    seconds."""
    total = read_offset(path) + days * 86400 + hours * 3600 + seconds
    set_offset(path, seconds=total)
    time.sleep(settle)
    return total


def server_env(path, base=None):
    """The environment to start a tagged server in: base (default: the
    process's own) with the variable that names path; path is created with an
    offset of zero when it is not there, so a later set is a rewrite."""
    if not os.path.exists(path):
        set_offset(path)
    env = dict(os.environ if base is None else base)
    env[ENV_FILE] = os.path.abspath(path)
    env.pop(ENV_OFFSET, None)
    return env


def main(argv):
    p = argparse.ArgumentParser(description="Move the time of a server built with the tag clock; see this tool's docstring.")
    p.add_argument("command", nargs="?", choices=("set", "show"))
    p.add_argument("file", nargs="?", help="the offset file the server was started with")
    p.add_argument("--days", type=float, default=0)
    p.add_argument("--hours", type=float, default=0)
    p.add_argument("--seconds", type=float, default=0)
    p.add_argument("--self-test", action="store_true", help="check the tool against files it writes and the Go side's own tests")
    args = p.parse_args(argv)
    if args.self_test:
        from nopacs_scripts.tools import clock_offset_selftest
        return clock_offset_selftest.run()
    if not args.command or not args.file:
        p.error("a command (set or show) and a file are required")
    if args.command == "set":
        print("clock_offset: %s now holds %s" % (args.file, set_offset(args.file, args.days, args.hours, args.seconds)))
    else:
        print("clock_offset: %s holds %.0f seconds (%.2f days)" % (args.file, read_offset(args.file), read_offset(args.file) / 86400))
    return 0
