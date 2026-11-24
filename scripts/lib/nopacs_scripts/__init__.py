"""The shared code of the scripts that check NoPACs from outside.

Functions only: a function takes what it needs as arguments and returns data,
and never reads the command line or ends the process. The files under
scripts/check and scripts/prove are the entries; each one starts a main()
from here and turns its Failure into a message and an exit status
(nopacs_scripts.entry).
"""
