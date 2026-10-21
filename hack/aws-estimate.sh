#!/usr/bin/env bash
# Estimate what an AWS plan costs per hour while it runs, at us-east-1 and
# us-east-2 list prices (the same in both): the EKS control plane, the nodes (spot at the current price), their
# disks and public IPv4 addresses, and one workspace disk. S3 and ECR hold a
# few hundred MB to a few GB: under a cent a day. No NAT gateway or load
# balancer is planned (it says so if one is).
# Usage: hack/aws-estimate.sh PLAN.json
set -euo pipefail
plan=${1:?usage: aws-estimate.sh PLAN.json}

created() { jq -r --arg t "$1" '[.resource_changes[] | select(.type == $t and (.change.actions | index("create")))] | length' "$plan"; }
ng() { jq -r ".resource_changes[] | select(.type == \"aws_eks_node_group\") | .change.after | $1" "$plan"; }

nodes=$(ng '.scaling_config[0].desired_size')
itype=$(ng '.instance_types[0]')
capacity=$(ng '.capacity_type')
region=$(jq -r '.variables.region.value // "us-east-1"' "$plan")
disk=$(jq -r '[.resource_changes[] | select(.type == "aws_launch_template") | .change.after.block_device_mappings[0].ebs[0].volume_size][0]' "$plan")
clusters=$(created aws_eks_cluster)
nat=$(created aws_nat_gateway)
lbs=$(($(created aws_lb) + $(created aws_elb)))

price=$(hack/aws-price.sh "$itype" "$capacity" "$region")

awk -v clusters="$clusters" -v nodes="$nodes" -v itype="$itype" -v capacity="$capacity" -v price="$price" \
  -v disk="$disk" -v nat="$nat" -v lbs="$lbs" 'BEGIN {
  gp3 = 0.08 / 730; ipv4 = 0.005
  row("EKS control plane", clusters, 0.10)
  row(sprintf("%s nodes (%s)", itype, tolower(capacity)), nodes, price)
  row(sprintf("node root disks, %d GiB gp3", disk), nodes, disk * gp3)
  row("public IPv4 addresses (nodes)", nodes, ipv4)
  row("workspace disk, 10 GiB gp3 (one bench sandbox)", 1, 10 * gp3)
  row("NAT gateways", nat, 0.045)
  row("load balancers", lbs, 0.0225)
  printf "%-52s %10s\n", "", "--------"
  printf "%-52s %9.4f/h\n", "Estimated total", total
  printf "%-52s %9.2f\n", "A 2-hour run (create, push, bench, tear down)", 2 * total
  print "S3, ECR and CloudWatch add well under a cent an hour; data stays in the region."
}
function row(what, n, each) {
  if (n == 0) return
  printf "%-52s %3d x %8.4f = %8.4f/h\n", what, n, each, n * each
  total += n * each
}'
