#!/usr/bin/env python3
"""Check NoPACs' search answers against a reference filter over the files, read with pydicom.

The work is in nopacs_scripts.checks.search_pydicom; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import search_pydicom  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(search_pydicom.main, sys.argv[1:]))
