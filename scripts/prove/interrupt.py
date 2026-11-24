#!/usr/bin/env python3
"""The interruption harness: the server killed at each named durable step of
a correction and a rebuild, and judged from outside after each restart.

The work is in nopacs_scripts.sections.interrupt; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.sections import interrupt  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(interrupt.main, sys.argv[1:]))
