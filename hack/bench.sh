#!/usr/bin/env bash
# Run sbx-bench against the deployed platform: pull the bench images onto
# every node first, so wakes don't measure downloads, then measure. When
# the platform runs the bundled object storage (kind), it is reached
# through a port-forward with its generated credentials, so the bench can
# wipe the caches for a cold first run.
# Usage: hack/bench.sh [sbx-bench flags...]
set -euo pipefail

ns="${NAMESPACE:-sbx-system}"
config="${BENCH_CONFIG:-test/bench/repos.json}"
secret() { kubectl -n "$ns" get secret "$1" -o jsonpath="{.data.$2}" | base64 -d; }

runtime=$(kubectl -n "$ns" get deploy sbx-controller \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SBX_RUNTIME_IMAGE")].value}')
# shellcheck disable=SC2046 # one image per word
NAMESPACE="$ns" hack/prepull.sh "$runtime" $(jq -r '.repos[].image' "$config")

SBX_TOKEN=$(secret sbx-gateway-admin token)
export SBX_TOKEN
args=()
if kubectl -n "$ns" get service sbx-s3 >/dev/null 2>&1; then
  SBX_S3_ACCESS_KEY=$(secret sbx-s3-credentials accessKey)
  SBX_S3_SECRET_KEY=$(secret sbx-s3-credentials secretKey)
  export SBX_S3_ACCESS_KEY SBX_S3_SECRET_KEY
  kubectl -n "$ns" port-forward service/sbx-s3 18333:8333 >/dev/null &
  pf=$!
  trap 'kill $pf' EXIT
  curl -s -o /dev/null --retry 30 --retry-connrefused --retry-delay 1 http://127.0.0.1:18333/
  args+=(--s3-endpoint http://127.0.0.1:18333)
fi

# ${args[@]+...}: bash 3.2 (macOS) calls an empty array unset under set -u.
bin/sbx-bench --config "$config" --namespace "$ns" --platform-commit "$(git rev-parse --short HEAD)" ${args[@]+"${args[@]}"} "$@"
