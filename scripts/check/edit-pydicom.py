#!/usr/bin/env python3
"""Check the revisions an edit of header values wrote, with pydicom.

The work is in nopacs_scripts.checks.edit_pydicom; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import edit_pydicom  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(edit_pydicom.main, sys.argv[1:]))
