"""What the run object of every proof does the same way: count and print
results, tell which program a command belongs to, read a command's JSON and
stop the server it started. A proof's own class adds what is its own (its
work directory, its tenants, its ports) and gives these methods the
attributes they use: counts, bin, sbin, server and m(*args)."""

import json
import platform
import subprocess


class Reporting:
    """The results of a proof: one printed line each, and the counts."""

    def result(self, name, ok, detail, evidence=()):
        """Counts and prints one check; the first 12 lines of evidence follow a failure."""
        word = "ok" if ok else "FAIL"
        self.counts[word] += 1
        print("%-4s %s: %s" % (word, name, detail), flush=True)
        if not ok:
            for e in list(evidence)[:12]:
                print("     | %s" % e, flush=True)

    def skip(self, name, why):
        """Counts and prints one check that could not run here, and why."""
        self.counts["skip"] += 1
        print("skip %s: %s" % (name, why), flush=True)

    def note(self, text):
        """Prints a line that is not a check."""
        print("     " + text, flush=True)


class Programs:
    """The two programs a proof runs, and the server among them."""

    def exe(self, *args):
        """The program a command belongs to: the commands that touch the data directory with the server stopped are nopacs-server's."""
        server = args[:1] in (("init",), ("backup",)) or args[:2] == ("tenant", "import")
        return self.sbin if server else self.bin

    def mj(self, *args):
        """A command's --json output, parsed."""
        return json.loads(self.m(*args, "--json"))

    def stop(self):
        """Ends the server as a person does (SIGTERM), killing it if it does not end in a minute."""
        if self.server and self.server.poll() is None:
            self.server.terminate()
            try:
                self.server.wait(60)
            except subprocess.TimeoutExpired:
                self.server.kill()
                self.server.wait()


def machine():
    """The hardware and system a measured number was taken on, for the line
    that states it: the processor and memory where sysctl names them, else
    the system's name."""
    try:
        cpu = subprocess.run(["sysctl", "-n", "machdep.cpu.brand_string"], capture_output=True, text=True).stdout.strip()
        mem = int(subprocess.run(["sysctl", "-n", "hw.memsize"], capture_output=True, text=True).stdout.strip()) / 2**30
        return "%s, %.0f GiB, %s %s" % (cpu, mem, platform.system(), platform.release())
    except (OSError, ValueError):
        return "%s %s %s" % (platform.system(), platform.release(), platform.machine())
