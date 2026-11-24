#!/usr/bin/env python3
"""Check NoPACs' plain HTTP download of instances with curl and hashlib.

The work is in nopacs_scripts.checks.download_curl; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import download_curl  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(download_curl.main, sys.argv[1:]))
