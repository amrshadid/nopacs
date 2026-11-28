#!/usr/bin/env bash
# The outside check of a tenant's Postgres database: the registry creates a
# tenant on Postgres, as the server will, in a schema of its own, with a
# Postgres master in another, and the tenant suite's sample is written into
# it; then psql and pg_dump, which are Postgres's own and not NoPACs', read
# the tenant's schema and the master's.
#
# It lists the tables and rows, checks every constraint and index, reads
# times and audit details with Postgres's own functions, reads the tenant's
# row and its state change from the master, checks the schema records the
# tenant's id, and searches both dumps for the login's password, which must
# be absent.
#
# The driver is given NOPACS_TEST_POSTGRES, the URL of a scratch database
# the login may make schemas in. PSQL and PG_DUMP are the commands that run
# psql and pg_dump, which may be the ones in a container, as for
# scripts/check/master-postgres.sh (which says why); PSQL_URL is the URL as
# psql sees it.
#
#   NOPACS_TEST_POSTGRES=postgres://u:p@127.0.0.1:5432/db scripts/check/tenant-postgres.sh
#
# Run from the repository's root. Exits non-zero on the first failed check.
# Nothing here prints a URL: it holds the password.
set -euo pipefail

PSQL="${PSQL:-psql}"
PG_DUMP="${PG_DUMP:-pg_dump}"
GO="${GO:-go}"
: "${NOPACS_TEST_POSTGRES:?set NOPACS_TEST_POSTGRES to the URL of a scratch database}"
PSQL_URL="${PSQL_URL:-$NOPACS_TEST_POSTGRES}"

# fail and want (scripts/lib/sh/check.sh); q and show for psql (scripts/lib/sh/psql.sh).
# shellcheck source=../lib/sh/check.sh
. "$(dirname "$0")/../lib/sh/check.sh"
# shellcheck source=../lib/sh/psql.sh
. "$(dirname "$0")/../lib/sh/psql.sh"

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
chmod 700 "$dir"
schema="nopacs_check_t_$(date +%s)_$$"
mschema="${schema}_master"

cleanup() {
	q "DROP SCHEMA IF EXISTS \"$schema\" CASCADE; DROP SCHEMA IF EXISTS \"$mschema\" CASCADE" > /dev/null 2>&1 || true
	rm -rf "$dir"
}
trap cleanup EXIT

echo "psql: $(q 'SHOW server_version')"

NOPACS_SAMPLE_SCHEMA="$schema" NOPACS_SAMPLE_DIR="$dir" "$GO" test -count=1 -run '^TestWriteTenantSample$' ./internal/driver/postgres

t="\"$schema\""
m="\"$mschema\""
echo
echo "== the tenant's tables"
tables_hold "$(q "SELECT string_agg(table_name, ' ' ORDER BY table_name COLLATE \"C\") FROM information_schema.tables WHERE table_schema = '$schema'")" \
	subjects sessions scans instances receptions quarantine storages receivers senders corrections audit storage_owners schema_migrations settings events jobs profiles tenant_secrets tenant_identity \
	storage_generations replica_totals replica_scans replica_damage recalls repairs repair_jobs replicate_jobs repair_outcomes delete_batch_jobs delete_batch_targets route_rule_sets route_matches storage_switches \
	scrub_verified scan_sidecars \
	correction_keys correction_objects correction_records_owed correction_copies_abandoned \
	retention_rules retention_sessions retention_state holds hold_records dispose_jobs dispose_sessions

show schema_migrations "SELECT version, name, encode(checksum, 'hex') AS checksum, applied FROM $t.schema_migrations"
show tenant_identity "SELECT * FROM $t.tenant_identity"
show settings "SELECT * FROM $t.settings ORDER BY name"
show audit "SELECT id, at, key_id, remote_addr, method, tenant_name, object, outcome, reason, details FROM $t.audit ORDER BY at, id"
echo

mig="$(migrations_in internal/driver/postgres/migrations/tenant)"
want "schema_migrations rows" "$(printf '%s' "$mig" | wc -w | awk '{print $1 / 2}')" "$(q "SELECT count(*) FROM $t.schema_migrations")"
want "the newest migration" "$(printf '%s' "$mig" | awk '{print $(NF-1), $NF}')" "$(q "SELECT version || ' ' || name FROM $t.schema_migrations ORDER BY version DESC LIMIT 1")"
# A receiver is of type dimse, on a port of its own, or stow or import, on no port.
want "the receivers' rules of types and ports" "CHECK ((((type = 'dimse'::text) AND ((port >= 1) AND (port <= 65535))) OR ((type = ANY (ARRAY['stow'::text, 'import'::text])) AND (port = 0) AND (bind_address = ''::text)))) CHECK ((type = ANY (ARRAY['dimse'::text, 'stow'::text, 'import'::text])))" \
	"$(q "SELECT string_agg(pg_get_constraintdef(oid), ' ' ORDER BY conname) FROM pg_constraint WHERE conname IN ('receivers_type_check', 'receivers_port_check') AND connamespace = '$schema'::regnamespace")"
want "the schema records the tenant's id" sample-tenant "$(q "SELECT id FROM $t.tenant_identity")"
# A repair holds its scan only while it runs: an ended one holds nothing.
want "the repairs' rule of an ended repair's hold" "CHECK ((((open_work IS NULL) = (open_attempt IS NULL)) AND ((open_work IS NULL) = (hold_until IS NULL)) AND ((repaired_at IS NULL) OR (open_work IS NULL))))" \
	"$(q "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'repairs_hold_check' AND connamespace = '$schema'::regnamespace")"
want "the index of the repairs that hold" "CREATE INDEX repairs_holding ON $schema.repairs USING btree (series_uid) WHERE (open_work IS NOT NULL)" \
	"$(q "SELECT indexdef FROM pg_indexes WHERE schemaname = '$schema' AND indexname = 'repairs_holding'")"
want "settings rows" 2 "$(q "SELECT count(*) FROM $t.settings")"
want "audit rows" 5 "$(q "SELECT count(*) FROM $t.audit")"
want "constraints that are not valid" 0 \
	"$(q "SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = '$schema' AND NOT c.convalidated")"
want "the audit indexes" "audit_by_key audit_by_outcome audit_by_seq audit_by_tenant audit_by_time audit_pkey audit_unsealed" \
	"$(q "SELECT string_agg(indexname, ' ' ORDER BY indexname COLLATE \"C\") FROM pg_indexes WHERE schemaname = '$schema' AND tablename = 'audit'")"
# The columns that name or key something are compared bytewise, whatever the
# database's locale. The baseline schema has at least this many; a later
# migration only adds columns.
want "columns with the C collation (at least 90)" t \
	"$(q "SELECT count(*) >= 90 FROM information_schema.columns WHERE table_schema = '$schema' AND collation_name = 'C'")"
want "at is timestamptz and details jsonb" "timestamp with time zone jsonb" \
	"$(q "SELECT (SELECT data_type FROM information_schema.columns WHERE table_schema = '$schema' AND table_name = 'audit' AND column_name = 'at') || ' ' || (SELECT data_type FROM information_schema.columns WHERE table_schema = '$schema' AND table_name = 'audit' AND column_name = 'details')")"
# The reception pipeline's tables (migrations/tenant/0001_baseline.sql):
# their constraints and indexes as Postgres lists them, the running totals'
# one row each, and the rules the code relies on held by the database.
show "the pipeline's constraints" "SELECT c.conrelid::regclass AS table_name, c.conname, pg_get_constraintdef(c.oid) AS definition FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = '$schema' AND c.conrelid::regclass::text NOT IN ('audit', 'settings', 'tenant_identity', 'schema_migrations') ORDER BY 1, 2"
show "the pipeline's indexes" "SELECT tablename, indexname, indexdef FROM pg_indexes WHERE schemaname = '$schema' AND tablename NOT IN ('audit', 'settings', 'tenant_identity', 'schema_migrations') ORDER BY 1, 2"
want "the pipeline's indexes (at least the 78 the baseline schema has)" t \
	"$(q "SELECT count(*) >= 78 FROM pg_indexes WHERE schemaname = '$schema' AND tablename NOT IN ('audit', 'settings', 'tenant_identity', 'schema_migrations')")"
want "the instances' indexes" "instances_by_scan instances_pkey" \
	"$(q "SELECT string_agg(indexname, ' ' ORDER BY indexname COLLATE \"C\") FROM pg_indexes WHERE schemaname = '$schema' AND tablename = 'instances'")"
want "one open reception per receiver and study, and one arrival number per study" "receptions_arrival receptions_open" \
	"$(q "SELECT string_agg(indexname, ' ' ORDER BY indexname COLLATE \"C\") FROM pg_indexes WHERE schemaname = '$schema' AND tablename = 'receptions' AND indexdef LIKE 'CREATE UNIQUE INDEX%' AND indexname <> 'receptions_pkey'")"
want "the work claim's index leaves out finished units" "WHERE (state = ANY (ARRAY['queued'::text, 'leased'::text]))" \
	"$(q "SELECT substring(indexdef from 'WHERE .*') FROM pg_indexes WHERE schemaname = '$schema' AND indexname = 'work_claimable'")"
want "checksums are 32 bytes" "CHECK ((octet_length(sha256) = 32))" \
	"$(q "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'instances_sha256_check' AND connamespace = '$schema'::regnamespace")"
want "the catalog's totals, one row" "1 0 0 0 0" "$(q "SELECT count(*) || ' ' || max(subjects) || ' ' || max(sessions) || ' ' || max(scans) || ' ' || max(instances) FROM $t.catalog_totals")"
want "the quarantine's totals, one row" "1 0 0" "$(q "SELECT count(*) || ' ' || max(files) || ' ' || max(bytes) FROM $t.quarantine_totals")"
want "a held instance with a checksum of the wrong length is refused by the database" "refused" \
	"$(q "DO \$\$ BEGIN INSERT INTO $t.instances VALUES ('2.25.1', 1, 'r', decode('00', 'hex'), NULL, '', 0, 0, 0, now()); RAISE NOTICE 'kept'; EXCEPTION WHEN check_violation THEN NULL; END \$\$; SELECT CASE WHEN count(*) = 0 THEN 'refused' ELSE 'kept' END FROM $t.instances")"
want "the override set twice keeps the second value" 90 "$(q "SELECT value FROM $t.settings WHERE name = 'reception.quiet_seconds'")"
want "a Unicode value, as UTF-8" "Zoë-山田" "$(q "SELECT value FROM $t.settings WHERE name = 'label.prefix'")"
want "every audit entry's details are a JSON object" 5 "$(q "SELECT count(*) FROM $t.audit WHERE jsonb_typeof(details) = 'object'")"
want "the refusal's entry" "refused Bad Name" "$(q "SELECT outcome || ' ' || object FROM $t.audit WHERE outcome <> 'done'")"
want "an audit time, with its microseconds, in UTC" "2026-11-18 09:30:05.123456" \
	"$(q "SELECT to_char(max(at) AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US') FROM $t.audit")"

# The master: the tenant's row is ready, its address is sealed (the plaintext
# is a URL with a password, and neither is in the row), and the system
# recorded the creation, and the request's own entry has the change from
# upgrading to ready.
echo
echo "== the master's row and state change"
show tenants "SELECT id, name, driver, length(address) AS sealed_bytes, state, state_reason FROM $m.tenants"
want "the tenant's state in the master" "sample postgres ready" "$(q "SELECT name || ' ' || driver || ' ' || state FROM $m.tenants")"
want "entries of the creation's request" 2 "$(q "SELECT count(*) FROM $m.audit WHERE request_id = 'sample-request'")"
want "the creation started, by the system" "done system sample-create postgres" \
	"$(q "SELECT outcome || ' ' || (details ->> 'actor') || ' ' || (details ->> 'request_entry') || ' ' || (details ->> 'database_type') FROM $m.audit WHERE request_id = 'sample-request' AND method = 'nopacs.registry/CreateTenantStarted'")"
want "the request's own entry, with the change to ready" "sample-create done upgrading -> ready" \
	"$(q "SELECT id || ' ' || outcome || ' ' || (details ->> 'old_state') || ' -> ' || (details ->> 'new_state') FROM $m.audit WHERE request_id = 'sample-request' AND method = '/nopacs.v1.TenantService/CreateTenant'")"
want "no other state change was written" 0 "$(q "SELECT count(*) FROM $m.audit WHERE method = 'nopacs.registry/SetTenantState'")"

# The login's password is in neither dump, nor in the master's rows.
pw="$(printf '%s' "$NOPACS_TEST_POSTGRES" | sed -n 's|^[a-zA-Z]*://[^:@/]*:\([^@]*\)@.*|\1|p')"
tdump="$dir/tenant.sql"
mdump="$dir/master.sql"
# shellcheck disable=SC2086
$PG_DUMP --schema="$schema" "$PSQL_URL" > "$tdump"
# shellcheck disable=SC2086
$PG_DUMP --schema="$mschema" "$PSQL_URL" > "$mdump"
echo "dumps: $(wc -l < "$tdump" | tr -d ' ') lines of the tenant's, $(wc -l < "$mdump" | tr -d ' ') of the master's"
if [ -n "$pw" ]; then
	for d in "$tdump" "$mdump"; do
		if grep -qF "$pw" "$d"; then
			fail "the login's password is in $(basename "$d")"
		fi
	done
	echo "ok: the login's password is in neither dump"
else
	echo "skipped: the address holds no password to search for"
fi
grep -q "postgres://" "$mdump" && fail "a database address is in the master's dump in plain text"
echo "ok: no database address in plain text in the master's dump"

echo
echo "check-tenant-postgres: all checks passed"
