#!/usr/bin/env bash
# The proof of this version: it shows, from outside, that the installed binary
# does what this version of NoPACs claims (an installation made and served,
# keys, grants and tenants on SQLite and Postgres, refusals, secrets kept
# sealed, crash safety, the audit log, the local socket, both command line
# forms and the static builds, and reading the archive: the catalog, the
# headers, downloads, DICOMweb and STOW-RS, the Python client and refusals
# on every way in to what a tenant holds). It builds the real binary as "make build"
# does, starts it, and checks each claim with tools that are not NoPACs.
# Nothing here reaches into a Go package: it drives the installed thing from outside, with the binary's own commands,
# grpcurl, curl, openssl, sqlite3, psql, pg_dump and a Python grpcio client
# (scripts/prove/steps.py).
#
# Each claim is a named check that prints one line, ok or FAIL with its
# evidence; a failed check does not stop the others. The run ends with a table
# of every check and the numbers it measured, and exits 1 when any check
# failed (or, with NOPACS_PROVE_STRICT=1, as in CI, when any was skipped).
#
#   1  install and run       6  a tenant stops alone
#   2  the first key         7  nothing half-made after a crash
#   3  tenants on both DBs   8  audited
#   4  refused everywhere    9  one server, one user
#   5  hashed and sealed    10  both forms of the command line
#                           11  the static builds and their sizes
#                           12  where a key may be used from
#
#  13  a reading installation, built by sending public studies with storescu
#  14  the catalog equals pydicom's reading
#  15  metadata equals pydicom's to_json_dict
#  16  downloads whole, in ranges, and resumed
#  17  DICOMweb with dicomweb-client, STOW-RS included
#  18  the Python client against the real server
#  19  reading is closed by default
#
# Needs: go, make, curl, openssl, docker (for Postgres), sqlite3 (SQLITE3=...),
# grpcurl (GRPCURL=...), and a Python (PYTHON=...) with
# scripts/requirements/check-server.txt installed. Bash 3.2 or later.
# Sections 13 to 19 also need dcmtk's storescu (DCMTK=its directory, since
# pynetdicom installs a program of the same name) and
# scripts/requirements/prove-reading.txt in that Python; without them they
# are skipped, and say so.
#
# Postgres: by default the script starts its own container (image
# NOPACS_PROVE_PG_IMAGE, postgres:18) and removes it at the end. With
# NOPACS_PROVE_PG_CONTAINER (a running container, such as CI's service
# container) and NOPACS_PROVE_PG_PORT (its port on 127.0.0.1) it uses that
# one, makes its own role and databases in it, and drops them at the end. In
# both cases the container is stopped and started again for section 6, and psql
# and pg_dump are run inside it, so their version is the server's.
#
# The second operating system user of section 9 is made with sudo on Linux; on a
# system where that cannot be done the check says so and is skipped there only.
#
#   make prove PYTHON=/path/to/venv/bin/python GRPCURL=grpcurl
#
# NOPACS_PROVE_BUILD=host builds only this machine's binary, for a quicker
# run while working on the script; check 11 is then skipped.
# NOPACS_PROVE_ONLY=reading runs the builds and sections 13 to 19 alone,
# without Docker: the reading tenant on Postgres is then skipped.
set -euo pipefail

if [ "${BASH_VERSINFO[0]}" -lt 3 ] || { [ "${BASH_VERSINFO[0]}" -eq 3 ] && [ "${BASH_VERSINFO[1]}" -lt 2 ]; }; then
	echo "prove: needs bash 3.2 or later, this is $BASH_VERSION" >&2
	exit 2
fi

GO=${GO:-go}
PYTHON=${PYTHON:-python3}
GRPCURL=${GRPCURL:-grpcurl}
SQLITE3=${SQLITE3:-sqlite3}
DOCKER=${DOCKER:-docker}
PG_CONTAINER=${NOPACS_PROVE_PG_CONTAINER:-}
PG_PORT=${NOPACS_PROVE_PG_PORT:-}
PG_IMAGE=${NOPACS_PROVE_PG_IMAGE:-postgres:18}
STRICT=${NOPACS_PROVE_STRICT:-0}
BUILD=${NOPACS_PROVE_BUILD:-all}
ONLY=${NOPACS_PROVE_ONLY:-}
DCMTK=${DCMTK:-}
STORESCU=${DCMTK:+$DCMTK/}storescu
# At least 20 kills, so that a creation interrupted at different moments is
# seen more than once; more give the adaptive delays
# more chances to land inside the creation.
CRASHES=${NOPACS_PROVE_CRASHES:-24}

root=$(cd "$(dirname "$0")/../.." && pwd)
helper=$root/scripts/prove/steps.py
started_at=$(date +%s)

case $ONLY in
	'' | reading) ;;
	*) echo "prove: NOPACS_PROVE_ONLY is '$ONLY'; it takes only 'reading'" >&2; exit 2 ;;
esac
needs=("$GO" make curl openssl "$SQLITE3" "$GRPCURL" "$PYTHON")
[ "$ONLY" = reading ] || needs+=("$DOCKER")
for tool in "${needs[@]}"; do
	command -v "$tool" > /dev/null || { echo "prove: $tool is not installed (see the header of this script)" >&2; exit 2; }
done
# What only the reading sections need. Without it they are skipped, and
# said to be, so NOPACS_PROVE_STRICT=1 fails; asked for alone, the run stops.
reading_lacks=""
case $("$STORESCU" --version 2> /dev/null || true) in
	*dcmtk*) ;;
	*) reading_lacks="$STORESCU is not dcmtk's storescu (DCMTK= names dcmtk's directory)" ;;
esac
"$PYTHON" -c 'import pydicom, dicomweb_client, requests' 2> /dev/null ||
	reading_lacks="${reading_lacks:+$reading_lacks; }$PYTHON lacks pydicom, dicomweb-client or requests (scripts/requirements/prove-reading.txt)"
if [ -n "$reading_lacks" ] && [ "$ONLY" = reading ]; then
	echo "prove: $reading_lacks" >&2
	exit 2
fi
"$PYTHON" -c 'import grpc, prometheus_client' 2> /dev/null || {
	echo "prove: $PYTHON lacks grpcio or prometheus-client; install scripts/requirements/check-server.txt" >&2
	exit 2
}

# A short directory: the local socket's path must fit the 104 bytes a Unix
# socket address holds on macOS, so the system's temporary directory (long on
# macOS) is not used.
work=$(mktemp -d /tmp/nopacs-proof.XXXXXX)
chmod 700 "$work"
: > "$work/results.tsv"
: > "$work/ledger.tsv"
: > "$work/secrets"
: > "$work/search"
mkdir -m 700 "$work/keys"

# --- Cleanup: everything the run made goes, on success, failure or a signal.
pids=""
own_pg=""
pg_made=""
user_made=""
summary_done=0
cleanup() {
	local rc=$?
	set +e
	trap - EXIT INT TERM
	for p in $pids; do kill -TERM "$p" 2> /dev/null; done
	sleep 0.3
	for p in $pids; do kill -KILL "$p" 2> /dev/null; wait "$p" 2> /dev/null; done
	if [ "$summary_done" = 0 ]; then
		echo "prove: stopped before the end; the checks run so far:"
		summary
		[ "$rc" != 0 ] || rc=1
	fi
	if [ -n "$own_pg" ]; then
		"$DOCKER" rm -f "$own_pg" > /dev/null 2>&1
	elif [ -n "$pg_made" ] && [ -n "$PG_CONTAINER" ]; then
		# A container that is not ours is left running as it was found, with
		# what this run made in it dropped.
		"$DOCKER" start "$PG_CONTAINER" > /dev/null 2>&1
		wait_pg > /dev/null 2>&1
		for db in $("$DOCKER" exec "$PG_CONTAINER" psql -U postgres -qtAc "select datname from pg_database where datname like 'nopacs_proof%'" 2> /dev/null); do
			"$DOCKER" exec "$PG_CONTAINER" psql -U postgres -qc "drop database if exists $db with (force)" > /dev/null 2>&1
		done
		"$DOCKER" exec "$PG_CONTAINER" psql -U postgres -qc "drop role if exists nopacs_proof" > /dev/null 2>&1
	fi
	if [ -n "$user_made" ]; then sudo -n userdel "$user_made" > /dev/null 2>&1; fi
	rm -rf "$work"
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
track() { pids="$pids $1"; }
untrack() {
	local p out=""
	for p in $pids; do [ "$p" = "$1" ] || out="$out $p"; done
	pids=$out
}

# --- Output: one line per check. A secret is never printed: every text goes
# through redact, which shows a secret as its length and a fingerprint.
sha256hex() {
	if command -v sha256sum > /dev/null; then sha256sum | cut -d ' ' -f 1; else shasum -a 256 | cut -d ' ' -f 1; fi
}
fingerprint() { printf '%s bytes, sha256 %s...' "${#1}" "$(printf %s "$1" | sha256hex | cut -c 1-8)"; }
redact() {
	local text=$1 s
	while IFS= read -r s; do
		[ -n "$s" ] || continue
		case $text in *"$s"*) text=${text//"$s"/<secret, $(fingerprint "$s")>} ;; esac
	done < "$work/secrets"
	printf '%s' "$text"
}
# secret VALUE [search]: remember a secret, for redaction, and for the search
# of section 5 when the second argument is "search".
secret() {
	printf '%s\n' "$1" >> "$work/secrets"
	if [ "${2:-}" = search ]; then printf '%s\n' "$1" >> "$work/search"; fi
}
result() {
	local name=$1 status=$2 detail ev
	detail=$(redact "$3")
	shift 3
	printf '%-4s %s: %s\n' "$status" "$name" "$detail"
	if [ "$status" = FAIL ]; then
		for ev in "$@"; do redact "$ev" | head -20 | sed 's/^/     | /'; echo; done
	fi
	printf '%s\t%s\t%s\n' "$name" "$status" "$(printf %s "$detail" | tr '\t\n' '  ')" >> "$work/results.tsv"
}
ok() { result "$1" ok "$2"; }
fail() {
	local n=$1 d=$2
	shift 2
	result "$n" FAIL "$d" "$@"
}
skip() { result "$1" skipped "$2"; }
section() { printf '\n== %s\n' "$1"; }

summary() {
	local n_ok n_fail n_skip
	n_ok=$(awk -F '\t' '$2 == "ok"' "$work/results.tsv" | wc -l | tr -d ' ')
	n_fail=$(awk -F '\t' '$2 == "FAIL"' "$work/results.tsv" | wc -l | tr -d ' ')
	n_skip=$(awk -F '\t' '$2 == "skipped"' "$work/results.tsv" | wc -l | tr -d ' ')
	echo
	echo "| Check | Result | Measured |"
	echo "|---|---|---|"
	awk -F '\t' '{ gsub(/\|/, "/", $3); printf "| %s | %s | %s |\n", $1, $2, $3 }' "$work/results.tsv"
	echo
	if [ -s "$work/sizes.txt" ]; then cat "$work/sizes.txt"; echo; fi
	echo "$n_ok ok, $n_fail FAIL, $n_skip skipped, in $(($(date +%s) - started_at)) s on $(uname -s) $(uname -m)"
}

mode_of() { stat -c '%a' "$1" 2> /dev/null || stat -f '%Lp' "$1"; }
free_port() { "$PYTHON" -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }
now() { "$PYTHON" -c 'import time; print("%.6f" % time.time())'; }
urlescape() { "$PYTHON" -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"; }
json_get() { "$PYTHON" -c 'import json, sys; d = json.load(sys.stdin); print(eval("d" + sys.argv[1]))' "$1"; }
# median_max FILE: the median and the largest of the numbers in FILE, in ms.
median_max() { sort -n "$1" | awk '{ v[NR] = $1 * 1000 } END { printf "%.1f %.1f", v[int((NR + 1) / 2)], v[NR] }'; }

# --- The request ledger: every request made to the main server, with the
# request id from its response header when the client shows it, so section 8
# can find each one in the audit log, and the log can be reconciled against
# it the other way.
# The seventh field, for a request without an id, is the second it was sent
# in and the second it ended in, so it is matched only to an entry of that
# time.
ledger() { printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "${1:--}" "$2" "$3" "$4" "$5" "$6" "${7:-}" >> "$work/ledger.tsv"; }
outcome_of() {
	case $1 in
		200) echo 'done' ;;
		401 | 403) echo refused ;;
		*) echo failed ;;
	esac
}

# api VIA KEY METHOD BODY [TENANT]: one call by curl, as JSON over HTTP, on
# TLS (VIA tls) or the local socket (VIA sock), with the key named KEY (a file
# under keys/) or none (""). Sets API_STATUS, API_TIME (s), API_BODY, API_RID.
# The key goes in a header file, never in curl's arguments.
api() {
	local via=$1 key=$2 method=$3 body=$4 tenant=${5:-*} out url key_id=""
	local args=(-sS -o "$work/api.body" -D "$work/api.head" -w '%{http_code} %{time_total}' -H 'Content-Type: application/json' -d "$body")
	if [ "$via" = tls ]; then
		url=https://127.0.0.1:$port/$method
		args+=(--cacert "$work/cert.pem")
	else
		url=http://local/$method
		args+=(--unix-socket "$sock")
		key_id=local
	fi
	if [ -n "$key" ]; then
		(umask 077 && printf 'Authorization: Bearer %s\n' "$(cat "$work/keys/$key")" > "$work/auth.h")
		args+=(-H "@$work/auth.h")
		key_id=$(cat "$work/keys/$key.id")
	fi
	: > "$work/api.body"
	: > "$work/api.head"
	out=$(curl "${args[@]}" "$url" 2> "$work/api.err") || out="000 0"
	rm -f "$work/auth.h"
	API_STATUS=${out%% *}
	API_TIME=${out#* }
	API_BODY=$(cat "$work/api.body")
	API_RID=$(tr -d '\r' < "$work/api.head" | awk -F ': ' 'tolower($1) == "nopacs-request-id" { print $2 }')
	if [ "$API_STATUS" != 000 ]; then
		if [ "$method" = grpc.health.v1.Health/Check ]; then
			ledger "$API_RID" "/$method" "$key_id" none "$tenant" "curl $via health"
		else
			ledger "$API_RID" "/$method" "$key_id" "$(outcome_of "$API_STATUS")" "$tenant" "curl $via"
		fi
	fi
}

# cli DATA METHOD TENANT ARGS...: the binary's own command on the local socket
# of the installation in DATA. Sets CLI_STATUS; stdout in cli.out, stderr in
# cli.err. When METHOD is not "", the request is entered in the ledger: the
# commands show a request id only on an error, so a success is entered
# without one and matched in the audit log by what it was.
cli() {
	local dir=$1 method=$2 tenant=$3 rid outcome
	shift 3
	CLI_STATUS=0
	CLI_T0=$(date +%s)
	env -u NOPACS_SERVER -u NOPACS_KEY NOPACS_DATA_DIR="$dir" "$bin" "$@" > "$work/cli.out" 2> "$work/cli.err" || CLI_STATUS=$?
	[ -n "$method" ] || return 0
	rid=$(sed -n 's/.*(request id \([a-z2-7]*\)).*/\1/p' "$work/cli.err" | tail -1)
	case $CLI_STATUS in
		0) outcome='done' ;;
		3) outcome=refused ;;
		*) outcome=failed ;;
	esac
	# A command that failed before it reached the server has no request id
	# and made no request.
	if [ "$CLI_STATUS" = 0 ] || [ -n "$rid" ]; then ledger "${rid:--}" "$method" local "$outcome" "$tenant" "cli $1 ${2:-}" "$CLI_T0 $(date +%s)"; fi
}

# start_server LOG DATA [serve flags...]: start serve in the background and
# wait until it answers the health check on its local socket. Sets
# SERVER_PID; returns 1 when it exits or does not answer within 15 s.
start_server() {
	local log=$1 dir=$2
	shift 2
	"$sbin" serve --data-dir "$dir" "$@" >> "$log" 2>&1 &
	SERVER_PID=$!
	track "$SERVER_PID"
	wait_ready "$dir/run/nopacs.sock" "$SERVER_PID"
}
wait_ready() {
	local i
	for i in $(seq 1 300); do
		kill -0 "$2" 2> /dev/null || return 1
		curl -sf -o /dev/null --max-time 2 --unix-socket "$1" -H 'Content-Type: application/json' -d '{}' \
			http://local/grpc.health.v1.Health/Check 2> /dev/null && return 0
		sleep 0.05
	done
	return 1
}
# stop_server PID: SIGTERM, and the exit status in STOP_STATUS.
stop_server() {
	STOP_STATUS=0
	kill -TERM "$1" 2> /dev/null || true
	wait "$1" 2> /dev/null || STOP_STATUS=$?
	untrack "$1"
}

# --- Postgres.
psql_su() { "$DOCKER" exec -i "$PG_CONTAINER" psql -U postgres -v ON_ERROR_STOP=1 -qtA "$@"; }
wait_pg() {
	local i
	for i in $(seq 1 120); do
		# Over TCP inside the container: the image's first start runs a
		# server on its Unix socket only, then restarts it; TCP answers only
		# once the real server is up.
		"$DOCKER" exec "$PG_CONTAINER" pg_isready -q -h 127.0.0.1 -U postgres 2> /dev/null && return 0
		sleep 0.5
	done
	return 1
}

# ===========================================================================
# The sections, one file each beside this one, in the order they run: the
# builds first, since the binary under test is one of them; the audit log
# (8) after every section that made requests; the search for secrets in
# everything written (the second part of 5) after the audit; the sizes of
# the builds last. The reading sections (13 to 19) run on an installation of
# their own, after section 3 has made the run's Postgres. Each is sourced,
# so it shares this shell's variables.
here=$root/scripts/prove
reading_parts="13-reading-installation 14-catalog 15-metadata 16-downloads 17-dicomweb 18-python-client 19-reading-refused"
if [ -n "$reading_lacks" ]; then
	skip "13-19 reading the archive" "$reading_lacks"
	reading_parts=""
fi
parts="11-builds 01-install 02-first-key 03-tenants 04-refused 05-sealed 06-tenant-stops-alone 07-crash 09-one-server 10-command-line 12-access $reading_parts 08-audited 05-stored-hashed 11-sizes"
[ "$ONLY" != reading ] || parts="11-builds $reading_parts"
for part in $parts; do
	# shellcheck source=/dev/null
	. "$here/$part.sh"
done

summary_done=1
summary
n_fail=$(awk -F '\t' '$2 == "FAIL"' "$work/results.tsv" | wc -l | tr -d ' ')
n_skip=$(awk -F '\t' '$2 == "skipped"' "$work/results.tsv" | wc -l | tr -d ' ')
if [ "$n_fail" != 0 ]; then exit 1; fi
if [ "$STRICT" = 1 ] && [ "$n_skip" != 0 ]; then
	echo "prove: NOPACS_PROVE_STRICT=1 and $n_skip check(s) skipped: nothing may be skipped here" >&2
	exit 1
fi
