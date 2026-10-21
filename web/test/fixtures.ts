import type { Workspace } from "../src/api.ts";

export function workspace(over: Partial<Workspace> = {}): Workspace {
  return {
    namespace: "sbx-workspaces",
    name: "demo",
    uid: "u1",
    repoURL: "https://example.com/repo.git",
    mode: "Auto",
    phase: "Running",
    epoch: 1,
    ready: true,
    createdAt: "2026-10-21T10:00:00Z",
    ...over,
  };
}

// at is an ISO time ms after a fixed start.
export const start = Date.parse("2026-10-21T10:00:00Z");
export const at = (ms: number) => new Date(start + ms).toISOString();
