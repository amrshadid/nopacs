#!/bin/bash
# Prints the hashed requirements of one outside check, for
# scripts/requirements-<name>.txt. The packages and their versions are the
# "name==version" lines of that file, so a version is changed there and this
# script run again; a package pip refuses for want of a pin is added there the
# same way.
#
# It downloads, with pip, the wheel pip would choose for each package on each
# platform the outside checks run on (Linux x86_64 with CPython 3.12 to 3.14,
# as CI may have, and this machine), hashes each file with "pip hash", and
# prints each package with all its hashes in the form pip install
# --require-hashes reads. Copy the output under the file's header comment.
#
#   scripts/requirements/hash.sh <name> [python]     name: check-seal, check-server, client-python, proto, prove-deid, prove-reception, prove-reading
set -euo pipefail
name="${1:?usage: scripts/requirements/hash.sh <name> [python], name being check-seal, check-server, client-python, proto, prove-deid, prove-reception or prove-reading}"
py="${2:-python3}"
file="$(dirname "$0")/$name.txt"
[ -f "$file" ] || { echo "hash.sh: no $file" >&2; exit 2; }

# The pins, in the file's order: the first word of every line that starts a
# requirement (not a comment, not a continued --hash line).
packages=()
while IFS= read -r pin; do
	packages+=("$pin")
done < <(sed -n 's/^\([A-Za-z0-9][A-Za-z0-9._-]*==[^ ]*\).*/\1/p' "$file")
[ "${#packages[@]}" -gt 0 ] || { echo "hash.sh: no name==version line in $file" >&2; exit 2; }

dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT

for version in 3.12 3.13 3.14; do
	"$py" -m pip download --quiet --no-deps --only-binary=:all: \
		--platform manylinux_2_34_x86_64 --platform manylinux_2_28_x86_64 \
		--platform manylinux_2_17_x86_64 --platform manylinux2014_x86_64 \
		--python-version "$version" --implementation cp -d "$dir" "${packages[@]}"
	# macOS wheels as well, so a person on a Mac with any of these versions
	# installs with the hashes, not only one who has this script's interpreter.
	"$py" -m pip download --quiet --no-deps --only-binary=:all: \
		--platform macosx_11_0_universal2 --platform macosx_11_0_arm64 \
		--platform macosx_10_14_universal2 --platform macosx_10_13_x86_64 \
		--platform macosx_10_9_x86_64 --platform macosx_10_9_universal2 \
		--python-version "$version" --implementation cp -d "$dir" "${packages[@]}"
done
"$py" -m pip download --quiet --no-deps --only-binary=:all: -d "$dir" "${packages[@]}"

# A wheel's file name starts with the package's name, with "-" and "." folded
# to "_" (PEP 427), then "-" and the version.
for pin in "${packages[@]}"; do
	pkg="${pin%%==*}"
	ver="${pin#*==}"
	prefix="$(printf '%s' "$pkg" | tr '.-' '__' | tr '[:upper:]' '[:lower:]')-$ver-"
	hashes=()
	for wheel in "$dir"/*; do
		base="$(basename "$wheel" | tr '[:upper:]' '[:lower:]')"
		case "$base" in
		"$prefix"*) hashes+=("$("$py" -m pip hash "$wheel" | sed -n 's/^--hash=//p')") ;;
		esac
	done
	[ "${#hashes[@]}" -gt 0 ] || { echo "hash.sh: no file was downloaded for $pin" >&2; exit 1; }
	printf '%s' "$pin"
	for h in "${hashes[@]}"; do
		printf ' \\\n    --hash=%s' "$h"
	done
	printf '\n'
done
