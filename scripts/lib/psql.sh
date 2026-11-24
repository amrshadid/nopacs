# Queries for the outside checks of a Postgres database, which source it
# after scripts/lib/check.sh. PSQL is the psql command and its arguments
# (it may run in a container, so its version is the server's); PSQL_URL is
# the database's address.
# shellcheck shell=bash

# q runs one statement and prints its rows, unaligned and without headers.
q() {
	# shellcheck disable=SC2086 # PSQL is a command and its arguments.
	$PSQL -X -q -A -t -v ON_ERROR_STOP=1 "$PSQL_URL" -c "$1"
}

# show prints a heading ($1) and the rows of a statement ($2) as a table.
show() {
	echo
	echo "== $1"
	# shellcheck disable=SC2086
	$PSQL -X -q -v ON_ERROR_STOP=1 "$PSQL_URL" -c "$2"
}
