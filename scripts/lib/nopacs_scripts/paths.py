"""Where the repository and its scripts are, found from this file's place so
that a script works from any current directory."""

import os

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
SCRIPTS = os.path.join(ROOT, "scripts")
PROVE = os.path.join(SCRIPTS, "prove")
LIB = os.path.join(SCRIPTS, "lib")
