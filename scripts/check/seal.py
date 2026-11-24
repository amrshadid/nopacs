#!/usr/bin/env python3
"""Check NoPACs' sealed-secret format against Python's cryptography package.

The work is in nopacs_scripts.checks.seal; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import seal  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(seal.main, sys.argv[1:]))
