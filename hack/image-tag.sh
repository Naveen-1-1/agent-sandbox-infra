#!/usr/bin/env bash
# Print an image tag derived from the built binaries, Dockerfiles and fixtures,
# so unchanged components keep their tag and pods don't roll needlessly.
set -euo pipefail
arch="${1:?usage: image-tag.sh ARCH}"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
{ find "bin/linux_${arch}" images test/fixtures -type f -print0 | sort -z | xargs -0 shasum -a 256; } \
  | shasum -a 256 | cut -c1-12 | sed 's/^/dev-/'
