// Timeline math for the hidden-wake demo: where the LLM's thinking, the
// wake's stages and the agent's command fall, in milliseconds from the
// user's message.

import type { ExecResponse, Timeline, Workspace } from "./api.ts";

// Reply is one user message in the demo: the harness may send a wake-hint
// at once, the LLM thinks, then the agent runs its first command.
export interface Reply {
  id: number;
  hinted: boolean;
  command: string;
  thinkingMs: number;
  messageAt: number; // browser clock
  epochBefore: number; // the workspace's epoch when the message came
  commandAt?: number;
  doneAt?: number;
  result?: ExecResponse;
  error?: string;
  after?: Workspace; // the workspace once the command finished
}

export type SpanKind = "thinking" | "pod" | "files" | "serving" | "deps" | "held" | "depsHeld" | "run";

export interface Span {
  lane: "thinking" | "wake" | "command";
  kind: SpanKind;
  start: number;
  end: number;
  label: string;
}

export interface Layout {
  spans: Span[];
  end: number; // the latest end, for the axis
  woke: boolean; // this reply woke the workspace (and the stages are its)
  wakeMs?: number; // from the wake request until the command could run
  waitedMs?: number; // what the agent waited: for the wake, then dependencies
  hiddenMs?: number; // the rest of the wake, hidden behind the thinking
}

const ms = (t?: string) => (t ? Date.parse(t) : undefined);

// stageOffsets are the wake's stage times from its request, in order.
export function stageOffsets(tl: Timeline): { pod?: number; files?: number; serving?: number; deps?: number } {
  const req = ms(tl.requestedAt);
  if (req === undefined) return {};
  const at = (t?: string) => {
    const v = ms(t);
    return v === undefined ? undefined : Math.max(0, v - req);
  };
  return { pod: at(tl.podScheduledAt), files: at(tl.filesReadyAt), serving: at(tl.readyAt), deps: at(tl.depsReadyAt) };
}

// layout places a reply's spans. The wake's stages come from the server's
// clock; they are anchored at the moment the browser asked for the wake
// (the hint, or else the command), so clock skew doesn't matter. live is
// the workspace now: its stages are used while it's still on the epoch the
// reply woke (dependencies may finish later), else those saved with it.
export function layout(r: Reply, live: Workspace | undefined, now: number): Layout {
  const ws = r.after && live?.epoch !== r.after.epoch ? r.after : live;
  const spans: Span[] = [];
  if (r.thinkingMs > 0) {
    spans.push({ lane: "thinking", kind: "thinking", start: 0, end: r.thinkingMs, label: "LLM thinking" });
  }

  const tl = ws?.timeline;
  const woke = !!ws && ws.epoch > r.epochBefore && !!tl?.requestedAt;
  const sent = r.commandAt === undefined ? undefined : r.commandAt - r.messageAt;
  const anchor = r.hinted ? 0 : sent;
  let wakeEnd: number | undefined;
  if (woke && tl && anchor !== undefined) {
    const st = stageOffsets(tl);
    let from = anchor;
    const stage = (kind: SpanKind, at: number | undefined, label: string) => {
      if (at === undefined) return;
      const end = anchor + at;
      spans.push({ lane: "wake", kind, start: from, end: Math.max(from, end), label });
      from = Math.max(from, end);
    };
    stage("pod", st.pod, "pod scheduled");
    stage("files", st.files, "files restored");
    stage("serving", st.serving, "serving");
    const servingAt = from;
    stage("deps", st.deps, "dependencies");
    const neededDeps = (r.result?.depsWaitedMillis ?? 0) > 0;
    wakeEnd = neededDeps && st.deps !== undefined ? anchor + st.deps : servingAt;
  }

  let waitedMs: number | undefined;
  if (sent !== undefined) {
    let at = sent;
    const res = r.result;
    if (res) {
      waitedMs = res.waitedMillis + res.depsWaitedMillis;
      if (res.waitedMillis > 0) {
        spans.push({ lane: "command", kind: "held", start: at, end: at + res.waitedMillis, label: "held for the wake" });
        at += res.waitedMillis;
      }
      if (res.depsWaitedMillis > 0) {
        spans.push({ lane: "command", kind: "depsHeld", start: at, end: at + res.depsWaitedMillis, label: "held for dependencies" });
        at += res.depsWaitedMillis;
      }
    }
    const done = (r.doneAt ?? now) - r.messageAt;
    spans.push({ lane: "command", kind: "run", start: Math.min(at, done), end: done, label: r.command });
  }

  const end = spans.reduce((m, s) => Math.max(m, s.end), 0);
  const out: Layout = { spans, end, woke };
  if (wakeEnd !== undefined && anchor !== undefined) {
    out.wakeMs = wakeEnd - anchor;
    if (waitedMs !== undefined) {
      out.waitedMs = waitedMs;
      out.hiddenMs = Math.max(0, out.wakeMs - waitedMs);
    }
  } else if (waitedMs !== undefined) {
    out.waitedMs = waitedMs;
  }
  return out;
}

// ticks are round times for an axis from 0 to the first tick at or past
// end, about max steps apart at most.
export function ticks(end: number, max = 6): number[] {
  if (end <= 0) return [0];
  const steps = [100, 250, 500, 1000, 2000, 2500, 5000, 10_000, 15_000, 30_000, 60_000, 120_000, 300_000];
  const step = steps.find((s) => end / s <= max) ?? Math.ceil(end / max / 60_000) * 60_000;
  const out = [0];
  while (out[out.length - 1] < end) out.push(out[out.length - 1] + step);
  return out;
}

// secs formats a duration the way the CLI demo does.
export function secs(msv: number): string {
  if (msv < 1000) return `${Math.round(msv)}ms`;
  if (msv < 59_950) return `${(msv / 1000).toFixed(1)}s`;
  const s = Math.round(msv / 1000);
  return `${Math.floor(s / 60)}m${s % 60}s`;
}

// firstCommand guesses the agent's first tool call for a dependency
// profile: one that needs the dependencies, so the deps gate shows too.
export function firstCommand(profile: string | undefined): string {
  switch (profile) {
    case "npm":
      return "npm ls --depth=0";
    case "pip":
      return "python -m pip list";
    case "uv":
      return "uv pip list";
    default:
      return "git status --short --branch";
  }
}
