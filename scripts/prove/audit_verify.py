#!/usr/bin/env python3
"""Verify an exported audit log without NoPACs: the chain with hashlib, each.

The work is in nopacs_scripts.tools.audit_verify; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.tools import audit_verify  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(audit_verify.main, sys.argv[1:]))
