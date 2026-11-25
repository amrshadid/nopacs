#!/usr/bin/env python3
"""Compare two storage trees (directories or buckets) by key, size and SHA-256.

The work is in nopacs_scripts.tools.tree_compare; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.tools import tree_compare  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(tree_compare.main, sys.argv[1:]))
