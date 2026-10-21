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
upstream base commit already has, so it contains only the agent's changes. Git leaves out
of the bundle any ref that points into the base's history, such as `main` itself or a branch
made without new commits. So the working-tree commit also records every ref by name and
object, and restore recreates them all. Commits use a
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

- **Running** asks the sleep policy (below) when to suspend.
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
phase, time in each phase, wake latency to files-ready and serving, restore bytes, wake
and suspend counts, policy verdicts, and wakes and suspends running or waiting for a slot.

### Sleep policy (`internal/policy`, `internal/controller/sleep`)

A policy is a pure function: given what is known about a workspace and the time, it says
"suspend now", or "keep it up, ask again at T, and here's why". Policies register by name
and take string parameters, so choosing one is configuration. Two ship:

- **`fixed`** suspends after `idleTimeout` (10m) with no activity, or `afterTurnEnded` (2m)
  after the agent ends its turn if nothing happens since. A running exec or terminal
  command, an open exec or app connection, or a request held at the gateway keeps the
  workspace up. An open editor tab does not.
- **`never`** suspends only when asked.

The controller merges the signals before each decision:

- the agent's activity, running commands and open streams (`GetSignals`);
- every gateway replica's turns, held requests and streams;
- the last wake or aborted suspend from status.

A collector polls all gateway replicas every 5s, finding them through the EndpointSlices of
their headless Service. A replica that doesn't answer makes the signals *unknown*, and a
policy never suspends on unknown signals. A turn ending re-decides that workspace at once.
The verdict is written to `status.sleep` (reason, message, `suspendAt`) only when it
changes, and stays there after a suspend to say why.

The cluster policy lives in the `sbx-policy` ConfigMap, written at runtime (the settings
page). It falls back to the chart's `sbx-policy-defaults`, then to `never`. A change
re-decides every running workspace straight away. A config that fails to parse is reported
as a Warning Event on the ConfigMap, and the last good policy stays in force. A workspace can
override the cluster with `spec.policy`. Mode `AlwaysOn` never suspends, and mode `Off` or
an explicit suspend request always does.

### Operation limiter (`internal/controller/ops`)

A storm of wakes or suspends must not overwhelm the nodes or S3. At most `maxConcurrent`
wakes and suspends (8) run at once. The reconciler is level-triggered, so it never blocks:
a workspace asks for a slot, and if none is free it is queued and asks again shortly.
Free slots go to the most urgent waiter: a wake a request is held for, then a wake a hint
predicted, then a suspend. A hinted wake moves up as soon as a gateway holds a request for
it. `minSuspend` slots (2) stay open for suspends even while wakes wait, because suspends
free capacity. Work in progress re-registers itself on every reconcile, so a new leader
rebuilds the counts, and a slot nobody reports for a while is reclaimed.

### Workspace pod (`internal/controller/podspec`)

One epoch of a workspace is one upstream `Sandbox` pod, built by a pure function and
pinned by a golden file. Its containers start in order:

1. `sbx-restore` puts the files on this epoch's disk.
2. `sbx-sidecar` runs as a native sidecar.
3. The user's image runs with `sbx-agent` as PID 1, mounting the disk at `/workspace`.

The user's image runs our agent and editor from `/sbx`. That is mounted read-only from the
sbx-runtime image as an *image volume*, so nothing is copied and the editor's few hundred
MB cost a wake nothing. Clusters without image volumes (before Kubernetes 1.35) can set
`workspace.tools: copy` instead, which copies `/sbx` in a first init container. The image
is chosen in this order: `spec.image`; the repository's `devcontainer.json` (`image` only,
since building images isn't supported); then the platform default. `status.imageSource`
says which, and why a `devcontainer.json` was ignored.

Kubernetes guarantees the workload never starts before its files exist. The disk's mount
root is owned by root on every storage driver, so the workload's git is told through its
environment that `/workspace` is a safe directory. The per-epoch
token Secret is mounted only into our containers. The workload gets just the hash of its
agent token, has no service-account token, and our containers drop all capabilities.

### Dependencies (`internal/depcache`, restored by the sidecar)

Dependency directories (`node_modules`, `.venv`) are never in a snapshot. They come back
from a shared cache instead:

- **Profiles.** npm, pip or uv, detected from the lockfile in the repository. Each
  profile names its directory, its install command (`npm ci`, `uv sync --frozen`, a venv
  plus `pip install -r`) and the programs that need it (`npm`, `node`, `python`, `pytest`, ...).
- **Key.** An archive is keyed by everything that determines the directory: profile,
  lockfile hash, the workload image's digest (which pins the runtime and package manager)
  and CPU arch. The key is shared by every workspace, so a new workspace from the same repo
  starts with its dependencies.
- **Restore.** Once files are restored and the controller knows the image digest, the
  sidecar looks the key up through the broker. On a hit it downloads and extracts the
  archive next to the target, then renames it into place. On a miss it asks the agent to
  run the install in the workload container. The workspace serves meanwhile, and only
  commands that need the dependencies wait (see the gateway). `DepsReady` and
  `status.deps` say where the directory came from.
- **Upload.** During the next suspend, while the workload is frozen, the sidecar uploads
  an install it ran itself. It skips the upload if the lockfile has changed since, because
  then the directory no longer matches either key. Uploads are write-once and checksummed.
- **Extraction is hardened.** Every write goes through an `os.Root`. Archives are refused
  if they contain absolute or `..` names, relative links that leave the directory, hard
  links, devices or duplicate entries. Setuid bits are dropped and total size is bounded.

Known limits:

- The cache is shared across all workspaces, so it fits a single tenant; per-tenant scoping
  comes after the MVP.
- npm workspaces symlink packages out of `node_modules`, so they always install instead of
  using the cache.
- The restore starts at about the time the pod turns ready, because that is when kubelet
  reports the image digest.

### Services (agent, controller)

`spec.services` declares long-running processes, such as a dev server. The agent runs each
one in its own process group, restarts it with backoff when it exits, and keeps the tail
of its output. Its process tree stops with SIGTERM, then SIGKILL. The controller starts
the declared services once per epoch. Those that wait for dependencies (the default) start
when the restore settles, so after every wake the dev server comes back in the new pod.
Running services don't keep a workspace awake under the `fixed` policy.

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

### Editor (code-server in the agent, `extensions/vscode-activity`)

Each workspace has a VS Code editor in the browser: code-server, started by the agent the
first time someone opens it, listening only on loopback. The agent proxies it at
`/sbx/editor/`, and any app port at `/sbx/port/<port>/`, behind the agent token. The
gateway then serves them on hosts of their own (see the gateway).

Only real input counts as activity. A small extension reports edits (at most one every
five seconds), saves, and terminal commands starting and ending to a loopback-only agent
endpoint. A terminal command keeps the workspace up the way an exec does, and is dropped
if the extension stops reminding the agent it is running. An editor tab that is open but
idle reports nothing, and its WebSocket doesn't keep the workspace awake.

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
exec, files, services, wake, wake-hint, turn-ended and suspend. Each call needs either the admin token
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
- A command that needs the dependency directory waits for `DepsReady`. Those are commands
  that run the profile's programs, or that ask with `waitFor: ["deps"]`. `ls`, `cat` and
  `git` run as soon as the pod serves. A failed restore releases the waiting commands, so
  they run and report the error.

**Workspace hosts.** Browsers reach a workspace's editor at `<name>.<namespace>.<domain>`,
and an app port at `<port>.<name>.<namespace>.<domain>` (`*.sbx.localhost` on kind, which
browsers resolve to loopback). Each host gets its own cookies and origin:

- **Sign-in.** A sign-in link from the API (`sbxctl editor`) carries a five-minute ticket.
  The gateway swaps it for an HttpOnly cookie bound to that host that expires after 12 hours.
- **Bearer tokens.** Scripts can send the workspace token instead of a cookie.
- **Origin checks.** Cookie-borne WebSocket upgrades and writes must come from the host's
  own pages.
- **Wake rule.** Only a person opening the page (a top-level navigation) wakes a sleeping
  workspace. Reconnects from a tab left open, polls, assets and iframes get a 503, so a
  forgotten tab never keeps a workspace awake or wakes it.

Status reads, `/healthz`, `/readyz` and `/metrics` never wake anything; the health and
metrics endpoints are on a separate admin port. Each replica also keeps the traffic signals
the sleep policy reads: last activity, turn started and ended, open streams and held
requests. It serves them over gRPC to the controller.

**Settings and events.** Two admin endpoints serve the web UI:

- `/api/v1/policy` reads, validates and writes the `sbx-policy` ConfigMap that the
  controller already watches, so a change reaches running workspaces at once. A policy is
  built before it is saved, so a bad one is a 400. A concurrent edit is a 409. The gateway
  may touch only the two policy ConfigMaps in its own namespace, plus create, which RBAC
  can't limit by name.
- `/api/v1/events` streams server-sent events: every workspace, then `ready`, then each
  change. Each stream keeps the set of workspaces that changed, not a queue of events, so a
  slow browser gets the latest state of each, never a backlog. Streams are bounded per
  replica and pinged every 15 seconds.

### Tokens (`internal/token`)

Every per-epoch token is `HMAC(master key, purpose | workspace UID | epoch)`. The purposes
are sidecar, agent and broker. The controller and gateway hold the master key and recompute
tokens on demand, so nothing is stored. A pod receives only its own epoch's tokens, so a
compromised workspace can't mint tokens for another workspace or another epoch. The
gateway token, which clients use, is derived with epoch 0 so that it survives wakes. It
is never given to the pod.

### Snapshot store (`internal/snapstore`)

Owns the S3 layout. `ws/<uid>/b/<sha256>.bundle` holds bundles, written once and content
addressed. `bases/<url-hash>/<sha>.bundle` holds cached upstream bases, and
`deps/<profile>/<key>.tar.zst` holds shared dependency archives. Each workspace has
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
- optionally, SeaweedFS and a test-only git fixtures server;
- optionally, kube-prometheus-stack, trimmed to Prometheus, Grafana and the operator.

The token master key is generated once and kept across upgrades. Images are built from
binaries cross-compiled on the host and tagged with a content hash, so an upgrade only
rolls what changed. There are four images:

- `sbx-runtime` (sidecar, agent, git, code-server and the activity extension) for
  workspace pods;
- `sbx-controller`;
- `sbx-gateway` (distroless);
- `sbx-fixtures` (git over plain HTTP) for tests and the demo.

### Observability (`internal/metrics`, `internal/cost`, `charts/sbx/dashboards`)

The controller and every gateway replica export `sbx_*` metrics. ServiceMonitors put them
in Prometheus, and three Grafana dashboards ship with the chart:

- **Fleet:** workspaces awake and asleep, time spent in each phase, wakes and suspends,
  sleep-policy verdicts, and operations queued for a slot.
- **Wake:** latency from the wake request to files restored, serving and dependencies
  ready, by trigger; how far ahead a hint started the wake; requests held and rejected;
  the dependency cache hit rate.
- **Storage and cost:** what object storage holds by kind (snapshots, bases, dependency
  archives), the sizes pushed on suspend and fetched on wake, and the cost.

Cost is computed at scrape time from a table of list prices in the chart values. An awake
workspace pays for its pod's CPU and memory requests and its disk; a sleeping one only for
its snapshot in object storage. `sbx_cost_saved_usd_per_hour` is what the sleeping
workspaces would cost awake, less what they cost asleep. The leader also lists object
storage now and then to size it. Every phase is exported, zeros included, so a quiet fleet
reads 0 rather than "no data". A unit test fails if a dashboard queries a metric nothing
exports, and an e2e test runs every dashboard query against the live Prometheus.

### Demo (`internal/demo`, `sbxctl demo`, `make demo`)

Plays an agent session through the gateway API alone, as an agent harness would, holding
only the workspace's own token:

1. It creates a workspace from the fixture repository.
2. The agent's turn writes a file, makes a branch, stages one change and leaves another
   unstaged.
3. The agent ends its turn, and the policy suspends the workspace.
4. When the user replies, the harness sends a wake-hint and the simulated LLM thinks.
   The first command then shows whether the wake was hidden.
5. The same happens again without a hint, for comparison.

Each wake prints a timeline from the request to pod scheduled, files restored, serving and
the first command. The demo checks that the git state after every sleep matches the state
before it. `make demo` pre-pulls the images onto every node that runs workspaces first (a
short-lived DaemonSet), so the wakes measure restore time, not image download. An e2e test
runs the same session.

### Benchmark (`internal/bench`, `cmd/sbx-bench`, `make bench`)

Measures the platform on real repositories pinned to commits (`test/bench/repos.json`): a
Python ML project with torch (pip), a TypeScript app (npm) and a small library (uv). For
each, one at a time:

1. **Cold start.** It wipes that repository's cached base bundle and dependency archives,
   creates a workspace and records the clone, the start and the dependency install.
2. **Cycle 0.** The first suspend uploads the dependency archive, and the first wake
   restores it. It is recorded apart, as no later cycle looks like it.
3. **Ten cycles.** Each edits a tracked file, suspends, and wakes with a command that needs
   the dependencies. Odd cycles send a wake-hint and then "think" for 10 seconds; even ones
   send the command straight away.

Each cycle records the suspend time, the wake's stages from its request (files restored,
serving, dependencies), how long the command was held, the bundle size, the S3 requests
(PUT and GET class), and the CPU time and peak memory of the sidecar and the agent.

- **Counters, not scrapes.** S3 requests come from counters the controller keeps for its own
  calls and for the presigned URLs it hands to pods. The bench reads them from every
  controller replica through the API server's pod proxy, so a cycle's count is exact.
- **Usage.** The sidecar's CPU and memory come from its own `/proc`, read with
  `kubectl exec`. The agent's come through the gateway: its supervisor is PID 1 and the
  agent is the supervisor's child.
- **One workspace at a time.** The bench refuses to run beside other workspaces, so nothing
  else shares the nodes and the running cost it reads is the bench workspace's own.

It writes `notes/bench-<env>-<date>.json`: every cycle, per-repository summaries (average and
nearest-rank p95), and the averages the cost report runs on.

### Web UI (`web/`, embedded in the gateway)

A React app that Vite builds into `internal/gateway/webui/dist`. The gateway embeds it and
serves it under `/ui/`, so the UI ships with the gateway and needs no server of its own.

- **Sign-in.** It calls the gateway API with the admin token. `sbxctl ui` prints a link with
  the token in the URL fragment, which browsers never send to a server or in a Referer. The
  page moves it to session storage and drops it from the address bar. It is a bearer token,
  not a cookie, so other sites can't send requests with it.
- **Live state.** The page reads the event stream with fetch, because EventSource can't
  send an Authorization header. It reconnects with backoff, and after a reconnect it drops
  the workspaces that weren't sent again.
- **Hidden-wake demo.** The page plays the CLI demo's harness: a wake-hint or not, the
  thinking, then the agent's first command. It draws the thinking, the wake's stages and
  the held command on one axis. The server's stage times are placed from the moment the
  browser asked for the wake, so clock skew between the browser and the cluster doesn't
  distort them.
- **Settings.** The page edits the cluster's sleep policy through `/api/v1/policy`, with each
  parameter's default and help taken from the policy registry.

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
