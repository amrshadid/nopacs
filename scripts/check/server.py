#!/usr/bin/env python3
"""The server checked by Python's grpcio, a gRPC implementation that is not Go.

The work is in nopacs_scripts.checks.server; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import server  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(server.main, sys.argv[1:]))
