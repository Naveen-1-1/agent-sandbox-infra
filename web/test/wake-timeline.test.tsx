import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { WakeTimeline } from "../src/components/WakeTimeline.tsx";
import type { Reply } from "../src/timeline.ts";
import { at, start, workspace } from "./fixtures.ts";

afterEach(cleanup);

const live = workspace({
  epoch: 2,
  timeline: { requestedAt: at(0), podScheduledAt: at(1000), filesReadyAt: at(4000), readyAt: at(6000) },
});

describe("WakeTimeline", () => {
  it("says how much of the wake the thinking hid", () => {
    const r: Reply = {
      id: 1,
      hinted: true,
      command: "ls",
      thinkingMs: 10_000,
      messageAt: start,
      epochBefore: 1,
      commandAt: start + 10_000,
      doneAt: start + 10_200,
      result: { exitCode: 0, stdout: "", stderr: "", waitedMillis: 0, depsWaitedMillis: 0 },
    };
    render(<WakeTimeline reply={r} live={live} now={start + 11_000} />);
    expect(screen.getByRole("img").getAttribute("aria-label")).toBe(
      "The wake took 6.0s and the agent waited 0ms: 6.0s of it was hidden behind the thinking.",
    );
    expect(screen.getByTitle("files restored: 1.0s to 4.0s")).toBeTruthy();
  });

  it("counts the thinking down", () => {
    const r: Reply = { id: 1, hinted: true, command: "ls", thinkingMs: 10_000, messageAt: start, epochBefore: 1 };
    render(<WakeTimeline reply={r} live={live} now={start + 3000} />);
    expect(screen.getByText(/the LLM is thinking \(3\.0s of 10\.0s\)/)).toBeTruthy();
  });
});
