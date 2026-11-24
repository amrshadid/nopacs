#!/usr/bin/env python3
"""The sections of scripts/prove/run.sh that read the archive, from outside.

The work is in nopacs_scripts.sections.reading; this file only starts it.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "lib"))
from nopacs_scripts import entry  # noqa: E402
from nopacs_scripts.sections import reading  # noqa: E402

if __name__ == "__main__":
    sys.exit(entry.run(reading.main, sys.argv[1:]))
