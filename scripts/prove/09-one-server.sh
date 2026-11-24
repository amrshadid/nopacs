# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "9. one server, one user".
# It uses the runner's variables and helpers (result, ok, fail, skip, api,
# cli, the work directory) and sets what later sections read.
# shellcheck shell=bash

section "9. one server, one user"
second=0
"$sbin" serve --data-dir "$data" > "$work/second.log" 2>&1 &
spid=$!
track "$spid"
for i in $(seq 1 100); do kill -0 "$spid" 2> /dev/null || break; sleep 0.1; done
if kill -0 "$spid" 2> /dev/null; then
	kill -KILL "$spid"
	second=running
fi
wait "$spid" 2> /dev/null || second=$?
untrack "$spid"
api sock "" nopacs.v1.AccessService/WhoAmI '{}'
# Refused for the reason that matters, the lock, and not for another one
# (such as the socket's address being in use) that a server without the lock
# might also meet.
if [ "$second" != 0 ] && [ "$second" != running ] && grep -q 'another nopacs-server serve is running' "$work/second.log" && [ "$API_STATUS" = 200 ]; then
	ok "9.1 a second serve on the same directory is refused" "exit $second: '$(head -1 "$work/second.log")'; the first still answers on its socket"
else
	fail "9.1 a second serve on the same directory is refused" "second serve: $second; first server's answer: $API_STATUS" "$(cat "$work/second.log")"
fi

if [ "$(uname -s)" = Linux ] && sudo -n true 2> /dev/null; then
	other=corproof$$
	sudo -n useradd --no-create-home --shell /usr/sbin/nologin "$other"
	user_made=$other
	other_uid=$(id -u "$other")
	try_other() {
		OTHER_STATUS=0
		sudo -n -u "$other" curl -sS --max-time 5 -o /dev/null -w '%{http_code}' --unix-socket "$sock" -H 'Content-Type: application/json' \
			-d '{}' http://local/nopacs.v1.AccessService/WhoAmI > "$work/other.out" 2> "$work/other.err" || OTHER_STATUS=$?
	}
	# As the server keeps its directories: the socket cannot be reached.
	try_other
	s1=$OTHER_STATUS
	e1=$(cat "$work/other.err")
	h1=$(cat "$work/other.out")
	# With every directory on the way and the socket opened to everyone, the
	# peer credential check is the only guard left: the connection must be
	# closed unanswered.
	chmod o+x "$work" "$data" "$data/run"
	chmod o+rw "$sock"
	try_other
	s2=$OTHER_STATUS
	e2=$(cat "$work/other.err")
	h2=$(cat "$work/other.out")
	chmod o-x "$work" "$data" "$data/run"
	chmod o-rw "$sock"
	sleep 0.2
	peer_log=$(grep -c "\"peer_user\":\"$other_uid\"" "$work/serve.log" || true)
	if [ "$s1" != 0 ] && [ "$s2" != 0 ] && [ "$h1" = 000 ] && [ "$h2" = 000 ] && [ "$peer_log" -ge 1 ]; then
		ok "9.2 another operating system user cannot use the local socket" "user $other (uid $other_uid): with the server's modes curl exits $s1 ($e1); with every mode opened curl exits $s2 ($e2), no HTTP answer, and the server logs the refused peer"
	else
		fail "9.2 another operating system user cannot use the local socket" "closed: exit $s1 http $h1; opened: exit $s2 http $h2; peer refusals in the log: $peer_log" "$e1" "$e2"
	fi
	sudo -n userdel "$other" && user_made=""
else
	skip "9.2 another operating system user cannot use the local socket" "needs Linux and sudo without a password to make a second user; this is $(uname -s) $(sudo -n true 2>&1 | head -1 | cut -c 1-60)"
fi
