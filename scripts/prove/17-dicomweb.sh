# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "17. DICOMweb with dicomweb-client, STOW-RS included", made
# by scripts/prove/reading.py (dicomweb) on the reading
# installation of section 13.
# shellcheck shell=bash

section "17. DICOMweb with dicomweb-client, STOW-RS included"
if [ -n "$rd_pid" ]; then
	"$PYTHON" "$reading" --work "$work" dicomweb || true
else
	fail "17.0 the reading installation is there" "section 13 could not make or serve it"
fi
