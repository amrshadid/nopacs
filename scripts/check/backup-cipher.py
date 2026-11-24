#!/usr/bin/env python3
"""Check NoPACs' backup file format against Python's standard library and.

The work is in nopacs_scripts.checks.backup_cipher; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import backup_cipher  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(backup_cipher.main, sys.argv[1:]))
