#!/usr/bin/env python3
"""Run the built nopacs binary's interactive program under a pseudo-terminal.

The work is in nopacs_scripts.checks.tui; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import tui  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(tui.main, sys.argv[1:]))
