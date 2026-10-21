import type { Workspace } from "../api.ts";
import { layout, secs, ticks, type Layout, type Reply, type Span } from "../timeline.ts";

const lanes: { id: Span["lane"]; name: string }[] = [
  { id: "thinking", name: "LLM" },
  { id: "wake", name: "Sandbox" },
  { id: "command", name: "Agent" },
];

const legend: { kind: Span["kind"]; label: string }[] = [
  { kind: "thinking", label: "thinking" },
  { kind: "pod", label: "pod scheduled" },
  { kind: "files", label: "files restored" },
  { kind: "serving", label: "serving" },
  { kind: "deps", label: "dependencies" },
  { kind: "held", label: "held for the wake" },
  { kind: "depsHeld", label: "held for dependencies" },
  { kind: "run", label: "command" },
];

// WakeTimeline draws one reply: the LLM thinking, the sandbox waking and
// the agent's command, on one time axis from the user's message. Replies
// drawn together share minEnd, so their bars compare.
export function WakeTimeline({ reply, live, now, minEnd = 0 }: { reply: Reply; live?: Workspace; now: number; minEnd?: number }) {
  const l = layout(reply, live, now);
  const axis = ticks(Math.max(l.end, reply.thinkingMs, minEnd, 1000));
  const span = axis[axis.length - 1];
  const pct = (t: number) => `${(Math.min(t, span) / span) * 100}%`;
  const text = summary(reply, l, now);
  return (
    <figure className="tl">
      <figcaption>
        <strong>{reply.hinted ? "With a wake-hint" : "Without a hint"}</strong> {text}
      </figcaption>
      <div className="tl-chart" role="img" aria-label={text}>
        {lanes.map((lane) => (
          <div className="tl-lane" key={lane.id}>
            <div className="tl-name">{lane.name}</div>
            <div className="tl-track">
              {l.spans
                .filter((s) => s.lane === lane.id)
                .map((s, i) => (
                  <div
                    key={i}
                    className={`tl-span k-${s.kind}`}
                    style={{ left: pct(s.start), width: `max(3px, calc(${pct(s.end)} - ${pct(s.start)}))` }}
                    title={`${s.label}: ${secs(s.start)} to ${secs(s.end)}`}
                  >
                    <span>{s.label}</span>
                  </div>
                ))}
            </div>
          </div>
        ))}
        <div className="tl-lane tl-axis" aria-hidden="true">
          <div className="tl-name" />
          <div className="tl-track">
            {axis.map((t) => (
              <span key={t} style={{ left: pct(t) }}>
                {t === 0 ? "0s" : secs(t)}
              </span>
            ))}
          </div>
        </div>
      </div>
    </figure>
  );
}

export function TimelineLegend() {
  return (
    <ul className="tl-legend" aria-hidden="true">
      {legend.map((k) => (
        <li key={k.kind}>
          <i className={`k-${k.kind}`} />
          {k.label}
        </li>
      ))}
    </ul>
  );
}

function summary(r: Reply, l: Layout, now: number): string {
  if (r.error) return `failed: ${r.error}`;
  if (r.commandAt === undefined) return `the LLM is thinking (${secs(now - r.messageAt)} of ${secs(r.thinkingMs)})…`;
  if (!r.result) return `the agent is running ${r.command}…`;
  const exit = r.result.exitCode === 0 ? "" : ` It exited ${r.result.exitCode}.`;
  if (!l.woke) return `The sandbox was awake already: the agent waited ${secs(l.waitedMs ?? 0)}.${exit}`;
  if (l.wakeMs === undefined) return `The agent waited ${secs(l.waitedMs ?? 0)}.${exit}`;
  const hidden = l.hiddenMs ?? 0;
  return (
    `The wake took ${secs(l.wakeMs)} and the agent waited ${secs(l.waitedMs ?? 0)}` +
    (hidden > 0 ? `: ${secs(hidden)} of it was hidden behind the thinking.` : ".") +
    exit
  );
}
