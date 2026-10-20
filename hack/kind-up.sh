#!/usr/bin/env bash
# Create (or reuse) the local kind cluster and its image registry.
# Idempotent: safe to run repeatedly.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cluster="${KIND_CLUSTER:-sbx}"
config="${KIND_CONFIG:-$root/deploy/kind/cluster.yaml}"
kubeconfig="${KUBECONFIG:-$root/bin/kubeconfig}"
reg_name="${REGISTRY_NAME:-kind-registry}"
reg_port="${REGISTRY_PORT:-5001}"

mkdir -p "$(dirname "$kubeconfig")"

# kind copies HTTP(S)_PROXY into the node containers. A proxy bound to the
# host's loopback is unreachable from inside a node, so drop those.
for v in HTTP_PROXY HTTPS_PROXY http_proxy https_proxy; do
  if [[ "${!v:-}" =~ @?(localhost|127\.0\.0\.1|\[::1\])(:|/|$) ]]; then unset "$v"; fi
done

# 1. Local registry container, reachable from the host as localhost:$reg_port.
if [[ "$(docker inspect -f '{{.State.Running}}' "$reg_name" 2>/dev/null || true)" != "true" ]]; then
  docker rm -f "$reg_name" >/dev/null 2>&1 || true
  docker run -d --restart=always -p "127.0.0.1:${reg_port}:5000" --network bridge \
    --name "$reg_name" registry:3 >/dev/null
fi

# 2. Cluster.
if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
  # kind occasionally races the API server while installing the CNI; retry once.
  kind create cluster --name "$cluster" --config "$config" --kubeconfig "$kubeconfig" --wait 120s ||
    kind create cluster --name "$cluster" --config "$config" --kubeconfig "$kubeconfig" --wait 120s
else
  kind export kubeconfig --name "$cluster" --kubeconfig "$kubeconfig" >/dev/null
fi

# 3. Point each node's containerd at the registry for localhost:$reg_port.
for node in $(kind get nodes --name "$cluster"); do
  dir="/etc/containerd/certs.d/localhost:${reg_port}"
  docker exec "$node" mkdir -p "$dir"
  printf '[host."http://%s:5000"]\n' "$reg_name" | docker exec -i "$node" cp /dev/stdin "$dir/hosts.toml"
done

# 4. Registry joins the kind network so nodes can pull from it.
if [[ "$(docker inspect -f '{{json .NetworkSettings.Networks.kind}}' "$reg_name")" == "null" ]]; then
  docker network connect kind "$reg_name"
fi

# 5. Advertise the registry (KEP-1755).
kubectl --kubeconfig "$kubeconfig" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${reg_port}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
YAML

echo "cluster '$cluster' ready; export KUBECONFIG=$kubeconfig"
