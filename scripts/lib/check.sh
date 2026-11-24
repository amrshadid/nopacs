# Shared by the outside checks of a database (scripts/check/*-sqlite.sh,
# *-postgres.sh), which source it: how a check fails and how it compares.
# shellcheck shell=bash

# fail prints its arguments as a failure and ends the check.
fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# want compares a value a tool printed ($3) with the one expected ($2),
# naming it ($1); a difference fails the check.
want() {
	if [ "$2" != "$3" ]; then
		fail "$1: got '$3', want '$2'"
	fi
	echo "ok: $1 = $3"
}
