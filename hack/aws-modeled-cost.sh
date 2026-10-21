#!/usr/bin/env bash
# What the platform's own numbers say a run cost, to set next to the bill:
# the workspaces (sbx_cost_usd_per_hour) and object storage
# (sbx_object_store_cost_usd_per_hour) summed over the window from
# Prometheus, and the nodes and control plane at list price.
# Usage: hack/aws-modeled-cost.sh RUN START END OUT [--lifetime]
# START and END are UTC times like 2026-10-21T15:00:00Z. With --lifetime
# the window runs from the cluster's creation until now instead.
set -euo pipefail
run=${1:?} start=${2:-} end=${3:-} outfile=${4:?} mode=${5:-}
ns=${NAMESPACE:-sbx-system}
tf() { terraform -chdir=infra/terraform output -raw "$1"; }
region=$(tf region) cluster=$(tf cluster_name) nodes=$(tf node_count) itype=$(tf instance_type) capacity=$(tf capacity_type)
if [[ "$mode" == --lifetime ]]; then
  start=$(aws eks describe-cluster --name "$cluster" --region "$region" --query cluster.createdAt --output text)
  end=$(date -u +%Y-%m-%dT%H:%M:%SZ)
fi
price=$(hack/aws-price.sh "$itype" "$capacity" "$region")

log=$(mktemp "${TMPDIR:-/tmp}/sbx-forward.XXXXXX")
kubectl -n "$ns" port-forward svc/sbx-monitoring-prometheus 19090:9090 >"$log" 2>&1 &
pf=$!
trap 'kill $pf 2>/dev/null' EXIT
for i in $(seq 61); do
  curl -sf -o /dev/null --max-time 5 http://127.0.0.1:19090/-/ready && break
  if [[ $i == 61 ]] || ! kill -0 "$pf" 2>/dev/null; then
    echo "Can't reach Prometheus through a port-forward:" >&2
    cat "$log" >&2
    exit 1
  fi
  sleep 1
done

python3 - "$run" "$start" "$end" "$outfile" "$nodes" "$price" <<'PY'
import json, sys, urllib.parse, urllib.request
from datetime import datetime
run, start, end, outfile, nodes, price = sys.argv[1:7]
t0 = datetime.fromisoformat(start.replace("Z", "+00:00")).timestamp()
t1 = datetime.fromisoformat(end.replace("Z", "+00:00")).timestamp()
dur = max(int(t1 - t0), 60)

def integral(expr):
    # Sample every 30s over the window, then sum samples × 30s in hours.
    q = f"sum_over_time(({expr})[{dur}s:30s]) * 30 / 3600"
    url = "http://127.0.0.1:19090/api/v1/query?" + urllib.parse.urlencode({"query": q, "time": t1})
    res = json.load(urllib.request.urlopen(url, timeout=30))["data"]["result"]
    return float(res[0]["value"][1]) if res else 0.0

hours = dur / 3600
out = {
    "run": run, "start": start, "end": end,
    "workspacesUsd": integral("sum(max by (phase) (sbx_cost_usd_per_hour))"),
    "storageUsd": integral("max(sbx_object_store_cost_usd_per_hour)"),
    "nodeHours": int(nodes) * hours,
    "nodeUsd": int(nodes) * hours * float(price),
    "controlPlaneUsd": hours * 0.10,
}
json.dump(out, open(outfile, "w"), indent=2)
print(f"wrote {outfile}: workspaces ${out['workspacesUsd']:.4f}, storage ${out['storageUsd']:.6f}, "
      f"{out['nodeHours']:.2f} node-hours ${out['nodeUsd']:.4f}, control plane ${out['controlPlaneUsd']:.4f}")
PY
