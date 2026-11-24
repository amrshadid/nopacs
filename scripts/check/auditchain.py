#!/usr/bin/env python3
"""Check NoPACs' audit chain against Python's hashlib and openssl.

The work is in nopacs_scripts.checks.auditchain; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import auditchain  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(auditchain.main, sys.argv[1:]))
