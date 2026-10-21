// The demo's agent harness, as the CLI demo (internal/demo) plays it: the
// user's message arrives, the harness sends a wake-hint at once (or not),
// the LLM thinks, then the agent runs its first command.

import { errorText, type Api, type PolicyRef, type Workspace } from "./api.ts";
import type { Reply } from "./timeline.ts";

export interface ReplyOptions {
  id: number;
  hinted: boolean;
  thinkingMs: number;
  command: string;
}

export type Harness = Pick<Api, "wakeHint" | "exec" | "get">;

const wait = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

// playReply plays one user message against ws, reporting progress through
// update: first the new reply, then changes to it.
export async function playReply(
  api: Harness,
  ws: Workspace,
  o: ReplyOptions,
  update: (r: Reply) => void,
  sleep: (ms: number) => Promise<void> = wait,
  clock: () => number = Date.now,
): Promise<Reply> {
  let r: Reply = { ...o, messageAt: clock(), epochBefore: ws.epoch };
  const set = (patch: Partial<Reply>) => {
    r = { ...r, ...patch };
    update(r);
  };
  update(r);
  try {
    if (o.hinted) await api.wakeHint(ws.namespace, ws.name);
    await sleep(o.thinkingMs);
    set({ commandAt: clock() });
    const result = await api.exec(ws.namespace, ws.name, { command: ["sh", "-c", o.command] });
    const doneAt = clock();
    const after = await api.get(ws.namespace, ws.name).catch(() => undefined);
    set({ doneAt, result, after });
  } catch (err) {
    set({ doneAt: clock(), error: errorText(err) });
  }
  return r;
}

// demoPolicy sleeps soon after the agent ends its turn, so the demo
// doesn't wait for the cluster's idle timeout.
export function demoPolicy(afterTurnSeconds: number): PolicyRef {
  return { name: "fixed", params: { idleTimeout: "10m", afterTurnEnded: `${afterTurnSeconds}s` } };
}
