#!/usr/bin/env python3
"""Write synthetic DICOM files that hide identifying values where they are.

The work is in nopacs_scripts.checks.deid_adversarial; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import deid_adversarial  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(deid_adversarial.main, sys.argv[1:]))
