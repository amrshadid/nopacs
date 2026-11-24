#!/usr/bin/env python3
"""Check a tenant's DICOMweb services with dicomweb-client and pydicom.

The work is in nopacs_scripts.checks.dicomweb_client; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.checks import dicomweb_client  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(dicomweb_client.main, sys.argv[1:]))
