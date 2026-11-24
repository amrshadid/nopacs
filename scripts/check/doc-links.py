#!/usr/bin/env python3
"""Fail on a link or an anchor in README.md and docs/ that leads nowhere.

The work is in nopacs_scripts.checks.doc_links; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import doc_links  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(doc_links.main, sys.argv[1:]))
