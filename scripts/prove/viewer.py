#!/usr/bin/env python3
"""The viewer harness: a real web viewer reads the archive through DICOMweb.

It builds and serves the real program, sets it up with its own commands (a
tenant, a directory storage, a DIMSE receiver, a reader key, a listed web
origin) and archives public studies sent by dcmtk's storescu: a CT, an MR, a
multi-frame ultrasound, and JPEG baseline, JPEG 2000 and RLE images, each as
a study of its own under new UIDs. It then starts two containers on a network
of their own, OHIF Viewer (the official ohif/app image) configured by a
generated app-config.js, and headless Chromium driven by Playwright (the
official Playwright image), which opens each study in OHIF, waits for the
viewport, takes a screenshot and records every request made to the archive.
The host reads the result with Pillow and numpy and prints, for each study,
whether its picture is blank and every request that failed (path, status,
Accept), and the most requests the viewer kept open at once. Exit status 1
when a picture is blank or a request failed.

  viewer.py --go GO --dcmtk DIR --docker-host UNIX-SOCKET-URL [--keep-shots DIR]
            [--bind ADDRESS] [--thumbnail-rendering wadors|thumbnail]
            [--omit-quotes] [--dry-run]
                                                                (make prove-viewer)

Needs Docker (the containers are named nopacs-viewer-*, removed at the end even
when the run fails), dcmtk's storescu, and a Python with pydicom, Pillow and
numpy (scripts/requirements/prove-reading.txt). Playwright itself is the
image's; no Python package is installed for it. --docker-host is a DOCKER_HOST
value, such as unix:///Users/NAME/.colima/default/docker.sock.

The browser runs in a container, so the archive must be reachable from it.
The server listens on loopback only (it refuses a wider address without TLS);
a byte relay in this script listens on --bind (0.0.0.0 by default, for a
Docker that runs in a virtual machine such as colima, whose containers cannot
reach the host's loopback) for the length of the run, and the page reaches the
archive through it as host.docker.internal, with a reader key that expires in
an hour. The relay's port is open to the network while it runs; pass the
host's own address to narrow it. The key goes to
the page as Authorization: Bearer by a few lines of app-config.js that wrap
XMLHttpRequest and fetch for the archive's address: OHIF has no setting for
a static header (see the comment on config_js).

--dry-run does everything but start containers: it serves, archives, writes
app-config.js and prints the docker commands.
"""

import argparse
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time

import pydicom
from pydicom.uid import generate_uid

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
# The repository's own corpus, then pydicom's test_files (PYDICOM_CORPUS names
# the whole directory; the installed pydicom lacks some files).
CORPUS = [os.path.join(ROOT, "internal", "receive", "testdata"),
          os.environ.get("PYDICOM_CORPUS") or os.path.join(os.path.dirname(pydicom.__file__), "data", "test_files")]

OHIF_IMAGE = "ohif/app:v3.13.15"
PLAYWRIGHT_IMAGE = "mcr.microsoft.com/playwright/python:v1.49.0-noble"
# The image carries the browsers that release drives, not the Python package:
# the same release is installed in the container before the driver runs.
PLAYWRIGHT_PACKAGE = "playwright==1.49.0"
TENANT = "viewer"
QUIET = "5s"

# One study per file: name, corpus file, what it covers.
STUDIES = [
    ("ct", "CT_small.dcm", "CT, native"),
    ("mr", "MR_small.dcm", "MR, native"),
    ("multiframe", "jpeg_3frame_nobot.dcm", "multi-frame ultrasound, JPEG baseline"),
    ("jpeg-baseline", "SC_rgb_jpeg_dcmtk.dcm", "color, JPEG baseline"),
    ("jpeg2000", "MR_small_jp2klossless.dcm", "MR, JPEG 2000 lossless"),
    ("rle", "MR_small_RLE.dcm", "MR, RLE"),
]
# The storescu option that proposes a compressed transfer syntax, by its UID.
PROPOSE = {"1.2.840.10008.1.2.5": "-xr", "1.2.840.10008.1.2.4.50": "-xy", "1.2.840.10008.1.2.4.90": "-xv"}


def corpus_path(name):
    for d in CORPUS:
        if os.path.exists(os.path.join(d, name)):
            return os.path.join(d, name)
    raise SystemExit("viewer: %s is in none of %s" % (name, CORPUS))


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def config_js(api_root, key, thumbnail_rendering, omit_quotes):
    """OHIF's app-config.js. OHIF 3's DICOMweb data source takes its headers
    from userAuthenticationService.getAuthorizationHeader(), which only an
    OpenID Connect login implements; the data source's configuration has no
    key for a static header (extensions/default/src/DicomWebDataSource/
    index.ts: `headers: userAuthenticationService.getAuthorizationHeader()`,
    reassigned before every QIDO, WADO and STOW request;
    platform/core/src/services/UserAuthenticationService: the default
    implementation returns nothing). So the key is added where every request
    passes, in the browser: the page's XMLHttpRequest and fetch, for the
    archive's address only. The request is the viewer's own, so the CORS
    preflight a browser makes for a request with Authorization is exercised."""
    cfg = {
        "routerBasename": "/",
        # OHIF 3 iterates both lists at start; empty means only the
        # extensions and modes the image is built with.
        "extensions": [],
        "modes": [],
        "showStudyList": True,
        "maxNumRequests": {"interaction": 100, "thumbnail": 75, "prefetch": 25},
        "defaultDataSourceName": "nopacs",
        "dataSources": [{
            "namespace": "@ohif/extension-default.dataSourcesModule.dicomweb",
            "sourceName": "nopacs",
            "configuration": {
                "friendlyName": "NoPACs",
                "name": "nopacs",
                "wadoUriRoot": api_root,
                "qidoRoot": api_root,
                "wadoRoot": api_root,
                "qidoSupportsIncludeField": False,
                "imageRendering": "wadors",
                "thumbnailRendering": thumbnail_rendering,
                "enableStudyLazyLoad": True,
                "supportsFuzzyMatching": False,
                "supportsWildcard": True,
                "omitQuotationForMultipartRequest": omit_quotes,
                "bulkDataURI": {"enabled": True},
            },
        }],
    }
    return """(function () {
  var root = %s, auth = %s;
  var open = XMLHttpRequest.prototype.open, send = XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.open = function (m, u) { this._nopacsUrl = String(u); return open.apply(this, arguments); };
  XMLHttpRequest.prototype.send = function () {
    if (this._nopacsUrl && this._nopacsUrl.indexOf(root.split('/tenants/')[0]) === 0) this.setRequestHeader('Authorization', auth);
    return send.apply(this, arguments);
  };
  var f = window.fetch;
  window.fetch = function (input, init) {
    var u = typeof input === 'string' ? input : input.url;
    if (u && u.indexOf(root.split('/tenants/')[0]) === 0) {
      init = Object.assign({}, init); var h = new Headers(init.headers || (typeof input === 'string' ? undefined : input.headers));
      h.set('Authorization', auth); init.headers = h;
    }
    return f.call(this, input, init);
  };
})();
window.config = %s;
""" % (json.dumps(api_root), json.dumps("Bearer " + key), json.dumps(cfg, indent=2))


class Relay:
    """A byte relay from an address the browser container can reach to the
    server's loopback listener. The server refuses to listen beyond loopback
    without TLS (keys would cross the network in clear text), and a Docker in
    a virtual machine cannot reach the host's loopback, so the relay stands
    between; the server still sees every caller as loopback."""

    def __init__(self, bind, target_port):
        import socketserver
        import threading
        target = ("127.0.0.1", target_port)

        class Handler(socketserver.BaseRequestHandler):
            def handle(self):
                try:
                    up = socket.create_connection(target, timeout=30)
                except OSError:
                    return

                def pipe(a, b):
                    try:
                        while True:
                            d = a.recv(65536)
                            if not d:
                                break
                            b.sendall(d)
                    except OSError:
                        pass
                    finally:
                        try:
                            b.shutdown(socket.SHUT_WR)
                        except OSError:
                            pass

                t = threading.Thread(target=pipe, args=(up, self.request), daemon=True)
                t.start()
                pipe(self.request, up)
                t.join(60)
                up.close()

        class Server(socketserver.ThreadingTCPServer):
            daemon_threads = True
            allow_reuse_address = True

        self.server = Server((bind, 0), Handler)
        self.port = self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class Archive:
    """The server, the way sending.py runs one: a work directory, the binary,
    the command line on the local socket."""

    def __init__(self, args):
        self.args = args
        # Short: the local socket's path must fit the 104 bytes macOS allows.
        self.work = tempfile.mkdtemp(prefix="nopacs-view.", dir="/tmp")
        os.chmod(self.work, 0o700)
        self.data = os.path.join(self.work, "data")
        self.bin = os.path.join(self.work, "nopacs")
        self.sbin = os.path.join(self.work, "nopacs-server")
        self.roots = os.path.join(self.work, "roots", "fast")
        self.port = free_port()
        self.server = None

    def exe(self, *args):
        """The program a command belongs to: the commands that touch the data directory with the server stopped are nopacs-server's."""
        server = args[:1] in (("init",), ("backup",)) or args[:2] == ("tenant", "import")
        return self.sbin if server else self.bin

    def run(self, *a):
        return subprocess.run([self.exe(*a), "--data-dir", self.data, *a], capture_output=True, text=True, timeout=120)

    def m(self, *a):
        p = self.run(*a)
        if p.returncode != 0:
            raise SystemExit("nopacs %s: exit %d: %s" % (" ".join(a), p.returncode, p.stderr.strip()))
        return p.stdout

    def serve(self):
        log = open(os.path.join(self.work, "serve.log"), "a")
        self.server = subprocess.Popen([self.sbin, "--data-dir", self.data, "serve", "--listen", "127.0.0.1:%d" % self.port],
                                       stdout=log, stderr=log)
        for _ in range(300):
            if self.server.poll() is not None:
                raise SystemExit("the server exited at its start; see %s" % os.path.join(self.work, "serve.log"))
            if self.run("whoami").returncode == 0:
                return
            time.sleep(0.1)
        raise SystemExit("the server did not answer")

    def stop(self):
        if self.server and self.server.poll() is None:
            self.server.terminate()
            try:
                self.server.wait(60)
            except subprocess.TimeoutExpired:
                self.server.kill()
                self.server.wait()

    def wait_up(self, receiver, timeout=30):
        deadline = time.time() + timeout
        while time.time() < deadline:
            p = self.run("--json", "receiver", "show", TENANT, receiver)
            if p.returncode == 0 and json.loads(p.stdout).get("receiver", {}).get("health", {}).get("state") == "RECEIVER_STATE_UP":
                return
            time.sleep(0.2)
        raise SystemExit("the receiver %s did not come up" % receiver)

    def setup(self, origin):
        subprocess.run([self.args.go, "build", "-o", self.bin, os.path.join(ROOT, "cmd", "nopacs")], check=True, cwd=ROOT)
        subprocess.run([self.args.go, "build", "-o", self.sbin, os.path.join(ROOT, "cmd", "nopacs-server")], check=True, cwd=ROOT)
        self.m("init")
        self.serve()
        os.makedirs(self.roots)
        self.m("setting", "set", "storage.directory-roots", "fast=" + self.roots)
        self.m("setting", "set", "api.cors-origins", origin)
        self.m("tenant", "create", TENANT)
        dimse = free_port()
        self.m("setting", "set", "receiver.ports", str(dimse), "--tenant", TENANT)
        self.m("setting", "set", "receiver.allow-as-received", "true", "--tenant", TENANT)
        self.m("storage", "add", TENANT, "main", "--type", "dir", "--root", "fast", "--path", "main")
        self.m("receiver", "add", TENANT, "scanner", "--storage", "main", "--port", str(dimse), "--aet", "ARCHIVE",
               "--quiet-period", QUIET, "--allow-aet", "STORESCU", "--as-received", "--enabled")
        self.wait_up("scanner")
        out = self.m("key", "create", "--holder", "viewer harness", "--expires", "1h")
        key = out.splitlines()[0].strip()
        self.m("grant", "set", key.split("_")[1], "--role", "reader", "--tenant", TENANT)
        return key, dimse

    def catalog(self):
        return os.path.join(self.data, "tenants", TENANT + ".db")


def make_studies(workdir):
    """Each corpus file written as a study of its own under new UIDs (the
    public files share some), keeping its pixels as encoded; returns the
    paths and the study UIDs by name."""
    out = {}
    for name, fname, _ in STUDIES:
        ds = pydicom.dcmread(corpus_path(fname))
        ds.StudyInstanceUID, ds.SeriesInstanceUID = generate_uid(), generate_uid()
        ds.SOPInstanceUID = ds.file_meta.MediaStorageSOPInstanceUID = generate_uid()
        path = os.path.join(workdir, name + ".dcm")
        if int(pydicom.__version__.split(".")[0]) >= 3:
            ds.save_as(path, enforce_file_format=True)
        else:
            ds.save_as(path, write_like_original=False)
        out[name] = (path, str(ds.StudyInstanceUID), str(ds.file_meta.TransferSyntaxUID))
    return out


def send(args, dimse, studies):
    """Sends each file with storescu, proposing its own transfer syntax;
    returns the number answered success."""
    tool = os.path.join(args.dcmtk, "storescu") if args.dcmtk else "storescu"
    ok = 0
    for name, (path, _, ts) in studies.items():
        opt = [PROPOSE[ts]] if ts in PROPOSE else []
        p = subprocess.run([tool, "-v", "-R", *opt, "-aet", "STORESCU", "-aec", "ARCHIVE", "127.0.0.1", str(dimse), path],
                           capture_output=True, text=True, timeout=300)
        n = (p.stdout + p.stderr).count("Received Store Response (Success)")
        if n != 1:
            raise SystemExit("storescu %s: not answered success:\n%s" % (name, (p.stdout + p.stderr)[-1500:]))
        ok += n
    return ok


def wait_archived(arch, studies, timeout=180):
    import sqlite3
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            db = sqlite3.connect("file:%s?mode=ro" % arch.catalog(), uri=True)
            rows = db.execute("SELECT study_uid, ended FROM receptions").fetchall()
            db.close()
            done = {r[0] for r in rows if r[1]}
            if all(s[1] in done for s in studies.values()):
                return
        except sqlite3.Error:
            pass
        time.sleep(0.3)
    raise SystemExit("the receptions did not all end within %ds" % timeout)


class Docker:
    """The docker command with the chosen host; every container and network
    it makes is removed by close()."""

    def __init__(self, host, suffix, dry):
        self.env = dict(os.environ)
        if host:
            self.env["DOCKER_HOST"] = host
        self.suffix, self.dry = suffix, dry
        self.containers, self.networks = [], []

    def run(self, *a, check=True, timeout=600):
        cmd = ["docker", *a]
        print("     $ " + " ".join(cmd), flush=True)
        if self.dry:
            return subprocess.CompletedProcess(cmd, 0, "", "")
        p = subprocess.run(cmd, capture_output=True, text=True, env=self.env, timeout=timeout)
        if check and p.returncode != 0:
            raise SystemExit("docker %s: exit %d: %s" % (" ".join(a[:2]), p.returncode, (p.stderr or p.stdout).strip()[-1500:]))
        return p

    def require(self):
        if shutil.which("docker") is None:
            raise SystemExit("viewer: docker is not installed (or not on PATH); this harness runs OHIF and Chromium in containers")
        p = subprocess.run(["docker", "info"], capture_output=True, text=True, env=self.env, timeout=60)
        if p.returncode != 0:
            raise SystemExit("viewer: docker does not answer (DOCKER_HOST=%s): %s\n  start it (colima start) and pass --docker-host "
                             "unix:///Users/NAME/.colima/default/docker.sock" % (self.env.get("DOCKER_HOST", "(unset)"), p.stderr.strip()[-400:]))

    def name(self, what):
        return "nopacs-viewer-%s-%s" % (what, self.suffix)

    def close(self):
        if self.dry:
            return
        for c in self.containers:
            subprocess.run(["docker", "rm", "-f", c], capture_output=True, env=self.env)
            print("     removed container %s" % c, flush=True)
        for n in self.networks:
            subprocess.run(["docker", "network", "rm", n], capture_output=True, env=self.env)
            print("     removed network %s" % n, flush=True)


def analyse(png):
    """Variance of the grey levels and the share of pixels that are not
    near-black, of a screenshot: a blank viewport has neither (the overlay's
    text alone is a small share)."""
    import numpy as np
    from PIL import Image
    a = np.asarray(Image.open(png).convert("L"), dtype=float)
    return float(a.var()), float((a > 16).mean())


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--go", default="go")
    ap.add_argument("--dcmtk", default="")
    ap.add_argument("--docker-host", default="")
    ap.add_argument("--bind", default="0.0.0.0")
    ap.add_argument("--keep-shots", default="", help="a directory the screenshots and result.json are copied to")
    ap.add_argument("--thumbnail-rendering", default="wadors", choices=["wadors", "thumbnail"])
    ap.add_argument("--omit-quotes", action="store_true")
    ap.add_argument("--timeout", type=int, default=90, help="seconds one study may take to draw")
    ap.add_argument("--min-variance", type=float, default=100.0)
    ap.add_argument("--min-lit", type=float, default=0.10, help="the least share of non-black pixels")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    suffix = "%d" % (int(time.time()) % 100000)
    docker = Docker(args.docker_host, suffix, args.dry_run)
    if not args.dry_run:
        docker.require()
    # Docker's bind mounts must lie where the virtual machine shares the
    # host's files: the home directory, not /tmp.
    share = tempfile.mkdtemp(prefix="nopacs-viewer.", dir=os.path.expanduser("~"))
    arch = Archive(args)
    failures, relay = [], None
    try:
        viewer_origin = "http://" + docker.name("ohif")
        key, dimse = arch.setup(viewer_origin)
        studies = make_studies(share)
        n = send(args, dimse, studies)
        wait_archived(arch, studies)
        print("ok   archived %d studies (%s) in tenant %s; the server listens on 127.0.0.1:%d; origin %s listed" % (
            n, ", ".join(studies), TENANT, arch.port, viewer_origin), flush=True)

        relay = Relay(args.bind, arch.port)
        host = "host.docker.internal"
        api_root = "http://%s:%d/tenants/%s/dicom-web" % (host, relay.port, TENANT)
        with open(os.path.join(share, "app-config.js"), "w") as f:
            f.write(config_js(api_root, key, args.thumbnail_rendering, args.omit_quotes))
        shutil.copy(os.path.join(HERE, "viewer_driver.py"), share)
        plan = {"viewer": viewer_origin, "api": api_root, "key": key, "timeout": args.timeout,
                "studies": [{"name": k, "uid": v[1]} for k, v in studies.items()]}
        with open(os.path.join(share, "plan.json"), "w") as f:
            json.dump(plan, f)
        os.chmod(os.path.join(share, "plan.json"), 0o600)

        net = docker.name("net")
        docker.run("network", "create", net)
        docker.networks.append(net)
        ohif = docker.name("ohif")
        docker.containers.append(ohif)
        docker.run("run", "-d", "--name", ohif, "--network", net, "-v", "%s/app-config.js:/usr/share/nginx/html/app-config.js:ro" % share,
                   OHIF_IMAGE)
        time.sleep(0 if args.dry_run else 3)
        pw = docker.name("browser")
        docker.containers.append(pw)
        p = docker.run("run", "--name", pw, "--network", net, "--add-host", "%s:host-gateway" % host, "--ipc=host",
                       "-v", "%s:/out" % share, PLAYWRIGHT_IMAGE, "sh", "-c",
                       "pip install -q --disable-pip-version-check %s && python /out/viewer_driver.py /out" % PLAYWRIGHT_PACKAGE,
                       check=False, timeout=1800)
        if args.dry_run:
            print("dry run: no container started; app-config.js is %s (removed with the work directory)" % os.path.join(share, "app-config.js"))
            return 0
        if p.returncode != 0:
            raise SystemExit("the browser container failed (exit %d): %s" % (p.returncode, (p.stderr or p.stdout).strip()[-1500:]))
        with open(os.path.join(share, "result.json")) as f:
            res = json.load(f)
        if not res["reachable"]:
            raise SystemExit("the archive is not reachable from the browser container at %s: %s\n  the relay listens on %s; check "
                             "that host.docker.internal is the host for this Docker, and the host's firewall" % (
                                 api_root, res["reachable_error"], args.bind))

        print("browser %s, viewer %s, %s" % (res.get("browser", "?"), OHIF_IMAGE, PLAYWRIGHT_IMAGE))
        peak = 0
        for s in res["studies"]:
            if "error" in s:
                failures.append(s["name"])
                print("FAIL %-14s %s" % (s["name"], s["error"]))
                continue
            var, lit = analyse(os.path.join(share, "shots", s["shot"])) if s["shot"] else (0.0, 0.0)
            bad = [r for r in s["requests"] if r["failed"] or (r["status"] or 0) >= 400 or r["status"] is None]
            peak = max(peak, s["peak_concurrent"])
            good = s["shot"] != "" and var > args.min_variance and lit > args.min_lit
            if not good or bad:
                failures.append(s["name"])
            print("%-4s %-14s canvas variance %.0f, lit %.0f%%, %d requests, peak %d at once, drawn after %.1fs" % (
                "ok" if good and not bad else "FAIL", s["name"], var, lit * 100, len(s["requests"]), s["peak_concurrent"], s["waited"]))
            for r in bad:
                print("       failed: %s %s -> %s %s (Accept: %s)" % (r["method"], r["url"], r["status"], r["failed"], r["accept"]))
            for c in s["console_errors"][:5]:
                print("       console: %s" % c)
        print("peak concurrent reads to the archive over all studies: %d" % peak)
        if args.keep_shots:
            os.makedirs(args.keep_shots, exist_ok=True)
            shutil.copytree(os.path.join(share, "shots"), os.path.join(args.keep_shots, "shots"), dirs_exist_ok=True)
            shutil.copy(os.path.join(share, "result.json"), args.keep_shots)
            print("screenshots and result.json kept in %s" % args.keep_shots)
    finally:
        docker.close()
        if relay:
            relay.close()
        arch.stop()
        shutil.rmtree(share, ignore_errors=True)
        shutil.rmtree(arch.work, ignore_errors=True)
    if failures:
        print("FAIL studies: %s" % ", ".join(failures))
        return 1
    print("ok   every study drew and no request failed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
