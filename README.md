# agent-sandbox-infra

Agent sandboxes on Kubernetes that **sleep cheaply and wake invisibly**. A sleeping sandbox
costs only a few kilobytes in object storage: its workspace is saved as a git bundle in S3,
then its pod and disk are deleted. When the user messages the agent, the sandbox starts
restoring while the LLM is still thinking, so it is usually ready before the first tool call.

Built on [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) v1.0.7
(extended, not forked). See [DESIGN.md](DESIGN.md) for how it works.

> Status: under construction (MVP in progress).

## Requirements

- macOS or Linux with Docker (6 CPUs / 10 GB RAM recommended for the full stack)
- Go ≥ 1.26, kind, helm, kubectl, node ≥ 20

```bash
brew install go kind helm kubectl node
```

Check everything with:

```bash
make doctor
```

## Quick start

```bash
make kind-up          # 3-node kind cluster + local image registry on localhost:5001
make deploy           # build images, push them, install the chart (charts/sbx)
export KUBECONFIG=$PWD/bin/kubeconfig
kubectl -n sbx-system get pods
```

`make deploy` installs, into `sbx-system`:

- the sbx controller (which serves the broker),
- the gateway, on `http://127.0.0.1:8080` through kind's port mapping,
- upstream agent-sandbox,
- a single-pod SeaweedFS for S3,
- a small git server with test repositories.

Every endpoint, credential, storage class and image registry is a Helm value in
`charts/sbx/values.yaml`. `deploy/values/kind.yaml` holds the local overrides.

## Using a workspace

`sbxctl` talks to the gateway. The admin token is in the `sbx-gateway-admin` Secret.

```bash
go build -o bin/sbxctl ./cmd/sbxctl
export SBX_TOKEN=$(kubectl -n sbx-system get secret sbx-gateway-admin -o jsonpath='{.data.token}' | base64 -d)
bin/sbxctl create --repo http://sbx-fixtures.sbx-system.svc/hello-node.git --name demo
bin/sbxctl wait sbx-workspaces/demo Running
bin/sbxctl exec sbx-workspaces/demo -- cat README.md
bin/sbxctl suspend sbx-workspaces/demo        # snapshot to S3, delete the pod and disk
bin/sbxctl exec sbx-workspaces/demo -- ls     # held while it wakes, then runs
bin/sbxctl wake-hint sbx-workspaces/demo      # start waking now, return at once
bin/sbxctl list
```

`create` prints a token scoped to that one workspace. An agent harness uses it in place of
the admin token.

### Sleep policy

Workspaces go to sleep on their own. The chart's default policy, `fixed`, suspends after
10 minutes without activity, or 2 minutes after the agent ends its turn
(`sbxctl turn-ended`). A running command keeps the workspace awake. `sbxctl list` shows
each verdict, and `sbxctl get` explains it.

```bash
# One workspace with its own policy:
bin/sbxctl create --repo http://sbx-fixtures.sbx-system.svc/hello-node.git \
  --policy fixed --param idleTimeout=1m --param afterTurnEnded=10s
# Change the cluster policy at runtime (takes effect at once; survives upgrades):
kubectl -n sbx-system create configmap sbx-policy --from-literal=policy.yaml='name: fixed
params: {idleTimeout: 5m}'
```

The chart's defaults are under `policy:` in `charts/sbx/values.yaml`. A broken
`sbx-policy` is reported as an Event on the ConfigMap and ignored.

## Development

| Command | What it does |
|---|---|
| `make test` | Unit tests with `-race` and goroutine-leak checks |
| `make lint` | golangci-lint (tools are pinned in `tools.mod`) |
| `make test-e2e` | End-to-end tests against the deployed cluster (`RUN=<regex>`) |
| `make gen` / `make verify-gen` | Regenerate CRDs and gRPC code / check they're current |
| `make kind-up` / `make kind-down` | Create / delete the local cluster |
| `make deploy` / `make undeploy` | Install / remove the platform |
| `make verify-vendor` | Check the vendored upstream chart matches v1.0.7 |

`make help` lists every target.
