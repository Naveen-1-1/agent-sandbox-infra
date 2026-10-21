import { describe, expect, it } from "vitest";

import { emptyFleet, fleetReducer, sorted, tone, type Fleet, type FleetAction } from "../src/fleet.ts";
import { workspace } from "./fixtures.ts";

const run = (s: Fleet, ...actions: FleetAction[]) => actions.reduce(fleetReducer, s);

describe("fleetReducer", () => {
  it("goes live once the first full list is in", () => {
    const s = run(emptyFleet, { type: "connecting" }, { type: "workspace", workspace: workspace({ name: "a" }) });
    expect(s.live).toBe(false);
    const r = fleetReducer(s, { type: "ready" });
    expect(r.live).toBe(true);
    expect(Object.keys(r.items)).toEqual(["sbx-workspaces/a"]);
  });

  it("applies changes and deletions", () => {
    let s = run(
      emptyFleet,
      { type: "connecting" },
      { type: "workspace", workspace: workspace({ name: "a" }) },
      { type: "workspace", workspace: workspace({ name: "b" }) },
      { type: "ready" },
    );
    s = run(s, { type: "workspace", workspace: workspace({ name: "a", phase: "Suspended" }) });
    expect(s.items["sbx-workspaces/a"].phase).toBe("Suspended");
    s = run(s, { type: "deleted", namespace: "sbx-workspaces", name: "b" });
    expect(Object.keys(s.items)).toEqual(["sbx-workspaces/a"]);
    expect(fleetReducer(s, { type: "deleted", namespace: "x", name: "y" })).toBe(s);
  });

  it("drops what was deleted while disconnected, and keeps showing the rest meanwhile", () => {
    let s = run(
      emptyFleet,
      { type: "connecting" },
      { type: "workspace", workspace: workspace({ name: "a" }) },
      { type: "workspace", workspace: workspace({ name: "b" }) },
      { type: "ready" },
      { type: "disconnected", error: "network" },
    );
    expect(s.live).toBe(false);
    expect(s.error).toBe("network");
    s = run(s, { type: "connecting" }, { type: "workspace", workspace: workspace({ name: "b", epoch: 2 }) });
    expect(Object.keys(s.items).sort()).toEqual(["sbx-workspaces/a", "sbx-workspaces/b"]); // until "ready"
    s = fleetReducer(s, { type: "ready" });
    expect(Object.keys(s.items)).toEqual(["sbx-workspaces/b"]);
    expect(s.items["sbx-workspaces/b"].epoch).toBe(2);
    expect(s.error).toBeUndefined();
  });
});

describe("sorted", () => {
  it("lists newest first", () => {
    const s = run(
      emptyFleet,
      { type: "workspace", workspace: workspace({ name: "old", createdAt: "2026-10-01T00:00:00Z" }) },
      { type: "workspace", workspace: workspace({ name: "new", createdAt: "2026-10-20T00:00:00Z" }) },
    );
    expect(sorted(s).map((w) => w.name)).toEqual(["new", "old"]);
  });
});

describe("tone", () => {
  it("groups phases", () => {
    expect(["Running", "Waking", "Suspended", "Failed", "Pending"].map(tone)).toEqual(["ok", "busy", "sleep", "bad", "idle"]);
  });
});
