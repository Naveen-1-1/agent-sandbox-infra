# Design

## Goal

Agent sandboxes should cost almost nothing while idle and feel always-on to the user.
Idle sandboxes are **suspended**: their workspace is committed as a git bundle to S3, then
the pod and its disk are deleted. A **wake** brings up a fresh pod that restores the files
first and the dependencies in the background, ideally while the LLM is still thinking.

## Building on agent-sandbox

[agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) v1.0.7 provides the
`Sandbox` resource: one pod, one service, and `spec.operatingMode: Running|Suspended`.
Suspending deletes the pod immediately but keeps the disk. We add what it lacks: saving
the workspace off-cluster before the pod goes away, deleting the disk afterwards, waking on
demand, and deciding when to sleep. We use upstream's controller, its in-pod exec/files
server (sandboxd) and its Helm chart unmodified.

## Components

### Workspace API (`api/v1alpha1`, `sbx.dev/v1alpha1`)

A `Workspace` is the long-lived identity of a sandbox. Its spec names the source repository
(immutable), an optional image, a resource profile and a `mode` (`Auto`, `AlwaysOn`, `Off`).
Its status records where it is in the sleep/wake cycle. Ownership is split so two writers
never fight. The user owns `mode`. The gateway owns only `spec.wake.seq`, a counter it
bumps to ask for a wake (the API rejects decreases). The controller owns all of status:
`phase`, `epoch` (one per wake, naming that wake's pod, disk and tokens), the in-progress
suspend or wake, the last committed snapshot, and a per-stage wake timeline.

### gRPC contracts (`proto/sbx`, generated into `gen/go`)

Components talk over gRPC; the Kubernetes API holds the durable state. The controller drives
each pod's **sidecar** (start or rejoin a snapshot by attempt ID, identify the pod's epoch,
stream restore milestones) and **agent** (freeze, thaw, report activity, run declared
services). Pods call the **broker** with a per-epoch token to get presigned URLs for their
own workspace's objects only. The controller asks each **gateway** replica for traffic
signals in one batched call. The shared `Signals` message is the policy's input: activity,
running commands, open streams by kind, held requests and turn-ended. Code is generated with
buf and plugins pinned in `tools.mod`; `make verify-gen` fails if it is stale.

### Snapshot (`internal/snapshot`)

Turns a workspace into a git bundle and back. A snapshot is two synthetic commits laid out
like `git stash`: one holds the working tree (tracked and untracked files, minus gitignored
and excluded paths such as `.env`, keys and `node_modules`), the other holds the staging area.
The bundle carries those commits plus every local branch and tag, minus anything the
upstream base commit already has, so it contains only the agent's changes. Commits use a
fixed author and the freeze time, so retrying a snapshot of the same state produces the same
commit IDs. Snapshotting never writes to the user's `.git`: new objects go to a throwaway
"shadow" repository that reads the user's objects through git alternates. Restore fetches the
base (from a cached base bundle, or from origin), applies the bundle, puts HEAD back, writes
the working tree, then restores the staging area on top. Shallow clones always fetch their
base from origin, because git can't restore a bundle cut from a shallow repository into an
empty one.

Before writing the bundle, the snapshot checks exactly what it would carry: every object
the base doesn't have. Known secret files (`.env`, keys, `.npmrc`, cloud and kube configs)
are dropped from the working tree and staging area; templates like `.env.example` and
public keys are kept. A secret file inside one of the agent's own commits fails the
snapshot, because rewriting someone's history isn't safe. New files are scanned with
high-confidence rules (private keys, AWS, GitHub, Slack, Google, Stripe, Anthropic and
OpenAI keys). Size limits apply per file, to all new content, and to the bundle. Any
violation returns a `PolicyError` naming the files (never the secret), and the workspace
keeps running. Credentials embedded in a remote URL are used for fetching but never stored
in the restored repository or shown in errors.

### Controller (`cmd/sbx-controller`, `internal/controller/workspace`)

A level-triggered state machine over `Workspace`. Each pass does one idempotent step and
records it in status with an optimistic-concurrency update. A controller that crashes after
any side effect simply redoes the current step, so the reconciler keeps no memory of its
own. Leader election makes one replica run it; the broker runs on every replica.

```
Pending ─► Provisioning ─► Running ─► Suspending ─► TearingDown ─► Suspended ─► Waking ─► Running
                              ▲            │ abort (wake, AlwaysOn,                          │
                              └────────────┘ pod restart, timeout, failure)                  │
                              ▲                                                               │
                              └───────────────────────────────────────────────────────────────┘
```

- **Provisioning** pins the base commit (`git ls-remote`), sets epoch 1, and creates the
  token Secret, the protected disk and the Sandbox.
- **Suspending** runs these steps. Freeze (with a lease) → snapshot in the sidecar →
  commit the manifest in S3 → re-freeze without a lease → **commit point**. The commit point
  is a status write that fails if a wake request (`spec.wake.seq`) arrived meanwhile; any
  failure before it thaws the workload and returns it to Running. A failed snapshot sets
  `SnapshotFailed` with the reason (for example a detected secret), backs off exponentially,
  and never deletes the pod or disk.
- **TearingDown** cannot go back. It sets the Sandbox to Suspended, waits for the pod to go,
  then removes our finalizer from the disk and deletes it.
- **Waking** advances the epoch first, so the new pod's tokens are the ones the broker
  accepts. It creates that epoch's Secret and disk, and patches the template and `Running`
  in one write. The resourceVersion-checked status write that starts a wake turns any
  number of concurrent wake requests into exactly one.
- **Deleting** removes the Sandbox, disks and Secrets, and purges the workspace's S3
  prefix. `sbx.dev/paused` makes the controller skip a workspace.

Tests run upstream's real Sandbox controller with a fake kubelet. One test crashes the
controller after each of the 28 side effects of a create/suspend/wake cycle and checks:
no disk is deleted before its snapshot is committed, there is exactly one wake, the
manifest matches status, and only one disk exists at a time. Metrics cover workspaces per
phase, time in each phase, wake latency to files-ready and serving, restore bytes, and wake
and suspend counts.

### Workspace pod (`internal/controller/podspec`)

One epoch of a workspace is one upstream `Sandbox` pod, built by a pure function and
pinned by a golden file. Its containers start in order:

1. `sbx-tools` copies our binaries into a shared volume.
2. `sbx-restore` puts the files on this epoch's disk.
3. `sbx-sidecar` runs as a native sidecar.
4. The user's image runs with `sbx-agent` as PID 1, mounting the disk at `/workspace`.

Kubernetes guarantees the workload never starts before its files exist. The per-epoch
token Secret is mounted only into our containers. The workload gets just the hash of its
agent token, has no service-account token, and our containers drop all capabilities.

### Agent (`cmd/sbx-agent`, `internal/agent`)

PID 1 of the workload container, which runs the user's devcontainer image. It re-executes
itself: PID 1 only reaps orphaned processes and forwards signals, and the child does the
work. The child embeds upstream sandboxd's exec (gRPC) and files (REST) APIs unmodified,
behind a gate of its own interceptors. The gate records activity, running commands and open
exec streams for the sleep policy. During a freeze it refuses new exec calls and file writes
with a "frozen" marker the gateway understands; reads still work. **Freeze** closes the
gate, waits for file writes already in flight, then sends SIGSTOP to every other process in
the container, rescanning `/proc` until nothing new appears so a process forked mid-scan is
caught. A freeze with a lease thaws itself if the controller disappears before committing;
once a suspend is committed, the controller re-freezes without a lease. All calls need the
per-epoch agent token except `/v1/health`.

### Sidecar (`cmd/sbx-sidecar`, `internal/sidecar`)

Our container in every workspace pod, next to the user's image, which can be any
devcontainer. It runs in three ways:

- **`install-tools`** (first init container) copies our binaries into a volume the workload
  container mounts.
- **`restore`** (second init container) asks the broker for this epoch's restore plan,
  downloads the base bundle (or fetches the base from origin) and the latest snapshot,
  checking the snapshot's checksum. It restores into a staging directory on the disk and
  moves it into place. Because it's an init container, the workload starts only after the
  files are there. `.sbx/volume.json` records the outcome: a `live` volume is never restored
  over, and a half-finished restore is wiped and redone.
- **`serve`** (native sidecar) takes snapshots for the controller. Attempts are keyed by
  ID, run one at a time, and have their result written to the disk, so a restarted
  controller or sidecar can rejoin an attempt. It uploads the bundle through a presigned URL
  and caches the base commit in S3 in the background, so later wakes don't need the upstream
  remote.

### Broker (`internal/broker`, served by the controller)

The only way a pod reaches S3. A pod presents its broker token, workspace and epoch. The
broker reads the Workspace live from the API server, verifies the token, and refuses any
epoch but the current one, so a pod left from an earlier wake is locked out. It then hands
out short-lived presigned URLs. Downloads cover the cached base and the last committed
snapshot. Uploads are create-only and checksum-checked under that workspace's own prefix,
with the bundle size limit enforced up front.

### Gateway (`cmd/sbx-gateway`, `internal/gateway`, API in `internal/sbxapi`)

The front door for agents, browsers and `sbxctl`. It serves a REST API: create, list, status,
exec, files, wake, wake-hint, turn-ended and suspend. Each call needs either the admin token
or that workspace's own token. It answers from an informer cache of Workspaces. A call that
needs the pod while the workspace is asleep is *held*:

- The gateway asks for a wake by bumping `spec.wake.seq`. It bumps only when no wake is
  pending, reading the Workspace live and patching with a resourceVersion precondition.
  So any number of concurrent requests and hints, on any number of replicas, make exactly
  one wake.
- Held requests wait on a per-workspace broadcast that informer updates trigger. Per-workspace
  and global limits answer overflow with `503 queue_full` and `Retry-After`. A timeout answers
  `504`. A client that goes away leaves at once.
- If the agent says FROZEN (a suspend started after the check), nothing ran. The gateway
  asks for a wake, which aborts the suspend before its commit point or wakes it straight
  after, then retries once.
- A wake-hint starts the wake and returns at once. Hints are rate-limited and dropped under
  load, because the real request will wake the workspace anyway.

Status reads, `/healthz`, `/readyz` and `/metrics` never wake anything; the health and
metrics endpoints are on a separate admin port. Each replica also keeps the traffic signals
the sleep policy reads: last activity, turn started and ended, open streams and held
requests. It serves them over gRPC to the controller.

### Tokens (`internal/token`)

Every per-epoch token is `HMAC(master key, purpose | workspace UID | epoch)`. The purposes
are sidecar, agent and broker. The controller and gateway hold the master key and recompute
tokens on demand, so nothing is stored. A pod receives only its own epoch's tokens, so a
compromised workspace can't mint tokens for another workspace or another epoch. The
gateway token, which clients use, is derived with epoch 0 so that it survives wakes. It
is never given to the pod.

### Snapshot store (`internal/snapstore`)

Owns the S3 layout. `ws/<uid>/b/<sha256>.bundle` holds bundles, written once and content
addressed. `bases/<url-hash>/<sha>.bundle` holds cached upstream bases. Each workspace has
one `ws/<uid>/manifest.json` naming its latest committed snapshot. A commit states the
sequence number it builds on. It first verifies the bundle is in S3 with the reported size
and checksum, then replaces the manifest with a compare-and-swap. Retrying an attempt that
already landed is a no-op. A commit that builds on an old sequence number, or comes from an
older epoch (a stale pod), gets `ErrStale`, so a newer snapshot is never overwritten. With 16
concurrent commits on SeaweedFS, exactly one wins.

### Object store (`internal/objstore`)

The small slice of S3 the platform uses. It covers immutable blobs written create-only
(`If-None-Match: *`), a manifest replaced only by compare-and-swap (`If-Match: <etag>`),
SHA-256 checksums the server verifies, and presigned URLs, so pods move bundles without
ever holding S3 credentials. An in-memory fake has the same semantics for unit tests. One
contract test suite runs against both the fake and the real endpoint, which is how we know
the fake is honest. Locally the endpoint is SeaweedFS (`charts/s3-local`); with 32
concurrent writers, exactly one create-only write and one compare-and-swap win.

### Packaging (`charts/sbx`, `images/`)

One umbrella chart installs:

- the controller;
- the gateway (two replicas);
- upstream agent-sandbox (vendored and unmodified; can be disabled);
- optionally, SeaweedFS and a test-only git fixtures server.

The token master key is generated once and kept across upgrades. Images are built from
binaries cross-compiled on the host and tagged with a content hash, so an upgrade only
rolls what changed. There are four images:

- `sbx-runtime` (sidecar, agent, git) for workspace pods;
- `sbx-controller`;
- `sbx-gateway` (distroless);
- `sbx-fixtures` (git over plain HTTP) for tests and the demo.

## Key decisions

- **No git server.** A snapshot is a git bundle containing only the agent's changes
  relative to the upstream base commit, stored in S3 under a content-addressed key. A small
  per-workspace manifest points at the latest bundle and is only ever advanced with a
  conditional write, so an older snapshot can never replace a newer one.
- **SeaweedFS** is the local S3 store (MinIO's community edition is archived). The code only
  uses the plain S3 API, so real S3 is a configuration change.
- **Push before delete.** The pod and disk are deleted only after the snapshot is verified
  in S3. If the push fails, the sandbox keeps running and retries.
- **Local cluster shaped like production.** kind runs a tainted control-plane plus two
  workers, so a workspace can wake on a different node than it slept on. Measured on an
  M4 with Docker at 10 GB: the upstream controller, SeaweedFS, kube-prometheus-stack and
  three code-server pods use about 4 GiB; an extra idle worker costs about 120 MiB.
