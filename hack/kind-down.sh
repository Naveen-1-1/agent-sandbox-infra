#!/usr/bin/env bash
# Delete the local kind cluster. Keeps the registry unless REGISTRY_TOO=1.
set -euo pipefail
cluster="${KIND_CLUSTER:-sbx}"
kind delete cluster --name "$cluster"
if [[ "${REGISTRY_TOO:-0}" == "1" ]]; then
  docker rm -f "${REGISTRY_NAME:-kind-registry}" >/dev/null 2>&1 || true
fi
