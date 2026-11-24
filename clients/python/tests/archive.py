"""A server holding a small public CT study, for the tests that read the archive.

The study is sent with dcmtk's storescu to a receiver the helper makes, on an
installation of its own, so the files the client reads are the ones a real
reception archived. Everything is a public corpus file: no patient data.
"""

import os
import pathlib
import shutil
import subprocess
import tempfile
import time

import pytest

from conftest import REPO, Server, free_port

CORPUS = REPO / "internal" / "receive" / "testdata"
# Two series: ct2n holds two instances, ct5n one.
STUDY = ["ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm"]
TENANT = "reading"


def find_storescu() -> str:
    """Answer the path of dcmtk's storescu, or skip the test that needs it."""
    candidates = []
    if os.environ.get("NOPACS_DCMTK"):
        candidates.append(pathlib.Path(os.environ["NOPACS_DCMTK"]) / "storescu")
    if shutil.which("storescu"):
        candidates.append(pathlib.Path(shutil.which("storescu")))
    for path in candidates:
        done = subprocess.run([str(path), "--version"], capture_output=True, text=True, timeout=60)
        if "dcmtk" in (done.stdout + done.stderr).lower():
            return str(path)
    pytest.skip("dcmtk's storescu is not installed; put it on the PATH or set NOPACS_DCMTK")


class Archive:
    """An installation with one tenant that holds the study, and its server."""

    def __init__(self, binary: pathlib.Path):
        storescu = find_storescu()
        self.work = pathlib.Path(tempfile.mkdtemp(prefix="nopacs-rd."))
        self.server = Server(binary, self.work)
        roots = self.work / "roots"
        roots.mkdir()
        cli = self.server.cli
        cli("setting", "set", "storage.directory-roots", f"fast={roots}")
        # A receiver that archives as received needs the installation to allow it.
        cli("setting", "set", "receiver.allow-as-received", "true")
        cli("tenant", "create", TENANT)
        key_id = self.server.key.split("_")[1]
        cli("grant", "set", key_id, "--role", "reader", "--tenant", TENANT)
        port = free_port()
        cli("setting", "set", "receiver.ports", str(port), "--tenant", TENANT)
        cli("storage", "add", TENANT, "main", "--type", "dir", "--root", "fast")
        cli("receiver", "add", TENANT, "scanner", "--storage", "main", "--port", str(port), "--aet", "ARCHIVE",
            "--allow-aet", "STORESCU", "--quiet-period", "5s", "--enabled")
        args = [storescu, "-aet", "STORESCU", "-aec", "ARCHIVE", "127.0.0.1", str(port), *[str(CORPUS / f) for f in STUDY]]
        for _ in range(20):
            done = subprocess.run(args, capture_output=True, text=True, timeout=120)
            if done.returncode == 0:
                break
            time.sleep(0.5)
        else:
            raise RuntimeError(f"storescu: {done.stdout}{done.stderr}")
        self.stored = roots / TENANT / "data"

    def wait_archived(self, client) -> None:
        """Wait until the catalog holds every instance sent: the reception ends after its quiet period."""
        from nopacs.v1 import catalog_pb2

        for _ in range(300):
            subjects = list(client.pages(client.catalog.ListSubjects, catalog_pb2.ListSubjectsRequest(tenant=TENANT), "subjects"))
            if sum(s.instance_count for s in subjects) == len(STUDY):
                return
            time.sleep(0.1)
        raise RuntimeError("the study was not archived in 30 seconds")

    def close(self) -> None:
        self.server.stop()
        shutil.rmtree(self.work, ignore_errors=True)
