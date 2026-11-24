#!/usr/bin/env python3
"""The proof that the archive is kept whole, from outside the binary.

The work is in nopacs_scripts.sections.whole; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.sections import whole  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(whole.main, sys.argv[1:]))
