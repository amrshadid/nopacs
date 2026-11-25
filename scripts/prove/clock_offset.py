#!/usr/bin/env python3
"""Move the time of a NoPACs server built for testing (the build tag clock).

The work is in nopacs_scripts.tools.clock_offset; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.tools import clock_offset  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(clock_offset.main, sys.argv[1:]))
