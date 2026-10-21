// The live list of workspaces, kept from the gateway's event stream. Each
// connection sends every workspace, then "ready", then changes; a reconnect
// starts over, and whatever wasn't resent was deleted meanwhile.

import { key, type Workspace } from "./api.ts";

export interface Fleet {
  items: Record<string, Workspace>;
  live: boolean; // connected and past the first full list
  seen: Record<string, true> | null; // keys resent since the connection opened, until "ready"
  error?: string;
}

export type FleetAction =
  | { type: "connecting" }
  | { type: "workspace"; workspace: Workspace }
  | { type: "deleted"; namespace: string; name: string }
  | { type: "ready" }
  | { type: "disconnected"; error?: string };

export const emptyFleet: Fleet = { items: {}, live: false, seen: null };

export function fleetReducer(s: Fleet, a: FleetAction): Fleet {
  switch (a.type) {
    case "connecting":
      return { ...s, live: false, seen: {} };
    case "workspace": {
      const k = key(a.workspace);
      return {
        ...s,
        items: { ...s.items, [k]: a.workspace },
        seen: s.seen ? { ...s.seen, [k]: true } : null,
      };
    }
    case "deleted": {
      const k = key(a);
      if (!(k in s.items)) return s;
      const items = { ...s.items };
      delete items[k];
      return { ...s, items };
    }
    case "ready": {
      const seen = s.seen ?? {};
      const items: Record<string, Workspace> = {};
      for (const [k, w] of Object.entries(s.items)) if (seen[k]) items[k] = w;
      return { items, live: true, seen: null };
    }
    case "disconnected":
      return { ...s, live: false, seen: null, error: a.error };
  }
}

// sorted lists workspaces newest first.
export function sorted(f: Fleet): Workspace[] {
  return Object.values(f.items).sort(
    (a, b) => b.createdAt.localeCompare(a.createdAt) || key(a).localeCompare(key(b)),
  );
}

// asleep reports whether a workspace has no pod: its state is in S3 only.
export function asleep(w: Workspace): boolean {
  return w.phase === "Suspended" || w.phase === "TearingDown";
}

// tone groups phases for coloring.
export function tone(phase: string): "ok" | "busy" | "sleep" | "bad" | "idle" {
  switch (phase) {
    case "Running":
      return "ok";
    case "Provisioning":
    case "Waking":
    case "Suspending":
    case "TearingDown":
    case "Deleting":
      return "busy";
    case "Suspended":
      return "sleep";
    case "Failed":
      return "bad";
    default:
      return "idle";
  }
}
