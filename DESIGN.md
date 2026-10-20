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

### Object store (`internal/objstore`)

The small slice of S3 the platform uses. It covers immutable blobs written create-only
(`If-None-Match: *`), a manifest replaced only by compare-and-swap (`If-Match: <etag>`),
SHA-256 checksums the server verifies, and presigned URLs, so pods move bundles without
ever holding S3 credentials. An in-memory fake has the same semantics for unit tests. One
contract test suite runs against both the fake and the real endpoint, which is how we know
the fake is honest. Locally the endpoint is SeaweedFS (`charts/s3-local`); with 32
concurrent writers, exactly one create-only write and one compare-and-swap win.

## Key decisions

- **No git server.** A snapshot is a git bundle containing only the agent's changes
  relative to the upstream base commit, stored in S3 under a content-addressed key. A small
  per-workspace manifest points at the latest bundle and is only ever advanced with a
  conditional write, so an older snapshot can never replace a newer one.
- **SeaweedFS** is the local S3 store (MinIO's community edition is archived). The code only
  uses the plain S3 API, so real S3 is a configuration change.
- **Push before delete.** The pod and disk are deleted only after the snapshot is verified
  in S3. If the push fails, the sandbox keeps running and retries.
