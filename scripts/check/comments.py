#!/usr/bin/env python3
"""Fail on text in the repository that points at something a reader cannot see.

The work is in nopacs_scripts.checks.comments; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import comments  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(comments.main, sys.argv[1:]))
