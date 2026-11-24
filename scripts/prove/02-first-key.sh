# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "2. the first key, with no default password".
# It uses the runner's variables and helpers (result, ok, fail, skip, api,
# cli, the work directory) and sets what later sections read.
# shellcheck shell=bash

section "2. the first key, with no default password"
n=$("$SQLITE3" "$data/master.db" 'select count(*) from api_keys; select count(*) from grants;' | tr '\n' ' ')
if [ "$n" = "0 0 " ]; then
	ok "2.1 no key and no grant exist after init" "sqlite3 counts api_keys and grants in master.db: 0 and 0; the only way in is the local socket"
else
	fail "2.1 no key and no grant exist after init" "api_keys, grants: $n"
fi

# make_key NAME CLI-ARGS...: a key made by "nopacs key create" on the local
# socket; the key in keys/NAME (mode 0600), its id in keys/NAME.id.
make_key() {
	local name=$1
	shift
	cli "$data" /nopacs.v1.AccessService/CreateKey "*" key create --holder "proof $name" "$@"
	(umask 077 && head -1 "$work/cli.out" > "$work/keys/$name")
	local k
	k=$(cat "$work/keys/$name")
	printf '%s' "$k" | grep -Eq '^nopacs_[1-9A-HJ-NP-Za-km-z]{12}_[1-9A-HJ-NP-Za-km-z]{40,}$' || return 1
	secret "$k" search
	secret "${k##*_}" search
	printf '%s' "$k" | cut -d _ -f 2 > "$work/keys/$name.id"
}
if make_key admin --no-expiry; then
	admin_id=$(cat "$work/keys/admin.id")
	cli "$data" /nopacs.v1.AccessService/SetGrant "*" grant set "$admin_id" --role admin --installation
	if [ "$CLI_STATUS" = 0 ]; then
		ok "2.2 the first key, made and granted on the local socket" "key create: $admin_id ($(fingerprint "$(cat "$work/keys/admin")")); grant set: admin on the installation"
	else
		fail "2.2 the first key, made and granted on the local socket" "grant set exit $CLI_STATUS" "$(cat "$work/cli.err")"
	fi
else
	fail "2.2 the first key, made and granted on the local socket" "key create exit $CLI_STATUS or the key is not nopacs_<id>_<secret>" "$(cat "$work/cli.err")"
	exit 1
fi
"$PYTHON" "$helper" --work "$work" refusals --mode walk --proto "$root/api/proto" --not-served "$root/api/not-served.txt" \
	--addr "127.0.0.1:$port" --ca "$work/cert.pem" --grpcurl "$GRPCURL" || true
