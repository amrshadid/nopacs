#!/usr/bin/env python3
"""Check a profile's text lists in de-identified files with pydicom.

The work is in nopacs_scripts.checks.deid_lists_pydicom; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import deid_lists_pydicom  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(deid_lists_pydicom.main, sys.argv[1:]))
