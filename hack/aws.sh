#!/usr/bin/env bash
# The AWS environment's lifecycle, behind the aws-* make targets:
#
#   plan        plan the environment and estimate its hourly cost (changes nothing)
#   apply       create it from the saved plan            (APPROVED=yes)
#   deploy      push the images and install the platform on it
#   bench       run the benchmark on it, and record what the metrics say it cost
#   down-plan   plan the teardown (changes nothing)
#   down        tear it down from the saved plan, then check nothing is left (APPROVED=yes)
#   verify      check nothing of a run is left
#   cost-actual what Cost Explorer billed the run (FROM=YYYY-MM-DD [TO=YYYY-MM-DD])
#
# Credentials come from your AWS CLI setup (profile, aws login, SSO) and are
# never printed. Everything is in one region (AWS_REGION, by default your
# profile's) and tagged project=sbx, run=<run>.
set -euo pipefail
cd "$(dirname "$0")/.."

tfdir=infra/terraform
tfvars=$tfdir/run.auto.tfvars
ns=${NAMESPACE:-sbx-system}
export KUBECONFIG=$PWD/bin/kubeconfig-aws
mkdir -p bin notes

tf() { terraform -chdir="$tfdir" "$@"; }
out() { tf output -raw "$1"; }
say() { printf '\n== %s\n' "$*"; }
saved_run() { if [[ -f "$tfvars" ]]; then sed -n 's/^run *= *"\(.*\)"/\1/p' "$tfvars"; fi; }
approve() {
  if [[ "${APPROVED:-}" != yes ]]; then
    echo "Not running $1 without APPROVED=yes. Read the plan above first." >&2
    exit 1
  fi
}

cmd_plan() {
  local run existing ip
  run=${RUN:-}
  existing=$(saved_run)
  if [[ -n "$existing" ]]; then
    if [[ -n "$run" && "$run" != "$existing" ]]; then
      echo "This checkout already manages run $existing; tear it down (make aws-down) before planning $run." >&2
      exit 1
    fi
    run=$existing
  fi
  run=${run:-bench-$(date +%Y%m%d)}
  ip=$(curl -fsS https://checkip.amazonaws.com | tr -d '[:space:]')
  cat >"$tfvars" <<EOF
# Written by make aws-plan; git-ignored.
run               = "$run"
region            = "${AWS_REGION:-us-east-1}"
node_count        = ${AWS_NODES:-2}
instance_type     = "${AWS_INSTANCE_TYPE:-m7g.large}"
capacity_type     = "${AWS_CAPACITY:-ON_DEMAND}"
api_allowed_cidrs = ["$ip/32"]
EOF
  say "Planning run $run (Kubernetes API open to $ip only)"
  tf init -input=false >/dev/null
  tf plan -input=false -out=up.tfplan
  tf show -json up.tfplan >"$tfdir/up.tfplan.json"
  hack/aws-estimate.sh "$tfdir/up.tfplan.json"
  echo
  echo "To create it: make aws-up APPROVED=yes"
}

cmd_apply() {
  approve "terraform apply"
  [[ -f "$tfdir/up.tfplan" ]] || { echo "No saved plan: run make aws-plan first." >&2; exit 1; }
  say "Creating run $(saved_run)"
  tf apply -input=false up.tfplan
  rm -f "$tfdir/up.tfplan" "$tfdir/up.tfplan.json"
}

cmd_deploy() {
  local region cluster registry bucket arch tag tools price cpu mem
  region=$(out region) cluster=$(out cluster_name) registry=$(out registry) bucket=$(out bucket)
  say "Connecting to $cluster"
  aws eks update-kubeconfig --name "$cluster" --region "$region" --kubeconfig "$KUBECONFIG" >/dev/null
  kubectl get nodes -o wide
  kubectl apply -f deploy/aws/storageclass-gp3.yaml

  arch=$(out arch)
  say "Pushing $arch images to $registry"
  aws ecr get-login-password --region "$region" | docker login --username AWS --password-stdin "${registry%%/*}" >/dev/null
  make push ARCH="$arch" REGISTRY="$registry" IMAGES="sbx-runtime sbx-controller sbx-gateway"

  tag=$(hack/image-tag.sh "$arch")
  tools=$(hack/aws-preflight.sh "$registry/sbx-runtime:$tag")
  # Price CPU and memory so that 2 vCPU and 8 GiB cost what the node does
  # (two thirds CPU, one third memory, as EC2's general-purpose types).
  price=$(hack/aws-price.sh "$(out instance_type)" ON_DEMAND "$region")
  cpu=$(awk -v p="$price" 'BEGIN {printf "%.6f", p / 3}')
  mem=$(awk -v p="$price" 'BEGIN {printf "%.6f", p / 24}')
  say "Installing the platform (workspace tools: $tools; \$$cpu per vCPU-hour, \$$mem per GiB-hour)"
  make deploy ARCH="$arch" REGISTRY="$registry" VALUES=deploy/values/aws.yaml IMAGES="sbx-runtime sbx-controller sbx-gateway" \
    DEPLOY_ARGS="--set s3.bucket=$bucket --set s3.region=$region --set workspace.tools=$tools --set pricing.cpuCoreHour=$cpu --set pricing.memoryGiBHour=$mem"

  say "Turning on the cost allocation tags (once per account; Cost Explorer may need up to a day to see them)"
  aws ce update-cost-allocation-tags-status --region us-east-1 \
    --cost-allocation-tags-status TagKey=project,Status=Active TagKey=run,Status=Active >/dev/null 2>&1 ||
    echo "Not yet: the tags aren't known to billing yet. make aws-cost-actual tries again." >&2
  echo
  echo "Gateway: kubectl --kubeconfig $KUBECONFIG -n $ns port-forward svc/sbx-gateway 8080:80"
}

cmd_bench() {
  local region bucket run pf start end
  region=$(out region) bucket=$(out bucket) run=$(out run)
  # On x86, torch from PyPI is the CUDA build (about 5 GB with its NVIDIA
  # libraries): too big to cache, so the ML repository is left out there.
  if [[ "$(out arch)" == amd64 && " $* " != *" --repos "* ]]; then
    echo "x86 nodes: benchmarking graphql-js and pluggy (vggt's CUDA torch is too big to cache)"
    set -- --repos graphql-js,pluggy "$@"
  fi
  kubectl -n "$ns" port-forward svc/sbx-gateway 18080:80 >/dev/null &
  pf=$!
  trap 'kill $pf 2>/dev/null' EXIT
  curl -s -o /dev/null --retry 30 --retry-connrefused --retry-delay 1 http://127.0.0.1:18080/
  start=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  go build -o bin/sbx-bench ./cmd/sbx-bench
  SBX_GATEWAY=http://127.0.0.1:18080 NAMESPACE="$ns" hack/bench.sh --env aws \
    --s3-endpoint "https://s3.$region.amazonaws.com" --s3-bucket "$bucket" --s3-region "$region" --s3-path-style=false "$@"
  end=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  hack/aws-modeled-cost.sh "$run" "$start" "$end" "notes/aws-modeled-$run.json"
}

cmd_down_plan() {
  say "Planning the teardown of run $(saved_run)"
  tf plan -destroy -input=false -out=down.tfplan
  echo
  echo "To tear it down: make aws-down APPROVED=yes"
}

cmd_down() {
  approve "the teardown"
  [[ -f "$tfdir/down.tfplan" ]] || { echo "No saved teardown plan: run make aws-down-plan first." >&2; exit 1; }
  local run region cluster vpc
  run=$(out run) region=$(out region) cluster=$(out cluster_name) vpc=$(out vpc_id)
  if kubectl get namespace "$ns" >/dev/null 2>&1; then
    say "Deleting workspaces, so their disks and snapshots go first"
    kubectl delete workspaces.sbx.dev --all --all-namespaces --wait --timeout=10m || true
    hack/aws-modeled-cost.sh "$run" "" "" "notes/aws-modeled-$run.json" --lifetime || true
    say "Uninstalling the platform"
    helm uninstall sbx --namespace "$ns" --wait --timeout 10m || true
    kubectl delete namespace sbx-workspaces --wait --timeout=5m --ignore-not-found || true
  fi
  say "Waiting for workspace disks to be deleted"
  for _ in $(seq 60); do
    [[ "$(aws ec2 describe-volumes --region "$region" --filters "Name=tag:run,Values=$run" "Name=tag-key,Values=CSIVolumeName" \
      --query 'length(Volumes)' --output text)" == 0 ]] && break
    sleep 5
  done
  say "Destroying run $run"
  tf apply -input=false down.tfplan
  rm -f "$tfdir/down.tfplan"
  hack/aws-verify-clean.sh "$run" "$region" "$cluster" "$vpc"
  mv "$tfvars" "$tfdir/run-$run.done" 2>/dev/null || true
}

cmd_verify() {
  local run=${RUN:-$(saved_run)}
  hack/aws-verify-clean.sh "$run" "${AWS_REGION:-us-east-1}" "sbx-$run" "${VPC:-}"
}

cmd_cost_actual() {
  local run=${RUN:-$(saved_run)} from=${FROM:?set FROM=YYYY-MM-DD, the day the run started} to=${TO:-}
  [[ -n "$run" ]] || { echo "set RUN=<run>" >&2; exit 1; }
  to=${to:-$(date -u -v+1d +%Y-%m-%d 2>/dev/null || date -u -d tomorrow +%Y-%m-%d)}
  if ! aws ce list-cost-allocation-tags --region us-east-1 --tag-keys run --status Active --query 'length(CostAllocationTags)' --output text | grep -q '^1$'; then
    echo "The run tag isn't an active cost allocation tag yet; activating it and backfilling this month." >&2
    aws ce update-cost-allocation-tags-status --region us-east-1 --cost-allocation-tags-status TagKey=project,Status=Active TagKey=run,Status=Active
    aws ce start-cost-allocation-tag-backfill --region us-east-1 --backfill-from "${from:0:8}01T00:00:00Z" >/dev/null || true
    echo "Billing applies it within a day; run this again tomorrow." >&2
    exit 1
  fi
  aws ce get-cost-and-usage --region us-east-1 --time-period "Start=$from,End=$to" --granularity DAILY \
    --metrics UnblendedCost --filter "{\"Tags\":{\"Key\":\"run\",\"Values\":[\"$run\"]}}" \
    --group-by Type=DIMENSION,Key=SERVICE --output json |
    jq --arg run "$run" --arg from "$from" --arg to "$to" '{run: $run, start: $from, end: $to,
      byService: (reduce (.ResultsByTime[].Groups[]) as $g ({}; .[$g.Keys[0]] += ($g.Metrics.UnblendedCost.Amount | tonumber)))}
      | .totalUsd = ([.byService[]] | add // 0)' >"notes/aws-cost-$run.json"
  echo "wrote notes/aws-cost-$run.json"
  jq . "notes/aws-cost-$run.json"
}

cmd=${1:?usage: hack/aws.sh plan|apply|deploy|bench|down-plan|down|verify|cost-actual}
shift
case "$cmd" in
plan) cmd_plan ;;
apply) cmd_apply ;;
deploy) cmd_deploy ;;
bench) cmd_bench "$@" ;;
down-plan) cmd_down_plan ;;
down) cmd_down ;;
verify) cmd_verify ;;
cost-actual) cmd_cost_actual ;;
*) echo "unknown command $cmd" >&2; exit 2 ;;
esac
