#!/usr/bin/env python3
"""An archive tier in front of an S3 server, for the proofs: archived keys answer InvalidObjectState until a restore.

The work is in nopacs_scripts.tools.s3_stub; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.tools import s3_stub  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(s3_stub.main, sys.argv[1:]))
