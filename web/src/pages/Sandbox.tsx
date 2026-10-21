import { useState, type ReactNode } from "react";

import { errorText, type Api, type Workspace } from "../api.ts";
import { Phase } from "../components/Phase.tsx";
import { TimelineLegend, WakeTimeline } from "../components/WakeTimeline.tsx";
import type { ReplyOptions } from "../demo.ts";
import { asleep } from "../fleet.ts";
import { useNow } from "../hooks.ts";
import { firstCommand, layout, secs, type Reply } from "../timeline.ts";

// Sandbox is one sandbox: its live state, its controls and the demo.
export function Sandbox({
  api,
  ws,
  profile,
  replies,
  onReply,
  onDeleted,
}: {
  api: Api;
  ws: Workspace;
  profile?: string;
  replies: Reply[];
  onReply: (o: Omit<ReplyOptions, "id">) => void;
  onDeleted: () => void;
}) {
  const [busy, setBusy] = useState("");
  const [error, setError] = useState<string>();
  const pending = replies.some((r) => !r.doneAt);
  const now = useNow(pending || !!ws.sleep?.suspendAt);

  const act = async (label: string, f: () => Promise<unknown>) => {
    setBusy(label);
    setError(undefined);
    try {
      await f();
    } catch (err) {
      setError(`${label}: ${errorText(err)}`);
    } finally {
      setBusy("");
    }
  };
  const openEditor = () => {
    // Open the tab now, while this is still a click, so it isn't blocked.
    const tab = window.open("about:blank", "_blank");
    void act("Open editor", async () => {
      try {
        const { url } = await api.editor(ws.namespace, ws.name);
        if (tab) {
          tab.opener = null;
          tab.location.href = url;
        } else {
          location.assign(url);
        }
      } catch (err) {
        tab?.close();
        throw err;
      }
    });
  };
  const remove = () => {
    if (!confirm(`Delete ${ws.namespace}/${ws.name} and its snapshots?`)) return;
    void act("Delete", async () => {
      await api.remove(ws.namespace, ws.name);
      onDeleted();
    });
  };

  return (
    <>
      <section className="panel">
        <header className="panel-head row">
          <h2>{ws.name}</h2>
          <Phase phase={ws.phase} />
          <span className="spacer" />
          <div className="row actions">
            <button type="button" onClick={openEditor} disabled={!!busy || ws.phase === "Deleting"}>
              Open editor
            </button>
            <button type="button" onClick={() => void act("Wake", () => api.wake(ws.namespace, ws.name))} disabled={!!busy || !asleep(ws)}>
              Wake
            </button>
            <button
              type="button"
              onClick={() => void act("Suspend", () => api.suspend(ws.namespace, ws.name))}
              disabled={!!busy || ws.phase !== "Running"}
            >
              Suspend
            </button>
            <button type="button" className="danger" onClick={remove} disabled={!!busy}>
              Delete
            </button>
          </div>
        </header>
        {ws.message && <p className="muted">{ws.message}</p>}
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
        <Facts ws={ws} now={now} />
      </section>
      <Demo ws={ws} profile={profile} replies={replies} onReply={onReply} act={act} busy={busy} api={api} now={now} />
    </>
  );
}

function Facts({ ws, now }: { ws: Workspace; now: number }) {
  const sleep = ws.sleep;
  const until = sleep?.suspendAt ? Date.parse(sleep.suspendAt) - now : undefined;
  const rows: [string, ReactNode][] = [
    ["Repository", `${ws.repoURL}${ws.ref ? ` @ ${ws.ref}` : ""}`],
    ["Image", ws.image ? `${ws.image}${ws.imageSource ? ` (${ws.imageSource})` : ""}` : "choosing…"],
    ["Epoch", `${ws.epoch}${ws.node ? ` on ${ws.node}` : ""}`],
    ["Sleep policy", ws.policy ? policyText(ws.policy) : "the cluster's"],
    [
      "Verdict",
      sleep
        ? `${sleep.reason}${sleep.message ? `: ${sleep.message}` : ""}${until !== undefined && until > 0 ? ` (sleeps in ${secs(until)})` : ""}`
        : "none yet",
    ],
    [
      "Dependencies",
      ws.deps?.profile
        ? `${ws.deps.profile}, ${ws.deps.pending ? "restoring" : ws.deps.ready ? "ready" : "failed"}${ws.deps.source ? ` (${ws.deps.source})` : ""}${ws.deps.message ? `: ${ws.deps.message}` : ""}`
        : asleep(ws)
          ? "no disk while asleep"
          : "none detected",
    ],
    ["Last snapshot", ws.lastSnapshot ? `#${ws.lastSnapshot.seq}, ${bytes(ws.lastSnapshot.bytes)}` : "none yet"],
  ];
  return (
    <dl className="facts">
      {rows.map(([k, v]) => (
        <div key={k}>
          <dt>{k}</dt>
          <dd>{v}</dd>
        </div>
      ))}
    </dl>
  );
}

function Demo({
  api,
  ws,
  profile,
  replies,
  onReply,
  act,
  busy,
  now,
}: {
  api: Api;
  ws: Workspace;
  profile?: string;
  replies: Reply[];
  onReply: (o: Omit<ReplyOptions, "id">) => void;
  act: (label: string, f: () => Promise<unknown>) => Promise<void>;
  busy: string;
  now: number;
}) {
  const [thinking, setThinking] = useState(10);
  const [command, setCommand] = useState(""); // empty: firstCommand, once the profile is known
  const pending = replies.some((r) => !r.doneAt);
  const shared = Math.max(0, ...replies.map((r) => Math.max(layout(r, ws, now).end, r.thinkingMs)));
  const send = (hinted: boolean) => onReply({ hinted, thinkingMs: thinking * 1000, command: command.trim() || firstCommand(profile) });
  return (
    <section className="panel demo">
      <header className="panel-head">
        <h2>Hidden wake</h2>
        <p className="muted">
          End the agent's turn and let the sandbox fall asleep. Then send a user message: the harness sends a
          wake-hint at once, the LLM thinks, and the agent runs its first command. Without a hint, the wake starts
          only when that command arrives, and the agent waits for all of it.
        </p>
      </header>
      <div className="row controls">
        <button type="button" onClick={() => void act("End turn", () => api.turnEnded(ws.namespace, ws.name))} disabled={!!busy || ws.phase !== "Running"}>
          End the agent's turn
        </button>
        <label>
          LLM thinks for
          <input
            type="number"
            min={0}
            max={120}
            className="tiny"
            aria-label="seconds the LLM thinks"
            value={thinking}
            onChange={(e) => setThinking(Math.max(0, Number(e.target.value) || 0))}
          />
          s, then runs
        </label>
        <input
          className="cmd"
          aria-label="the agent's first command"
          placeholder={firstCommand(profile)}
          value={command}
          onChange={(e) => setCommand(e.target.value)}
        />
      </div>
      <div className="row controls">
        <button type="button" className="primary" onClick={() => send(true)} disabled={pending}>
          User message, with a wake-hint
        </button>
        <button type="button" onClick={() => send(false)} disabled={pending}>
          User message, no hint
        </button>
        {!asleep(ws) && !pending && <span className="muted">The sandbox is {ws.phase.toLowerCase()}: there's nothing to wake.</span>}
      </div>
      {replies.map((r) => (
        <div key={r.id} className="reply">
          <WakeTimeline reply={r} live={ws} now={now} minEnd={shared} />
          {r.result && (r.result.stdout || r.result.stderr) && (
            <details>
              <summary>
                Output of <code>{r.command}</code> (exit {r.result.exitCode})
              </summary>
              <pre>{(r.result.stdout + r.result.stderr).slice(-4000)}</pre>
            </details>
          )}
        </div>
      ))}
      {replies.length > 0 && <TimelineLegend />}
    </section>
  );
}

function policyText(p: { name: string; params?: Record<string, string> }): string {
  const params = Object.entries(p.params ?? {}).map(([k, v]) => `${k}=${v}`);
  return params.length ? `${p.name} (${params.join(", ")})` : p.name;
}

function bytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / 1024 / 1024).toFixed(1)} MiB`;
}
