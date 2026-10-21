#!/usr/bin/env bash
# Print how workspaces should get the agent and editor on this cluster:
# "image" if a pod can mount the runtime image as a volume (Kubernetes 1.35
# and a containerd that supports it), else "copy".
# Usage: hack/aws-preflight.sh RUNTIME_IMAGE
set -euo pipefail
image=${1:?usage: aws-preflight.sh RUNTIME_IMAGE}
name=sbx-image-volume-check
trap 'kubectl delete pod "$name" --ignore-not-found --wait=false >/dev/null 2>&1' EXIT
kubectl delete pod "$name" --ignore-not-found >/dev/null 2>&1
if ! kubectl apply -f - >/dev/null 2>&1 <<EOF2
apiVersion: v1
kind: Pod
metadata:
  name: $name
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  containers:
    - name: check
      image: $image
      command: [sh, -c, "test -x /check/sbx/bin/sbx-agent"]
      volumeMounts: [{name: tools, mountPath: /check, readOnly: true}]
  volumes:
    - name: tools
      image: {reference: "$image", pullPolicy: IfNotPresent}
EOF2
then
  echo copy
  exit 0
fi
for _ in $(seq 90); do
  case "$(kubectl get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null)" in
  Succeeded) echo image; exit 0 ;;
  Failed) echo copy; exit 0 ;;
  esac
  sleep 2
done
echo copy
