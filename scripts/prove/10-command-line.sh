# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "10. both forms of the command line".
# It uses the runner's variables and helpers (result, ok, fail, skip, api,
# cli, the work directory) and sets what later sections read.
# shellcheck shell=bash

section "10. both forms of the command line"
reader_id=$(cat "$work/keys/reader.id")
docs=""
jsonout() {
	local name=$1 method=$2 field=$3 tenant=$4
	shift 4
	cli "$data" "$method" "$tenant" --json "$@"
	cp "$work/cli.out" "$work/json-$name.json"
	docs="$docs $work/json-$name.json:$field"
}
jsonout tenants /nopacs.v1.TenantService/ListTenants tenants "*" tenant list
jsonout keys /nopacs.v1.AccessService/ListKeys keys "*" key list
jsonout grants-inst /nopacs.v1.AccessService/ListGrants grants "*" grant list --installation
jsonout grants-key /nopacs.v1.AccessService/ListGrants grants "*" grant list --key-id "$reader_id"
jsonout grants-tenant /nopacs.v1.AccessService/ListGrants grants "*" grant list --tenant lite
jsonout settings /nopacs.v1.SettingsService/ListSettings settings "*" setting list
jsonout settings-lite /nopacs.v1.SettingsService/ListSettings settings lite setting list --tenant lite
jsonout audit /nopacs.v1.AuditService/ListAuditEntries entries "*" audit list
jsonout audit-lite /nopacs.v1.AuditService/ListAuditEntries entries lite audit list --tenant lite
jsonout audit-pg /nopacs.v1.AuditService/ListAuditEntries entries pg audit list --tenant pg
# shellcheck disable=SC2086
"$PYTHON" "$helper" --work "$work" json $docs || true
# The interactive program's requests cannot be told by id; section 8 counts
# them by their time window, which these two seconds keep apart from the
# commands above (matched to the second).
sleep 2
"$PYTHON" "$helper" --work "$work" tui --binary "$bin" --data-dir "$data" --tenant lite --tenant pg || true
