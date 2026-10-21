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
- Go ≥ 1.26, kind, helm, kubectl, node ≥ 24 (for the editor extension's build and tests)

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
make demo             # an agent session: sleep after a turn, wake hidden behind thinking
export KUBECONFIG=$PWD/bin/kubeconfig
kubectl -n sbx-system get pods
```

`make demo` prints, for a wake started by a wake-hint and one without, when the pod was
scheduled, when the files were restored, when the workspace was serving, and how long the
agent's first command waited. `DEMO_ARGS="--thinking 5s --keep"` changes the simulated
thinking time and keeps the workspace afterwards.

`make deploy` installs, into `sbx-system`:

- the sbx controller (which serves the broker),
- the gateway, on `http://127.0.0.1:8080` through kind's port mapping,
- upstream agent-sandbox,
- a single-pod SeaweedFS for S3,
- a small git server with test repositories,
- Prometheus and Grafana with the sbx dashboards.

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

### Editor

Every workspace has VS Code in the browser. On kind it is at
`http://<name>.<namespace>.sbx.localhost:8080/`, and `sbxctl editor` prints a sign-in link:

```bash
bin/sbxctl editor sbx-workspaces/demo              # the editor
bin/sbxctl editor --port 3000 sbx-workspaces/demo  # an app port inside the workspace
```

Opening the link wakes a sleeping workspace. An editor tab left open doesn't keep it awake.
Typing, saving and running terminal commands count as activity. Browsers resolve
`*.localhost` to 127.0.0.1 by themselves. Safari is the exception, so use another browser
or add a hosts entry.

### Dependencies and services

A workspace whose repository has a `package-lock.json`, `uv.lock` or `requirements.txt`
gets its `node_modules` or `.venv` restored from a shared cache on every wake. On a cache
miss it is installed, and cached when the workspace next sleeps. Only commands that need it
wait for it:

```bash
bin/sbxctl create --repo http://sbx-fixtures.sbx-system.svc/hello-deps.git --name app \
  --image mcr.microsoft.com/devcontainers/javascript-node:22
bin/sbxctl exec sbx-workspaces/app -- ls          # runs at once
bin/sbxctl exec sbx-workspaces/app -- npm test    # waits for node_modules if needed
bin/sbxctl service start sbx-workspaces/app web -- npm run dev   # restarted after every wake
bin/sbxctl service list sbx-workspaces/app
```

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

### Metrics and dashboards

On kind, `make deploy` also installs Prometheus and Grafana:

```bash
make grafana   # http://127.0.0.1:3000/dashboards?tag=sbx: fleet, wake, storage and cost
```

The dashboards show workspaces by state, wake latency (files, serving, dependencies),
held requests, snapshot and dependency sizes, the dependency cache hit rate, and what the
fleet costs per hour at the list prices under `pricing:` in `charts/sbx/values.yaml`. Set
`monitoring.enabled: false` to use the cluster's own Prometheus operator and Grafana. Then
`metrics.serviceMonitors` and `metrics.dashboards` install just the ServiceMonitors and
dashboards.

## Development

| Command | What it does |
|---|---|
| `make test` | Unit tests with `-race` and goroutine-leak checks, and the editor extension's |
| `make extension` | Type-check, test and build the editor activity extension |
| `make lint` | golangci-lint (tools are pinned in `tools.mod`) |
| `make test-e2e` | End-to-end tests against the deployed cluster (`RUN=<regex>`) |
| `make demo` | Play an agent session against the deployed cluster |
| `make grafana` | Open Grafana with the sbx dashboards |
| `make gen` / `make verify-gen` | Regenerate CRDs and gRPC code / check they're current |
| `make kind-up` / `make kind-down` | Create / delete the local cluster |
| `make deploy` / `make undeploy` | Install / remove the platform |
| `make verify-vendor` | Check the vendored upstream chart matches v1.0.7 |

`make help` lists every target.
