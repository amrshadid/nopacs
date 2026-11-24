"""Starting the programs under proof."""

import os
import subprocess

from nopacs_scripts.paths import ROOT


def build_binaries(go, command_line, server, cwd=None):
    """Builds cmd/nopacs to the path command_line and cmd/nopacs-server to
    the path server with the Go tool named go, as make build does but for this
    machine only; raises CalledProcessError when a build fails."""
    subprocess.run([go, "build", "-o", command_line, os.path.join(ROOT, "cmd", "nopacs")], check=True, cwd=cwd)
    subprocess.run([go, "build", "-o", server, os.path.join(ROOT, "cmd", "nopacs-server")], check=True, cwd=cwd)
