"""A real NoPACs server on a temporary installation, for the tests to call.

The binary is built from this repository (static, as the release is) and
started on a port the system picks, without TLS and with TLS. Everything is synthetic:
a new installation, a key made on its local socket, no DICOM data.
"""

import json
import os
import pathlib
import shutil
import socket
import subprocess
import tempfile
import time

import pytest

import nopacs

REPO = pathlib.Path(__file__).resolve().parents[3]


def free_port() -> int:
    """Answer a loopback port nobody listens on."""
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class Server:
    """A running server, the installation it serves and the admin key for it."""

    def __init__(self, binary: pathlib.Path, work: pathlib.Path, serve_args: tuple = ()):
        self.binary = binary
        self.data = work / "d"
        self.data.mkdir(mode=0o700)
        self.env = dict(os.environ, NOPACS_DATA_DIR=str(self.data))
        self.server_cli("init")
        self.port = 0
        self.process = self._start(*serve_args)
        self.key = self.make_key("admin", admin=True)

    def cli(self, *args: str) -> str:
        """Run a nopacs command on the installation's local socket; answer its standard output."""
        done = subprocess.run(
            [str(self.binary), *args], env=self.env, capture_output=True, text=True, timeout=60
        )
        assert done.returncode == 0, f"nopacs {' '.join(args)}: {done.stderr}"
        return done.stdout

    def server_cli(self, *args: str) -> str:
        """Run a nopacs-server command, built beside the nopacs binary; answer its standard output."""
        done = subprocess.run(
            [str(self.binary.with_name("nopacs-server")), *args],
            env=self.env, capture_output=True, text=True, timeout=60,
        )
        assert done.returncode == 0, f"nopacs-server {' '.join(args)}: {done.stderr}"
        return done.stdout

    def make_key(self, holder: str, admin: bool = False) -> str:
        """Make a key on the local socket, and give it admin on the installation if asked."""
        out = self.cli("key", "create", "--holder", holder, "--no-expiry")
        key = out.splitlines()[0].strip()
        assert key.startswith("nopacs_"), "the first line of key create is the key"
        if admin:
            key_id = key.split("_")[1]
            self.cli("grant", "set", key_id, "--role", "admin", "--installation")
        return key

    def _start(self, *extra: str) -> subprocess.Popen:
        """Start the server on a loopback port it picks, wait until it answers, and set self.port."""
        log_path = self.data.parent / "serve.log"
        with open(log_path, "wb") as log:
            process = subprocess.Popen(
                [str(self.binary.with_name("nopacs-server")), "serve", "--listen", "127.0.0.1:0", *extra],
                env=self.env, stdout=log, stderr=log,
            )
        sock = self.data / "run" / "nopacs.sock"
        for _ in range(300):
            if process.poll() is not None:
                raise RuntimeError(f"nopacs serve exited: {log_path.read_text()}")
            port = self._logged_port(log_path)
            if port and sock.exists():
                try:
                    self.cli("whoami")
                    self.port = port
                    return process
                except AssertionError:
                    pass
            time.sleep(0.05)
        process.kill()
        raise RuntimeError("nopacs serve did not answer in 15 seconds")

    @staticmethod
    def _logged_port(log_path: pathlib.Path) -> int:
        """The port the server logged for its network listener; 0 until it has."""
        for line in log_path.read_text().splitlines():
            try:
                entry = json.loads(line)
            except ValueError:
                continue
            if entry.get("msg") == "server: listening on the network":
                return int(entry["address"].rsplit(":", 1)[1])
        return 0

    def stop(self) -> None:
        """Stop the server the way an operator does, and wait for it."""
        self.process.terminate()
        self.process.wait(timeout=30)


@pytest.fixture(scope="session")
def server():
    """The server, built and started once for the whole run."""
    # A short directory: the local socket's path must fit a Unix socket address.
    work = pathlib.Path(tempfile.mkdtemp(prefix="nopacs-py."))
    binary = work / "nopacs"
    env = dict(os.environ, CGO_ENABLED="0")
    for name in ("nopacs", "nopacs-server"):
        subprocess.run(
            ["go", "build", "-o", str(work / name), f"./cmd/{name}"], cwd=REPO, env=env, check=True, timeout=600
        )
    srv = Server(binary, work)
    yield srv
    srv.stop()
    shutil.rmtree(work, ignore_errors=True)


@pytest.fixture
def client(server):
    """A client holding the admin key."""
    with nopacs.Client(f"127.0.0.1:{server.port}", server.key, tls=False) as c:
        yield c
