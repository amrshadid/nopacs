# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "15. metadata equals pydicom's to_json_dict", made
# by scripts/prove/reading.py (metadata) on the reading
# installation of section 13.
# shellcheck shell=bash

section "15. metadata equals pydicom's to_json_dict"
if [ -n "$rd_pid" ]; then
	"$PYTHON" "$reading" --work "$work" metadata || true
else
	fail "15.0 the reading installation is there" "section 13 could not make or serve it"
fi
