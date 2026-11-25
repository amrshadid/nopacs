#!/usr/bin/env bash
# The outside check of a tenant's SQLite database: the registry creates a
# tenant on SQLite in a fresh data directory, as the server will, and the
# tenant suite's sample is written into it; then the sqlite3 command line
# tool, which is SQLite's own and not NoPACs', opens the tenant's file and
# the master's.
#
# It checks that the file is where the design puts it
# (<data-dir>/tenants/<name>.db) and private, lists its tables and rows, runs
# SQLite's integrity and foreign key checks, and reads the tenant's row and
# its state change from the master.
#
#   scripts/check/tenant-sqlite.sh            uses sqlite3 and go from PATH
#   SQLITE3=/path/to/sqlite3 GO=go1.27 scripts/check/tenant-sqlite.sh
#
# Run from the repository's root. Exits non-zero on the first failed check.
set -euo pipefail

SQLITE3="${SQLITE3:-sqlite3}"
GO="${GO:-go}"

# fail and want (scripts/lib/sh/check.sh); q and show for sqlite3 (scripts/lib/sh/sqlite.sh).
# shellcheck source=../lib/sh/check.sh
. "$(dirname "$0")/../lib/sh/check.sh"
# shellcheck source=../lib/sh/sqlite.sh
. "$(dirname "$0")/../lib/sh/sqlite.sh"

# The migrations the driver ships, read from its directory, so that this check
# follows the schema instead of repeating it: "<version> <name> ..." in order.
migrations_in() {
	local f n out=""
	for f in "$1"/*.sql; do
		n="$(basename "$f")"
		out="$out${out:+ }$((10#${n%%_*})) $n"
	done
	printf '%s' "$out"
}
# tables_hold fails unless every name after the first argument (the tables the
# database has, a space-separated list) is among them.
tables_hold() {
	local have=" $1 " name
	shift
	for name in "$@"; do
		case "$have" in *" $name "*) ;; *) fail "tables: $name is missing from '$1'" ;; esac
	done
	echo "ok: tables hold $*"
}

dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT
# The key file's directory must be private to its owner (package secret).
chmod 700 "$dir"

echo "sqlite3 $("$SQLITE3" --version | cut -d' ' -f1)"

NOPACS_SAMPLE_DIR="$dir" "$GO" test -count=1 -run '^TestWriteTenantSample$' ./internal/registry
db="$dir/tenants/sample.db"
master="$dir/master.db"
[ -f "$db" ] || fail "the tenant's database is not at <data-dir>/tenants/sample.db"

# Modes as octal digits, read the way each system's stat spells it.
mode() {
	stat -c '%a' "$1" 2> /dev/null || stat -f '%Lp' "$1"
}
want "the tenants directory's mode" 700 "$(mode "$dir/tenants")"
want "the tenant file's mode" 600 "$(mode "$db")"

echo
echo "== tables"
"$SQLITE3" -batch "$db" '.tables'
tables_hold "$(q "SELECT group_concat(name, ' ') FROM (SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name)")" \
	subjects sessions scans instances receptions quarantine storages receivers senders corrections audit storage_owners schema_migrations settings events jobs profiles tenant_secrets reception_steps \
	storage_generations replica_totals replica_scans replica_damage recalls repairs repair_jobs repair_outcomes storage_switches \
	correction_keys correction_objects correction_records_owed correction_copies_abandoned \
	retention_rules retention_sessions retention_state holds hold_records dispose_jobs dispose_sessions

show schema_migrations "SELECT version, name, hex(checksum) AS checksum, applied FROM schema_migrations"
show settings "SELECT * FROM settings"
show audit "SELECT * FROM audit"
echo

mig="$(migrations_in internal/driver/sqlite/migrations/tenant)"
want "schema_migrations rows" "$(printf '%s' "$mig" | wc -w | awk '{print $1 / 2}')" "$(q 'SELECT count(*) FROM schema_migrations')"
want "the newest migration" "$(printf '%s' "$mig" | awk '{print $(NF-1), $NF}')" "$(q "SELECT version || ' ' || name FROM schema_migrations ORDER BY version DESC LIMIT 1")"
# A receiver is of type dimse, on a port of its own, or stow or import, on no port.
want "the receivers' rule of types and ports" 1 \
	"$(q "SELECT instr(sql, 'CHECK ((type = ''dimse'' AND port BETWEEN 1 AND 65535) OR (type IN (''stow'', ''import'') AND port = 0 AND bind_address = ''''))') > 0 FROM sqlite_schema WHERE type = 'table' AND name = 'receivers'")"
# A repair holds its scan only while it runs: an ended one holds nothing.
want "the repairs' rule of an ended repair's hold" 1 \
	"$(q "SELECT instr(sql, 'CHECK (repaired_at IS NULL OR open_work IS NULL)') > 0 FROM sqlite_schema WHERE type = 'table' AND name = 'repairs'")"
# A hold that stands has its end; one let go has none.
want "the repairs' rule of a hold's end" 1 \
	"$(q "SELECT instr(sql, 'CHECK ((open_work IS NULL) = (hold_until IS NULL))') > 0 FROM sqlite_schema WHERE type = 'table' AND name = 'repairs'")"
want "the index of the repairs that hold" 1 \
	"$(q "SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name = 'repairs_holding' AND sql LIKE '%WHERE open_work IS NOT NULL'")"
want "settings rows" 2 "$(q 'SELECT count(*) FROM settings')"
want "audit rows" 5 "$(q 'SELECT count(*) FROM audit')"
want "integrity_check" ok "$(q 'PRAGMA integrity_check')"
want "foreign_key_check (empty is clean)" "" "$(q 'PRAGMA foreign_key_check')"
want "journal_mode" wal "$(q 'PRAGMA journal_mode')"
want "tables that are not STRICT" 0 "$(q "SELECT count(*) FROM pragma_table_list WHERE schema = 'main' AND strict = 0 AND type = 'table' AND name NOT LIKE 'sqlite_%'")"
want "the override set twice keeps the second value" 90 "$(q "SELECT value FROM settings WHERE name = 'reception.quiet_seconds'")"
want "a Unicode value, as UTF-8" "Zoë-山田" "$(q "SELECT value FROM settings WHERE name = 'label.prefix'")"
want "every audit entry's details are a JSON object" 5 "$(q "SELECT count(*) FROM audit WHERE json_valid(details) AND json_type(details) = 'object'")"
want "the refusal's entry" "refused Bad Name" "$(q "SELECT outcome || ' ' || object FROM audit WHERE outcome <> 'done'")"
want "an audit time, read by SQLite" "2026-11-18 09:30:05" "$(q "SELECT datetime(max(at) / 1000000, 'unixepoch') FROM audit")"

# The master: the tenant's row is ready, its address is sealed (not the
# empty plaintext a SQLite tenant's address is), and the system recorded the
# change from upgrading to ready.
echo
echo "== the master's row and state change"
"$SQLITE3" -batch -header -column "$master" "SELECT id, name, driver, length(address) AS sealed_bytes, state, state_reason FROM tenants"
mq() {
	"$SQLITE3" -batch -noheader "$master" "$1"
}
want "the tenant's state in the master" "sample sqlite ready" "$(mq "SELECT name || ' ' || driver || ' ' || state FROM tenants")"
want "the sealed address's length (format, version, nonce, tag)" 33 "$(mq "SELECT length(address) FROM tenants")"
# The creation's audit trail, read by the request's id: a system entry
# written with the row (the creation started), which names the request's
# entry, and the request's own entry, written once with the change that made
# the tenant ready, with the state before and after.
echo
echo "== the creation's audit entries, by request id"
"$SQLITE3" -batch -header -column "$master" "SELECT id, method, outcome, request_id, details FROM audit WHERE request_id = 'sample-request' ORDER BY at, id"
want "entries of the creation's request" 2 "$(mq "SELECT count(*) FROM audit WHERE request_id = 'sample-request'")"
want "the creation started, by the system" "done system sample-create sqlite" \
	"$(mq "SELECT outcome || ' ' || json_extract(details, '\$.actor') || ' ' || json_extract(details, '\$.request_entry') || ' ' || json_extract(details, '\$.database_type') FROM audit WHERE request_id = 'sample-request' AND method = 'nopacs.registry/CreateTenantStarted'")"
want "the request's own entry, with the change to ready" "sample-create done upgrading -> ready" \
	"$(mq "SELECT id || ' ' || outcome || ' ' || json_extract(details, '\$.old_state') || ' -> ' || json_extract(details, '\$.new_state') FROM audit WHERE request_id = 'sample-request' AND method = '/nopacs.v1.TenantService/CreateTenant'")"
want "no other state change was written" 0 "$(mq "SELECT count(*) FROM audit WHERE method = 'nopacs.registry/SetTenantState'")"
want "the master's integrity_check" ok "$(mq 'PRAGMA integrity_check')"

echo
echo "check-tenant-sqlite: all checks passed"
