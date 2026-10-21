import { describe, expect, it } from "vitest";

import { ApiError, type ExecRequest } from "../src/api.ts";
import { demoPolicy, playReply, type Harness } from "../src/demo.ts";
import type { Reply } from "../src/timeline.ts";
import { workspace } from "./fixtures.ts";

function harness(fail?: Error) {
  const calls: string[] = [];
  const api: Harness = {
    wakeHint: async () => {
      calls.push("hint");
    },
    exec: async (_ns: string, _name: string, r: ExecRequest) => {
      calls.push(`exec ${r.command.join(" ")}`);
      if (fail) throw fail;
      return { exitCode: 0, stdout: "ok\n", stderr: "", waitedMillis: 0, depsWaitedMillis: 0 };
    },
    get: async () => {
      calls.push("get");
      return workspace({ epoch: 2 });
    },
  };
  return { api, calls };
}

// A clock that moves only when the harness "thinks".
function clock() {
  let now = 1000;
  return {
    now: () => now,
    sleep: async (ms: number) => {
      now += ms;
    },
  };
}

describe("playReply", () => {
  it("hints, thinks, then runs the command", async () => {
    const { api, calls } = harness();
    const c = clock();
    const seen: Reply[] = [];
    const r = await playReply(api, workspace(), { id: 1, hinted: true, thinkingMs: 5000, command: "npm ls" }, (x) => seen.push(x), c.sleep, c.now);
    expect(calls).toEqual(["hint", "exec sh -c npm ls", "get"]);
    expect(r).toMatchObject({ messageAt: 1000, commandAt: 6000, doneAt: 6000, epochBefore: 1, after: { epoch: 2 } });
    expect(r.result?.stdout).toBe("ok\n");
    expect(seen.map((x) => [x.commandAt, x.doneAt])).toEqual([
      [undefined, undefined],
      [6000, undefined],
      [6000, 6000],
    ]);
  });

  it("sends no hint without one", async () => {
    const { api, calls } = harness();
    const c = clock();
    await playReply(api, workspace(), { id: 1, hinted: false, thinkingMs: 0, command: "ls" }, () => {}, c.sleep, c.now);
    expect(calls).toEqual(["exec sh -c ls", "get"]);
  });

  it("reports a failed command", async () => {
    const { api } = harness(new ApiError(504, "wake_timeout", "the workspace did not wake in time"));
    const c = clock();
    const r = await playReply(api, workspace(), { id: 1, hinted: true, thinkingMs: 0, command: "ls" }, () => {}, c.sleep, c.now);
    expect(r.error).toBe("the workspace did not wake in time (wake_timeout)");
    expect(r.doneAt).toBe(1000);
  });
});

describe("demoPolicy", () => {
  it("sleeps soon after the turn ends", () => {
    expect(demoPolicy(5)).toEqual({ name: "fixed", params: { idleTimeout: "10m", afterTurnEnded: "5s" } });
  });
});
