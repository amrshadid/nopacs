#!/usr/bin/env python3
"""Dump a tenant's catalog from its database into one canonical JSON, so two.

The work is in nopacs_scripts.tools.catalog_dump; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.tools import catalog_dump  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(catalog_dump.main, sys.argv[1:]))
