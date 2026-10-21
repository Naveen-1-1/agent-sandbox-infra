import { describe, expect, it } from "vitest";

import { firstCommand, layout, secs, ticks, type Reply } from "../src/timeline.ts";
import { at, start, workspace } from "./fixtures.ts";

// A wake whose stages finish 1s, 4s, 6s and 9s after it was asked for, as
// the server saw it (its clock a little behind the browser's).
const woken = (requested: number, epoch = 2) =>
  workspace({
    epoch,
    timeline: {
      trigger: "Hint",
      requestedAt: at(requested),
      podScheduledAt: at(requested + 1000),
      filesReadyAt: at(requested + 4000),
      readyAt: at(requested + 6000),
      depsReadyAt: at(requested + 9000),
    },
  });

const reply = (over: Partial<Reply>): Reply => ({
  id: 1,
  hinted: true,
  command: "npm ls --depth=0",
  thinkingMs: 10_000,
  messageAt: start,
  epochBefore: 1,
  ...over,
});

const spans = (l: ReturnType<typeof layout>) => l.spans.map((s) => [s.kind, s.start, s.end]);

describe("layout", () => {
  it("puts a hinted wake under the thinking", () => {
    const r = reply({
      commandAt: start + 10_000,
      doneAt: start + 10_500,
      result: { exitCode: 0, stdout: "", stderr: "", waitedMillis: 0, depsWaitedMillis: 0 },
    });
    const l = layout(r, woken(-300), start + 20_000); // server clock 300ms behind
    expect(spans(l)).toEqual([
      ["thinking", 0, 10_000],
      ["pod", 0, 1000],
      ["files", 1000, 4000],
      ["serving", 4000, 6000],
      ["deps", 6000, 9000],
      ["run", 10_000, 10_500],
    ]);
    expect(l).toMatchObject({ woke: true, wakeMs: 6000, waitedMs: 0, hiddenMs: 6000, end: 10_500 });
  });

  it("starts an unhinted wake at the command, which waits for all of it", () => {
    const r = reply({
      hinted: false,
      commandAt: start + 10_000,
      doneAt: start + 19_400,
      result: { exitCode: 0, stdout: "", stderr: "", waitedMillis: 6000, depsWaitedMillis: 3000 },
    });
    const l = layout(r, woken(10_020), start + 20_000);
    expect(spans(l)).toEqual([
      ["thinking", 0, 10_000],
      ["pod", 10_000, 11_000],
      ["files", 11_000, 14_000],
      ["serving", 14_000, 16_000],
      ["deps", 16_000, 19_000],
      ["held", 10_000, 16_000],
      ["depsHeld", 16_000, 19_000],
      ["run", 19_000, 19_400],
    ]);
    // The command needed dependencies, so the wake ran until they were in.
    expect(l).toMatchObject({ woke: true, wakeMs: 9000, waitedMs: 9000, hiddenMs: 0 });
  });

  it("shows no wake when the sandbox was awake", () => {
    const r = reply({
      epochBefore: 2,
      commandAt: start + 10_000,
      doneAt: start + 10_200,
      result: { exitCode: 0, stdout: "", stderr: "", waitedMillis: 0, depsWaitedMillis: 0 },
    });
    const l = layout(r, woken(-5000), start + 20_000);
    expect(l.woke).toBe(false);
    expect(l.spans.map((s) => s.lane)).toEqual(["thinking", "command"]);
    expect(l.wakeMs).toBeUndefined();
    expect(l.waitedMs).toBe(0);
  });

  it("keeps its own wake once the sandbox has woken again", () => {
    const r = reply({
      commandAt: start + 10_000,
      doneAt: start + 10_500,
      result: { exitCode: 0, stdout: "", stderr: "", waitedMillis: 0, depsWaitedMillis: 0 },
      after: woken(0, 2),
    });
    const later = woken(60_000, 3);
    expect(layout(r, later, start + 90_000).wakeMs).toBe(6000);
    // While still on its epoch, the live state counts (dependencies may land later).
    expect(layout(r, woken(0, 2), start + 20_000).spans.some((s) => s.kind === "deps")).toBe(true);
  });

  it("runs the bars to now while the agent waits", () => {
    const r = reply({ commandAt: start + 10_000 });
    const l = layout(r, workspace({ epoch: 1 }), start + 12_000);
    expect(spans(l)).toEqual([
      ["thinking", 0, 10_000],
      ["run", 10_000, 12_000],
    ]);
  });
});

describe("ticks", () => {
  it("covers the span in round steps", () => {
    expect(ticks(10_500)).toEqual([0, 2000, 4000, 6000, 8000, 10_000, 12_000]);
    expect(ticks(900)).toEqual([0, 250, 500, 750, 1000]);
    expect(ticks(0)).toEqual([0]);
    expect(ticks(30 * 60_000).length).toBeLessThanOrEqual(8);
  });
});

describe("secs", () => {
  it("formats durations", () => {
    expect([350, 1234, 59_999, 61_000].map(secs)).toEqual(["350ms", "1.2s", "1m0s", "1m1s"]);
  });
});

describe("firstCommand", () => {
  it("needs the dependencies of the profile", () => {
    expect(firstCommand("npm")).toBe("npm ls --depth=0");
    expect(firstCommand(undefined)).toBe("git status --short --branch");
  });
});
