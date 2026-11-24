# Sourced by scripts/prove/run.sh, in the order it gives: the checks of
# section "11. the static builds (made first: the binary under test is one of them)".
# It uses the runner's variables and helpers (result, ok, fail, skip, api,
# cli, the work directory) and sets what later sections read.
# shellcheck shell=bash

section "11. the static builds (made first: the binary under test is one of them)"
host_os=$("$GO" env GOOS)
host_arch=$("$GO" env GOARCH)
mkdir -p "$work/bin"
if [ "$BUILD" = host ]; then
	CGO_ENABLED=0 "$GO" build -trimpath -ldflags "-s -w" -o "$work/bin/nopacs-$host_os-$host_arch" "$root/cmd/nopacs"
	CGO_ENABLED=0 "$GO" build -trimpath -ldflags "-s -w" -o "$work/bin/nopacs-server-$host_os-$host_arch" "$root/cmd/nopacs-server"
else
	if ! make -C "$root" --no-print-directory build BIN="$work/bin" GO="$GO" > "$work/build.log" 2>&1; then
		fail "11.1 a static build for every target of make build" "make build failed" "$(tail -20 "$work/build.log")"
		exit 1
	fi
fi
# bin is the command line, an API client; sbin is the server, which also holds
# the commands that touch the data directory with no server running.
bin=$work/bin/nopacs-$host_os-$host_arch
sbin=$work/bin/nopacs-server-$host_os-$host_arch
[ -x "$bin" ] && [ -x "$sbin" ] || { fail "11.1 a static build for every target of make build" "no nopacs and nopacs-server binaries for $host_os/$host_arch in $work/bin"; exit 1; }
echo "built $(ls "$work/bin" | tr '\n' ' ')"
