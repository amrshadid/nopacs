# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "3. tenants on both databases".
# It uses the runner's variables and helpers (result, ok, fail, skip, api,
# cli, the work directory) and sets what later sections read.
# shellcheck shell=bash

section "3. tenants on both databases"
if [ -n "$PG_CONTAINER" ]; then
	[ -n "$PG_PORT" ] || { echo "prove: NOPACS_PROVE_PG_CONTAINER needs NOPACS_PROVE_PG_PORT" >&2; exit 2; }
	pg_where="the container $PG_CONTAINER"
else
	PG_CONTAINER=nopacs-proof-$(basename "$work" | cut -d . -f 2 | tr '[:upper:]' '[:lower:]')
	supw=$(openssl rand -hex 16)
	secret "$supw"
	# A fixed host port: a published port chosen by Docker can change when
	# the container is started again in section 6. A port free here may still
	# be held where Docker runs (a virtual machine, on macOS), so a refused
	# port is tried again with another.
	own_pg=$PG_CONTAINER
	for attempt in 1 2 3; do
		PG_PORT=$(free_port)
		"$DOCKER" rm -f "$PG_CONTAINER" > /dev/null 2>&1 || true
		"$DOCKER" run -d --name "$PG_CONTAINER" -e POSTGRES_PASSWORD="$supw" -p "127.0.0.1:$PG_PORT:5432" "$PG_IMAGE" > /dev/null 2> "$work/docker.err" && break
		[ "$attempt" != 3 ] || { cat "$work/docker.err" >&2; exit 1; }
	done
	own_pg=$PG_CONTAINER
	pg_where="its own container from $PG_IMAGE"
fi
wait_pg || { fail "3.0 Postgres answers" "pg_isready never answered in $PG_CONTAINER"; exit 1; }
pg_version=$(psql_su -c 'show server_version' | cut -d ' ' -f 1)
# A password with the characters a URL must escape, so its escaped form
# differs and both forms are searched for in section 5.
pgpw="pr-$(openssl rand -hex 8)/@:+%$(openssl rand -hex 4)"
pgpw_esc=$(urlescape "$pgpw")
secret "$pgpw" search
secret "$pgpw_esc" search
secret "$(printf %s "$pgpw_esc" | tr 'A-F' 'a-f')" search
pg_made=1
psql_su -c "create role nopacs_proof login password '$pgpw'" > /dev/null
psql_su -c "create database nopacs_proof_main owner nopacs_proof" > /dev/null
pg_dsn() { printf 'postgres://nopacs_proof:%s@127.0.0.1:%s/%s?sslmode=disable' "$pgpw_esc" "$PG_PORT" "$1"; }
NOPACS_PROOF_DSN=$(pg_dsn nopacs_proof_main)
secret "$NOPACS_PROOF_DSN" search
echo "Postgres $pg_version in $pg_where, on 127.0.0.1:$PG_PORT; a role and an empty database made for the tenant"

cli "$data" /nopacs.v1.TenantService/CreateTenant lite --json tenant create lite
lite_state=$(json_get '["tenant"]["state"]' < "$work/cli.out" 2> /dev/null || echo "exit $CLI_STATUS")
if [ "$lite_state" = TENANT_STATE_READY ] && [ -f "$data/tenants/lite.db" ]; then
	ok "3.1 a tenant on SQLite, ready" "tenant create lite: $lite_state, file tenants/lite.db mode $(mode_of "$data/tenants/lite.db")"
else
	fail "3.1 a tenant on SQLite, ready" "state $lite_state" "$(cat "$work/cli.out" "$work/cli.err")"
fi
export NOPACS_PROOF_DSN
cli "$data" /nopacs.v1.TenantService/CreateTenant pg --json tenant create pg --db-env NOPACS_PROOF_DSN
unset NOPACS_PROOF_DSN
pg_state=$(json_get '["tenant"]["state"]' < "$work/cli.out" 2> /dev/null || echo "exit $CLI_STATUS")
if [ "$pg_state" = TENANT_STATE_READY ]; then
	ok "3.2 a tenant on Postgres, ready" "tenant create pg --db-env NOPACS_PROOF_DSN (sslmode=disable to loopback): $pg_state"
else
	fail "3.2 a tenant on Postgres, ready" "state $pg_state" "$(cat "$work/cli.out" "$work/cli.err")"
fi

# An address to another machine must say how the connection is protected:
# the driver's default tries encryption and silently falls back to none.
NOPACS_PROOF_REMOTE="postgres://nopacs_proof:$pgpw_esc@db.example.invalid:5432/nopacs_proof_main"
secret "$NOPACS_PROOF_REMOTE" search
export NOPACS_PROOF_REMOTE
cli "$data" /nopacs.v1.TenantService/CreateTenant remote tenant create remote --db-env NOPACS_PROOF_REMOTE
remote_status=$CLI_STATUS
remote_err=$(cat "$work/cli.err")
unset NOPACS_PROOF_REMOTE
rows=$("$SQLITE3" "$data/master.db" "select count(*) from tenants where name = 'remote'")
files=$(find "$data/tenants" -mindepth 1 -maxdepth 1 -name 'remote*' | wc -l | tr -d ' ')
cli "$data" /nopacs.v1.TenantService/ListTenants "*" --json tenant list
listed=$(grep -c '"name": "remote"' "$work/cli.out" || true)
if [ "$remote_status" = 2 ] && printf '%s' "$remote_err" | grep -q 'is not one the database type postgres takes' && [ "$rows" = 0 ] && [ "$files" = 0 ] && [ "$listed" = 0 ]; then
	ok "3.3 a remote address without sslmode is refused as invalid and leaves no tenant" "exit 2 (invalid): '$(printf '%s' "$remote_err" | sed 's/ (request id.*//')'; master rows 0, files 0, listed 0"
else
	fail "3.3 a remote address without sslmode is refused as invalid and leaves no tenant" "exit $remote_status, master rows $rows, files $files, listed $listed" "$remote_err"
fi

master_tables=$("$SQLITE3" "$data/master.db" "select name from sqlite_master where type = 'table' order by name" | tr '\n' ' ')
lite_tables=$("$SQLITE3" "$data/tenants/lite.db" "select name from sqlite_master where type = 'table' order by name" | tr '\n' ' ')
pg_tables=$(psql_su -d nopacs_proof_main -c "select tablename from pg_tables where schemaname = 'public' order by tablename" | tr '\n' ' ')
pg_owner=$(psql_su -d nopacs_proof_main -c "select distinct tableowner from pg_tables where schemaname = 'public'" | tr '\n' ' ')
# The tables change as the schema grows, so the check holds what must not: a
# list of tables each database must have, and the exact difference between a
# SQLite and a Postgres tenant. SQLite keeps its running count of receptions by
# step in reception_steps and its sequence of ids in sqlite_sequence; a
# Postgres tenant alone keeps its tenant's id (tenant_identity), so that a
# database given to two tenants by mistake is refused.
has_all() { # the tables listed in $1 hold every name given after it
	local have=" $1" name
	shift
	for name in "$@"; do
		case "$have" in *" $name "*) ;; *) return 1 ;; esac
	done
}
missing_of() { # the names given after $1 that $1 lacks
	local have=" $1" name out=""
	shift
	for name in "$@"; do
		case "$have" in *" $name "*) ;; *) out="$out$name " ;; esac
	done
	printf '%s' "$out"
}
# These lists follow the baseline schema (migrations/*/0001_baseline.sql) and
# the migration record the migrator keeps; a table added later is added here.
master_want=(api_keys audit audit_anchors audit_checkpoints grants schema_migrations settings tenants)
tenant_want=(subjects sessions scans instances receptions quarantine storages receivers senders corrections audit storage_owners schema_migrations settings events jobs profiles)
lite_alone=$(comm -23 <(printf '%s\n' $lite_tables | sort) <(printf '%s\n' $pg_tables | sort) | tr '\n' ' ')
pg_alone=$(comm -13 <(printf '%s\n' $lite_tables | sort) <(printf '%s\n' $pg_tables | sort) | tr '\n' ' ')
miss_master=$(missing_of "$master_tables" "${master_want[@]}")
miss_lite=$(missing_of "$lite_tables" "${tenant_want[@]}")
miss_pg=$(missing_of "$pg_tables" "${tenant_want[@]}")
# The id kept there must be the master's.
pg_identity=$(psql_su -d nopacs_proof_main -c "select id from tenant_identity" 2> /dev/null || true)
master_pg_id=$("$SQLITE3" "$data/master.db" "select id from tenants where name = 'pg'")
if [ -z "$miss_master$miss_lite$miss_pg" ] && [ "$lite_alone" = "reception_steps sqlite_sequence " ] && [ "$pg_alone" = "tenant_identity " ] &&
	[ "$pg_owner" = "nopacs_proof " ] && [ -n "$pg_identity" ] && [ "$pg_identity" = "$master_pg_id" ]; then
	ok "3.4 sqlite3 and psql list each database's tables" "master (sqlite3): $(printf '%s' "$master_tables" | wc -w | tr -d ' ') tables, all of ${master_want[*]}; lite (sqlite3) and pg (psql) each hold all of ${tenant_want[*]}; only lite has: $lite_alone; only pg has: $pg_alone(owner nopacs_proof; tenant_identity holds pg's id in the master)"
else
	fail "3.4 sqlite3 and psql list each database's tables" "missing in master: $miss_master; in lite: $miss_lite; in pg: $miss_pg; only lite has: $lite_alone; only pg has: $pg_alone; pg owner $pg_owner; pg identity '$pg_identity', master's '$master_pg_id'"
fi
