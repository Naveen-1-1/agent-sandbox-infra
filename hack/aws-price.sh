#!/usr/bin/env bash
# Print an instance type's hourly price in us-east-1: the on-demand list
# price, or for SPOT the current average across zones.
# Usage: hack/aws-price.sh INSTANCE_TYPE ON_DEMAND|SPOT [REGION]
set -euo pipefail
itype=${1:?usage: aws-price.sh INSTANCE_TYPE ON_DEMAND|SPOT [REGION]} capacity=${2:-ON_DEMAND} region=${3:-us-east-1}
case "$itype" in
m7g.medium) od=0.0408 ;; m7g.large) od=0.0816 ;; m7g.xlarge) od=0.1632 ;; m7g.2xlarge) od=0.3264 ;;
t4g.large) od=0.0672 ;; t4g.xlarge) od=0.1344 ;; m6g.large) od=0.077 ;; c7g.large) od=0.0725 ;;
*) echo "no list price for $itype; add it to hack/aws-price.sh" >&2; exit 1 ;;
esac
if [[ "$capacity" == SPOT ]]; then
  spot=$(aws ec2 describe-spot-price-history --region "$region" --instance-types "$itype" \
    --product-descriptions Linux/UNIX --start-time "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --query 'SpotPriceHistory[].SpotPrice' --output text | tr '\t' '\n' | awk '$1 > 0 {s+=$1; n++} END {if (n) printf "%.4f", s/n}')
  echo "${spot:-$od}"
else
  echo "$od"
fi
