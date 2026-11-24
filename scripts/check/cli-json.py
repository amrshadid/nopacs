#!/usr/bin/env python3
"""Check the JSON that every NoPACs command writes with --json, with Python.

The work is in nopacs_scripts.checks.cli_json; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import cli_json  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(cli_json.main, sys.argv[1:]))
