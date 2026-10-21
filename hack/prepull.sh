#!/usr/bin/env bash
# Pull images onto every schedulable kind node ahead of time, so a demo's
# wakes show restore time rather than image download time.
# Usage: hack/prepull.sh IMAGE...
set -euo pipefail

cluster="${KIND_CLUSTER:-sbx}"
pids=()
for node in $(kind get nodes --name "$cluster"); do
  [[ "$node" == *control-plane* ]] && continue # tainted: workspaces never run there
  for img in "$@"; do
    echo "pulling $img on $node"
    docker exec "$node" crictl pull "$img" >/dev/null &
    pids+=($!)
  done
done
for pid in "${pids[@]}"; do wait "$pid"; done
