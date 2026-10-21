#!/usr/bin/env bash
# Pull images onto every node that runs workspaces ahead of time, so wakes
# show restore time rather than image download time. Works on any cluster:
# a short-lived DaemonSet whose init containers each start one image (so
# every image needs /bin/sh). Tainted nodes, such as kind's control plane,
# are skipped, as workspaces never run there.
# Usage: hack/prepull.sh IMAGE...
set -euo pipefail

ns="${NAMESPACE:-sbx-system}"
name=sbx-prepull
inits=""
i=0
for img in "$@"; do
  inits+="
        - name: pull-$i
          image: $img
          imagePullPolicy: IfNotPresent
          command: [sh, -c, 'exit 0']
          resources: {requests: {cpu: 10m, memory: 16Mi}}"
  i=$((i + 1))
done

trap 'kubectl -n "$ns" delete daemonset "$name" --ignore-not-found --wait=false >/dev/null' EXIT
kubectl -n "$ns" apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: $name
  labels: {app.kubernetes.io/name: $name}
spec:
  selector:
    matchLabels: {app.kubernetes.io/name: $name}
  template:
    metadata:
      labels: {app.kubernetes.io/name: $name}
    spec:
      automountServiceAccountToken: false
      terminationGracePeriodSeconds: 0
      initContainers:$inits
      containers:
        - name: pulled
          image: registry.k8s.io/pause:3.10
          resources: {requests: {cpu: 10m, memory: 16Mi}}
EOF
echo "pulling $# image(s) onto every workspace node"
kubectl -n "$ns" rollout status daemonset "$name" --timeout="${PREPULL_TIMEOUT:-20m}" >/dev/null
