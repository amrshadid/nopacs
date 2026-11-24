# Queries for the outside checks of a SQLite database, which source it after
# scripts/lib/check.sh. SQLITE3 is the sqlite3 tool; db is the file.
# shellcheck shell=bash
# shellcheck disable=SC2154 # db is set by the script that sources this.

# q runs one statement and prints its rows without headers.
q() {
	"$SQLITE3" -batch -noheader "$db" "$1"
}

# show prints a heading ($1) and the rows of a statement ($2) as a table.
show() {
	echo
	echo "== $1"
	"$SQLITE3" -batch -header -column "$db" "$2"
}
