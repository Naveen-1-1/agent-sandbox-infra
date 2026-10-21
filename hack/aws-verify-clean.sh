#!/usr/bin/env bash
# Check that nothing of a run is left in AWS after its teardown: no nodes,
# NAT gateways, load balancers, disks, addresses, network interfaces,
# images, cluster, network or bucket. Fails if anything is.
# Usage: hack/aws-verify-clean.sh RUN REGION CLUSTER [VPC_ID]
set -euo pipefail
run=${1:?} region=${2:?} cluster=${3:?} vpc=${4:-}
left=0
check() { # what, count
  if [[ "$2" == 0 || -z "$2" || "$2" == None ]]; then
    printf '  ok    %s\n' "$1"
  else
    printf '  LEFT  %s: %s\n' "$1" "$2"
    left=1
  fi
}
a() { aws --region "$region" "$@"; }
tag="Name=tag:run,Values=$run"

echo "Checking that run $run is gone from $region:"
check "EC2 instances" "$(a ec2 describe-instances --filters "$tag" Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down \
  --query 'length(Reservations[].Instances[])' --output text)"
check "EBS volumes" "$(a ec2 describe-volumes --filters "$tag" --query 'length(Volumes)' --output text)"
check "EBS volumes of the cluster" "$(a ec2 describe-volumes --filters "Name=tag-key,Values=kubernetes.io/cluster/$cluster" --query 'length(Volumes)' --output text)"
check "NAT gateways" "$(a ec2 describe-nat-gateways --filter "$tag" Name=state,Values=pending,available,deleting \
  --query 'length(NatGateways)' --output text)"
check "Elastic IPs" "$(a ec2 describe-addresses --filters "$tag" --query 'length(Addresses)' --output text)"
if [[ -n "$vpc" ]]; then
  check "the VPC" "$(a ec2 describe-vpcs --filters "Name=vpc-id,Values=$vpc" --query 'length(Vpcs)' --output text)"
  check "network interfaces in the VPC" "$(a ec2 describe-network-interfaces --filters "Name=vpc-id,Values=$vpc" --query 'length(NetworkInterfaces)' --output text)"
  check "load balancers in the VPC" "$(a elbv2 describe-load-balancers --query "length(LoadBalancers[?VpcId=='$vpc'])" --output text)"
  check "classic load balancers in the VPC" "$(a elb describe-load-balancers --query "length(LoadBalancerDescriptions[?VPCId=='$vpc'])" --output text)"
fi
check "VPCs tagged with the run" "$(a ec2 describe-vpcs --filters "$tag" --query 'length(Vpcs)' --output text)"
check "ECR repositories" "$(a ecr describe-repositories --query "length(repositories[?starts_with(repositoryName, 'sbx-$run/')])" --output text)"
if a eks describe-cluster --name "$cluster" >/dev/null 2>&1; then check "the EKS cluster" 1; else check "the EKS cluster" 0; fi
buckets=$(aws s3api list-buckets --query "length(Buckets[?starts_with(Name, 'sbx-$run-')])" --output text)
check "S3 buckets" "$buckets"
if [[ $left == 0 ]]; then echo "Nothing left."; else echo "Something is left; see above." >&2; fi
exit $left
