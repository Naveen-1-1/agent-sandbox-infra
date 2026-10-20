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
- upstream agent-sandbox,
- a single-pod SeaweedFS for S3,
- a small git server with test repositories.

Every endpoint, credential, storage class and image registry is a Helm value in
`charts/sbx/values.yaml`. `deploy/values/kind.yaml` holds the local overrides.

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
