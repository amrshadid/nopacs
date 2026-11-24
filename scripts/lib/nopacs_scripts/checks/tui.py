"""Run the built nopacs binary's interactive program under a pseudo-terminal.

This is the outside check of internal/tui: a tool that is not NoPACs' own
code (Python's pty module, a real pseudo-terminal) starts the real binary against
the in-process fake server (internal/testkit/cmd/ptyserve), presses keys, reads what the
program draws, quits, and asserts what the terminal was left in. It uses only
the standard library, so it runs wherever python3 does.

The screen is read through a small terminal emulator in this file (class
Screen): Bubble Tea redraws by differences, so a word that changed can be split
by cursor movements in the raw bytes and is whole only on the screen.

It shows, on the built binary and not on a test double:

  * the program opens on a terminal with no arguments and draws its screens;
  * a quit key ends it with status 0, and the terminal is restored: the
    alternate screen entered, then left, and the cursor shown again after;
  * a SIGTERM sent mid-session ends it with status 0 and the same restoration;
    a SIGINT sent mid-session ends it with status 130 and the same;
  * the list of tenants asks the server for a page at a time (the helper counts
    the calls the server really answered);
  * with output that is not a terminal, or with any argument, it does not open:
    the usage line and exit status 2, as before.

Usage: tui.py --binary PATH --helper PATH
Linux and macOS only: Windows has no pty module.
"""

import argparse
import fcntl
import os
import pty
import re
import select
import signal
import struct
import subprocess
import termios
import time

from nopacs_scripts.report import Failure

ENTER_ALT = b"\x1b[?1049h"
LEAVE_ALT = b"\x1b[?1049l"
SHOW_CURSOR = b"\x1b[?25h"

# The escape sequences of a terminal's output, removed so text can be looked
# for in what the program drew. Cursor movement is dropped, so words on
# different lines are not told apart by position; the checks look for words.
ANSI = re.compile(rb"\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][A-Za-z0-9]|\x1b[=>]")

failures = []


class Screen:
    """A small terminal emulator: what the program drew, as a person sees it.

    Bubble Tea redraws by differences (it moves the cursor, writes only the
    cells that changed, and may use insert mode), so the stream of bytes does
    not hold a frame's text in one piece. This applies the sequences the
    renderer uses to a grid of cells, so a check can read the screen. It
    handles what the program emits (cursor movement, erase, insert and delete,
    repeat, insert mode, carriage return and line feed) and ignores colors,
    modes and anything else.
    """

    def __init__(self, cols, rows):
        self.resize(cols, rows)
        self.pending = b""

    def resize(self, cols, rows):
        """Clear the screen for a new size; the program redraws on a resize."""
        self.cols, self.rows = cols, rows
        self.grid = [[" "] * cols for _ in range(rows)]
        self.r = self.c = 0
        self.insert = False
        self.last = " "

    def text(self):
        """The screen's rows, without trailing spaces, one per line."""
        return "\n".join("".join(row).rstrip() for row in self.grid)

    def put(self, ch):
        row = self.grid[self.r]
        if self.insert:
            row.insert(self.c, ch)
            del row[self.cols:]
        else:
            row[self.c] = ch
        self.last = ch
        self.c = min(self.c + 1, self.cols - 1)

    def feed(self, data):
        """Apply bytes the program wrote."""
        data = self.pending + data
        self.pending = b""
        text = data.decode("utf-8", "ignore")
        # An escape sequence or a character cut at the end waits for more.
        cut = text.rfind("\x1b")
        if cut >= 0 and not re.match(r"\x1b(\[[0-9;?<>=]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[^\[\]])", text[cut:]):
            self.pending = text[cut:].encode()
            text = text[:cut]
        i = 0
        while i < len(text):
            ch = text[i]
            if ch == "\x1b":
                m = re.match(r"\x1b\[([0-9;?<>=]*)([ -/]*)([@-~])", text[i:])
                if m:
                    self.csi(m.group(1), m.group(2), m.group(3))
                    i += m.end()
                    continue
                m = re.match(r"\x1b\][^\x07\x1b]*(\x07|\x1b\\)", text[i:])
                if m:
                    i += m.end()
                    continue
                i += 2  # ESC and one more character: a charset or a keypad mode
                continue
            i += 1
            if ch == "\r":
                self.c = 0
            elif ch == "\n":
                self.r = min(self.r + 1, self.rows - 1)
            elif ch == "\b":
                self.c = max(self.c - 1, 0)
            elif ch == "\t":
                self.c = min((self.c // 8 + 1) * 8, self.cols - 1)
            elif ch >= " ":
                self.put(ch)

    def csi(self, params, _inter, final):
        """Apply one control sequence."""
        if params and params[0] in "?<>=":
            return
        nums = [int(x) if x else 0 for x in params.split(";")] if params else []
        n = nums[0] if nums and nums[0] else 1
        row = self.grid[self.r]
        if final in "Hf":
            r = (nums[0] if nums and nums[0] else 1) - 1
            c = (nums[1] if len(nums) > 1 and nums[1] else 1) - 1
            self.r, self.c = min(r, self.rows - 1), min(c, self.cols - 1)
        elif final == "A":
            self.r = max(self.r - n, 0)
        elif final in "Be":
            self.r = min(self.r + n, self.rows - 1)
        elif final in "Ca":
            self.c = min(self.c + n, self.cols - 1)
        elif final == "D":
            self.c = max(self.c - n, 0)
        elif final == "E":
            self.r, self.c = min(self.r + n, self.rows - 1), 0
        elif final == "F":
            self.r, self.c = max(self.r - n, 0), 0
        elif final in "G`":
            self.c = min(n - 1, self.cols - 1)
        elif final == "d":
            self.r = min(n - 1, self.rows - 1)
        elif final == "J":
            mode = nums[0] if nums else 0
            if mode == 0:
                row[self.c:] = [" "] * (self.cols - self.c)
                for k in range(self.r + 1, self.rows):
                    self.grid[k] = [" "] * self.cols
            elif mode == 1:
                row[: self.c + 1] = [" "] * (self.c + 1)
                for k in range(self.r):
                    self.grid[k] = [" "] * self.cols
            else:
                self.grid = [[" "] * self.cols for _ in range(self.rows)]
        elif final == "K":
            mode = nums[0] if nums else 0
            if mode == 0:
                row[self.c:] = [" "] * (self.cols - self.c)
            elif mode == 1:
                row[: self.c + 1] = [" "] * (self.c + 1)
            else:
                self.grid[self.r] = [" "] * self.cols
        elif final == "@":
            row[self.c:self.c] = [" "] * n
            del row[self.cols:]
        elif final == "P":
            del row[self.c:self.c + n]
            row.extend([" "] * (self.cols - len(row)))
        elif final == "X":
            for k in range(self.c, min(self.c + n, self.cols)):
                row[k] = " "
        elif final == "L":
            for _ in range(n):
                self.grid.insert(self.r, [" "] * self.cols)
                del self.grid[self.rows:]
        elif final == "M":
            for _ in range(n):
                del self.grid[self.r]
                self.grid.append([" "] * self.cols)
        elif final == "b":
            for _ in range(n):
                self.put(self.last)
        elif final in "hl" and params == "4":
            self.insert = final == "h"


def check(ok, what):
    """Record one assertion and print its result."""
    print(("ok    " if ok else "FAIL  ") + what)
    if not ok:
        failures.append(what)


def text_of(raw):
    """What a person would read in raw terminal output."""
    return ANSI.sub(b"", raw).decode("utf-8", "replace")


class Session:
    """The binary running on a pseudo-terminal of a given size."""

    def __init__(self, binary, datadir, cols=80, rows=24, env_extra=None):
        master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
        env = dict(os.environ)
        env.pop("NO_COLOR", None)
        env.update({"TERM": "xterm-256color", "NOPACS_DATA_DIR": datadir})
        env.update(env_extra or {})
        self.proc = subprocess.Popen(
            [binary], stdin=slave, stdout=slave, stderr=slave, env=env,
            start_new_session=True, close_fds=True,
            preexec_fn=lambda: fcntl.ioctl(0, termios.TIOCSCTTY, 0),
        )
        os.close(slave)
        self.master = master
        self.raw = b""
        self.screen = Screen(cols, rows)

    def read(self, timeout=0.2):
        """Read what is available for up to timeout seconds."""
        end = time.time() + timeout
        while time.time() < end:
            ready, _, _ = select.select([self.master], [], [], max(end - time.time(), 0))
            if not ready:
                break
            try:
                chunk = os.read(self.master, 65536)
            except OSError:
                break
            if not chunk:
                break
            self.raw += chunk
            self.screen.feed(chunk)

    def wait_for(self, text, timeout=20.0):
        """Read until the screen shows text; False when it never does."""
        end = time.time() + timeout
        while time.time() < end:
            self.read(0.1)
            if text in self.screen.text():
                return True
            if self.proc.poll() is not None:
                self.read(0.2)
                return text in self.screen.text()
        return False

    def resize(self, cols, rows):
        """Resize the terminal and tell the program."""
        fcntl.ioctl(self.master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
        self.screen.resize(cols, rows)
        os.kill(self.proc.pid, signal.SIGWINCH)

    def press(self, keys):
        """Type keys, as bytes, into the terminal."""
        os.write(self.master, keys if isinstance(keys, bytes) else keys.encode())

    def finish(self, timeout=20.0):
        """Wait for the program to end; returns its status, or None on a hang."""
        end = time.time() + timeout
        while time.time() < end:
            self.read(0.1)
            code = self.proc.poll()
            if code is not None:
                self.read(0.3)
                os.close(self.master)
                return code
        self.proc.kill()
        self.proc.wait()
        os.close(self.master)
        return None

    def restored(self):
        """True when the terminal was left as found: the alternate screen
        entered and then left last, and the cursor shown after the last enter."""
        i, j = self.raw.rfind(ENTER_ALT), self.raw.rfind(LEAVE_ALT)
        return i >= 0 and j > i and SHOW_CURSOR in self.raw[i:]


def helper_stats(helper):
    """Ask the helper how many calls each method has had."""
    helper.stdin.write(b"stats\n")
    helper.stdin.flush()
    line = helper.stdout.readline().decode()
    return dict((k, int(v)) for k, v in (p.split("=") for p in line.split()[1:]))


def start_helper(path, *args):
    """Start the fake server helper and return it with its data directory."""
    helper = subprocess.Popen([path, *args], stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    line = helper.stdout.readline().decode().strip()
    if not line.startswith("DATADIR="):
        raise Failure("the helper did not start: %r" % line)
    return helper, line[len("DATADIR="):]


def stop_helper(helper):
    helper.stdin.close()
    try:
        helper.wait(timeout=5)
    except subprocess.TimeoutExpired:
        helper.kill()


def scenario_quit(binary, helper_path):
    print("-- quit with q")
    helper, datadir = start_helper(helper_path)
    try:
        s = Session(binary, datadir)
        check(s.wait_for("local operator"), "the header says who is connected")
        check(s.wait_for("Audit log"), "the menu is drawn")
        check(s.wait_for("nopacs | local operator (user 501) | "), "the header line reads whole on the screen")
        s.press("1")
        check(s.wait_for("t000"), "the tenant list is drawn after a key")
        check("connection refused" in s.screen.text(), "a tenant that is not ready shows its reason")
        s.press("q")
        code = s.finish()
        check(code == 0, "q ends the program with status 0 (got %r)" % code)
        check(s.restored(), "the alternate screen was left and the cursor shown")
        check(ENTER_ALT in s.raw, "the program used the alternate screen")
    finally:
        stop_helper(helper)


def scenario_signal(binary, helper_path, sig, expect_code, name):
    print("-- %s mid-session" % name)
    helper, datadir = start_helper(helper_path)
    try:
        s = Session(binary, datadir)
        check(s.wait_for("local operator"), "the program is up")
        s.press("1")
        check(s.wait_for("t000"), "the list is on screen when the signal arrives")
        os.kill(s.proc.pid, sig)
        code = s.finish()
        check(code == expect_code, "%s ends the program with status %d (got %r)" % (name, expect_code, code))
        check(s.restored(), "after %s the alternate screen was left and the cursor shown" % name)
    finally:
        stop_helper(helper)


def scenario_paging(binary, helper_path):
    print("-- paging on demand, 250 tenants")
    helper, datadir = start_helper(helper_path, "-tenants", "250")
    try:
        s = Session(binary, datadir)
        check(s.wait_for("local operator"), "the program is up")
        s.press("1")
        check(s.wait_for("[100+ rows]"), "the list says 100+ rows after the first page")
        check(helper_stats(helper).get("ListTenants") == 1, "one page was fetched when the list opened")
        for _ in range(60):
            s.press("j")
        s.read(0.5)
        check(helper_stats(helper).get("ListTenants") == 1, "60 rows down, still one page")
        for _ in range(40):
            s.press("j")
        # The screen is drawn as differences, so one changed digit is not a
        # word to look for; the server's own count of the calls it answered is
        # the proof, read soon, before the list's five-second refresh adds one.
        calls = 1
        end = time.time() + 3
        while calls < 2 and time.time() < end:
            time.sleep(0.05)
            calls = helper_stats(helper).get("ListTenants", 0)
        check(calls == 2, "near the end of the first page the second is fetched (%d calls)" % calls)
        check(s.wait_for("[200+ rows]"), "the screen then says 200+ rows")
        s.read(0.3)
        check(helper_stats(helper).get("ListTenants") == 2, "exactly two pages fetched, not the third")
        s.press("q")
        check(s.finish() == 0, "q ends the program")
    finally:
        stop_helper(helper)


def scenario_small_and_resize(binary, helper_path):
    print("-- resize")
    helper, datadir = start_helper(helper_path)
    try:
        s = Session(binary, datadir, cols=80, rows=24)
        check(s.wait_for("local operator"), "the program is up at 80 by 24")
        s.resize(30, 30)
        check(s.wait_for("terminal too small"), "a terminal too small says so after a resize")
        s.resize(200, 50)
        s.press("2")
        check(s.wait_for("nightly export"), "after resizing back the screens draw again")
        s.press("q")
        check(s.finish() == 0, "q ends the program")
    finally:
        stop_helper(helper)


def scenario_not_a_terminal(binary, helper_path):
    print("-- no terminal, or an argument: it does not open")
    helper, datadir = start_helper(helper_path)
    try:
        env = dict(os.environ, NOPACS_DATA_DIR=datadir)
        want = b'nopacs: no command (usage: nopacs <noun> <verb> [flags], for example nopacs tenant list); try "nopacs --help"\n'
        # Standard output is a pipe; standard input is the null device, which
        # is a character device and must not be taken for a terminal.
        with open(os.devnull) as null:
            r = subprocess.run([binary], stdin=null, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, timeout=10)
        check(r.returncode == 2 and r.stderr == want and r.stdout == b"", "output a pipe: usage line, exit 2")
        # Standard input a terminal, standard output a pipe.
        master, slave = pty.openpty()
        r = subprocess.run([binary], stdin=slave, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, timeout=10)
        os.close(master)
        os.close(slave)
        check(r.returncode == 2 and r.stderr == want, "input a terminal, output a pipe: usage line, exit 2")
        # Standard output a terminal, standard input a pipe.
        master, slave = pty.openpty()
        r = subprocess.run([binary], stdin=subprocess.DEVNULL, stdout=slave, stderr=subprocess.PIPE, env=env, timeout=10)
        os.close(master)
        os.close(slave)
        check(r.returncode == 2 and r.stderr == want, "output a terminal, input not: usage line, exit 2")
        # An argument, on a terminal.
        master, slave = pty.openpty()
        r = subprocess.run([binary, "tenant", "list"], stdin=slave, stdout=slave, stderr=subprocess.PIPE, env=env, timeout=10)
        out = os.read(master, 65536)
        os.close(master)
        os.close(slave)
        check(r.returncode == 0 and ENTER_ALT not in out and b"t000" in out, "an argument on a terminal runs the command, not the program")
    finally:
        stop_helper(helper)


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--binary", required=True, help="the built nopacs binary")
    ap.add_argument("--helper", required=True, help="the built ptyserve helper")
    args = ap.parse_args(argv)

    scenario_quit(args.binary, args.helper)
    scenario_signal(args.binary, args.helper, signal.SIGTERM, 0, "SIGTERM")
    scenario_signal(args.binary, args.helper, signal.SIGINT, 130, "SIGINT")
    scenario_paging(args.binary, args.helper)
    scenario_small_and_resize(args.binary, args.helper)
    scenario_not_a_terminal(args.binary, args.helper)

    if failures:
        print("\n%d check(s) failed" % len(failures))
        return 1
    print("\nall checks passed")

