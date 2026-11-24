#!/usr/bin/env python3
"""Open a NoPACs backup file outside NoPACs: with the installation's key file,.

The work is in nopacs_scripts.tools.backup_open; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.tools import backup_open  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(backup_open.main, sys.argv[1:]))
