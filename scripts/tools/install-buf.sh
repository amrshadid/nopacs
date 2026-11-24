#!/usr/bin/env bash
# install-buf.sh VERSION DEST
#
# Downloads buf's release binary for this system, checks it against the SHA-256
# committed in scripts/tools/buf.sha256, and installs it at DEST. A binary whose hash
# does not match, or a system with no committed hash, fails: there is no
# fallback, so the buf that runs is always the one whose hash was reviewed.
# The hashes were taken from the release's sha256.txt and checked against the
# downloaded files when the version was pinned.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: install-buf.sh VERSION DEST" >&2
  exit 2
fi
version=$1
dest=$2

os=$(uname -s)
arch=$(uname -m)
# Linux reports 64-bit ARM as aarch64 or arm64; the release calls it aarch64.
if [ "$os" = Linux ] && [ "$arch" = arm64 ]; then
  arch=aarch64
fi
name="buf-$os-$arch"

sums="$(cd "$(dirname "$0")" && pwd)/buf.sha256"
want=$(awk -v file="$version/$name" '$2 == file { print $1 }' "$sums")
if [ -z "$want" ]; then
  echo "install-buf.sh: no SHA-256 for $version/$name in $sums" >&2
  exit 1
fi

mkdir -p "$(dirname "$dest")"
part="$dest.download"
url="https://github.com/bufbuild/buf/releases/download/$version/$name"
echo "install-buf.sh: $url"
curl --silent --show-error --fail --location --retry 3 --output "$part" "$url"

if command -v sha256sum > /dev/null; then
  got=$(sha256sum "$part" | cut -d ' ' -f 1)
else
  got=$(shasum -a 256 "$part" | cut -d ' ' -f 1)
fi
if [ "$got" != "$want" ]; then
  rm -f "$part"
  echo "install-buf.sh: $name has SHA-256 $got, want $want; not installed" >&2
  exit 1
fi

chmod +x "$part"
mv "$part" "$dest"
echo "install-buf.sh: $dest ($got)"
