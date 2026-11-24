#!/usr/bin/env python3
"""Compile the API contract with Python's grpcio-tools, a second toolchain.

The work is in nopacs_scripts.checks.proto_python; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import proto_python  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(proto_python.main, sys.argv[1:]))
