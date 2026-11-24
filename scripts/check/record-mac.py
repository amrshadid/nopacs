#!/usr/bin/env python3
"""Check the MAC of a NoPACs correction record against Python's standard.

The work is in nopacs_scripts.checks.record_mac; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import record_mac  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(record_mac.main, sys.argv[1:]))
