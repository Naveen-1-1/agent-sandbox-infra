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

*(filled in as each component lands)*

## Key decisions

- **No git server.** A snapshot is a git bundle containing only the agent's changes
  relative to the upstream base commit, stored in S3 under a content-addressed key. A small
  per-workspace manifest points at the latest bundle and is only ever advanced with a
  conditional write, so an older snapshot can never replace a newer one.
- **SeaweedFS** is the local S3 store (MinIO's community edition is archived). The code only
  uses the plain S3 API, so real S3 is a configuration change.
- **Push before delete.** The pod and disk are deleted only after the snapshot is verified
  in S3. If the push fails, the sandbox keeps running and retries.
