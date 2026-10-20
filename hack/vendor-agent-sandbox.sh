#!/usr/bin/env bash
# Copy the upstream agent-sandbox Helm chart, unmodified, into charts/agent-sandbox.
# Upstream does not publish the chart, so we vendor it at a pinned tag and commit.
#
#   hack/vendor-agent-sandbox.sh          refresh the vendored copy
#   hack/vendor-agent-sandbox.sh --check  fail if the vendored copy differs from upstream
set -euo pipefail

REPO=https://github.com/kubernetes-sigs/agent-sandbox.git
TAG=v1.0.7
SHA=807cc37107bd7c012115afac9fa9c3b77b8365c2

root="$(cd "$(dirname "$0")/.." && pwd)"
dest="$root/charts/agent-sandbox"
work="$(mktemp -d "${TMPDIR:-/tmp}/vendor-agent-sandbox.XXXXXX")"
trap 'rm -rf "$work"' EXIT

git clone --quiet --depth 1 --branch "$TAG" "$REPO" "$work/src" 2>/dev/null
got="$(git -C "$work/src" rev-parse HEAD)"
if [[ "$got" != "$SHA" ]]; then
  echo "upstream tag $TAG resolves to $got, expected $SHA" >&2
  exit 1
fi

mkdir -p "$work/out"
cp -R "$work/src/helm/." "$work/out/"
cp "$work/src/LICENSE" "$work/out/LICENSE"
printf 'repository: %s\ntag: %s\ncommit: %s\n' "$REPO" "$TAG" "$SHA" > "$work/out/UPSTREAM"

if [[ "${1:-}" == "--check" ]]; then
  diff -r "$work/out" "$dest"
  echo "charts/agent-sandbox matches upstream $TAG"
  exit 0
fi

rm -rf "$dest"
mkdir -p "$(dirname "$dest")"
cp -R "$work/out" "$dest"
echo "vendored agent-sandbox $TAG ($SHA) into charts/agent-sandbox"
