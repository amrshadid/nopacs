#!/usr/bin/env bash
# install-grpcurl.sh VERSION DEST
#
# Downloads grpcurl's release archive for this system, checks it against the
# SHA-256 committed in scripts/tools/grpcurl.sha256, and installs the grpcurl
# binary from it at DEST, as install-buf.sh does for buf. An archive whose
# hash does not match, or a system with no committed hash, fails. The hashes
# were taken from the release's grpcurl_<version>_checksums.txt and checked
# against the downloaded archives when the version was pinned.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: install-grpcurl.sh VERSION DEST" >&2
  exit 2
fi
version=$1
dest=$2

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=osx ;;
  *) echo "install-grpcurl.sh: no release for $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=x86_64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "install-grpcurl.sh: no release for $(uname -m)" >&2; exit 1 ;;
esac
name="grpcurl_${version#v}_${os}_${arch}.tar.gz"

sums="$(cd "$(dirname "$0")" && pwd)/grpcurl.sha256"
want=$(awk -v file="$version/$name" '$2 == file { print $1 }' "$sums")
if [ -z "$want" ]; then
  echo "install-grpcurl.sh: no SHA-256 for $version/$name in $sums" >&2
  exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
url="https://github.com/fullstorydev/grpcurl/releases/download/$version/$name"
echo "install-grpcurl.sh: $url"
curl --silent --show-error --fail --location --retry 3 --output "$work/$name" "$url"

if command -v sha256sum > /dev/null; then
  got=$(sha256sum "$work/$name" | cut -d ' ' -f 1)
else
  got=$(shasum -a 256 "$work/$name" | cut -d ' ' -f 1)
fi
if [ "$got" != "$want" ]; then
  echo "install-grpcurl.sh: $name has SHA-256 $got, want $want; not installed" >&2
  exit 1
fi

tar -xzf "$work/$name" -C "$work" grpcurl
mkdir -p "$(dirname "$dest")"
chmod +x "$work/grpcurl"
mv "$work/grpcurl" "$dest"
echo "install-grpcurl.sh: $dest ($got)"
