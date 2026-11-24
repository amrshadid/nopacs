#!/usr/bin/env python3
"""Fails when reception got slower than on a base commit.

The work is in nopacs_scripts.checks.bench_gate; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import bench_gate  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(bench_gate.main, sys.argv[1:]))
