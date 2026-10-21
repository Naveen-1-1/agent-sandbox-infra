import { useCallback, useEffect, useState, type FormEvent } from "react";

import { errorText, key, type Api, type Workspace } from "../api.ts";
import { Phase } from "../components/Phase.tsx";
import { demoPolicy, playReply, type ReplyOptions } from "../demo.ts";
import { sorted, type Fleet } from "../fleet.ts";
import type { Reply } from "../timeline.ts";
import { Sandbox } from "./Sandbox.tsx";

const keepReplies = 4;
let nextReply = 0;

// Sandboxes lists every sandbox live, creates new ones from a git URL, and
// shows the selected one with the hidden-wake demo.
export function Sandboxes({
  api,
  fleet,
  selected,
  select,
}: {
  api: Api;
  fleet: Fleet;
  selected?: string;
  select: (key?: string) => void;
}) {
  // Replies live here so they survive switching between sandboxes.
  const [replies, setReplies] = useState<Record<string, Reply[]>>({});
  const reply = useCallback(
    (ws: Workspace, o: Omit<ReplyOptions, "id">) => {
      const k = key(ws);
      void playReply(api, ws, { ...o, id: ++nextReply }, (r) =>
        setReplies((all) => {
          const list = all[k] ?? [];
          const i = list.findIndex((x) => x.id === r.id);
          const next = i < 0 ? [r, ...list].slice(0, keepReplies) : list.map((x) => (x.id === r.id ? r : x));
          return { ...all, [k]: next };
        }),
      );
    },
    [api],
  );

  // A sleeping sandbox has no disk, so no dependency status: remember the
  // profile it had for the demo's default command.
  const [profiles, setProfiles] = useState<Record<string, string>>({});
  useEffect(() => {
    setProfiles((known) => {
      let next = known;
      for (const [k, w] of Object.entries(fleet.items)) {
        const p = w.deps?.profile;
        if (p && known[k] !== p) next = { ...next, [k]: p };
      }
      return next;
    });
  }, [fleet.items]);

  const list = sorted(fleet);
  const ws = selected ? fleet.items[selected] : undefined;
  return (
    <div className="split">
      <aside className="side">
        <CreateForm api={api} onCreated={(w) => select(key(w))} />
        <section className="panel">
          <header className="panel-head row">
            <h2>Sandboxes</h2>
            <span className={`live${fleet.live ? " on" : ""}`} title={fleet.error}>
              {fleet.live ? "live" : fleet.error ? "reconnecting…" : "connecting…"}
            </span>
          </header>
          {list.length === 0 ? (
            <p className="muted">{fleet.live ? "None yet: create one above." : "Loading…"}</p>
          ) : (
            <ul className="ws-list">
              {list.map((w) => (
                <li key={key(w)}>
                  <button
                    type="button"
                    className={`ws-item${key(w) === selected ? " on" : ""}`}
                    aria-current={key(w) === selected ? "true" : undefined}
                    onClick={() => select(key(w))}
                  >
                    <span className="ws-name">
                      {w.name}
                      <span className="muted"> {w.namespace}</span>
                    </span>
                    <Phase phase={w.phase} />
                    <span className="ws-meta muted">
                      epoch {w.epoch}
                      {w.deps?.profile && ` · ${w.deps.profile}${w.deps.pending ? " restoring" : ""}`}
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </section>
      </aside>
      <main className="main">
        {ws ? (
          <Sandbox
            key={key(ws)}
            api={api}
            ws={ws}
            profile={ws.deps?.profile || profiles[key(ws)]}
            replies={replies[key(ws)] ?? []}
            onReply={(o) => reply(ws, o)}
            onDeleted={() => select(undefined)}
          />
        ) : (
          <section className="panel empty">
            {selected && fleet.live ? (
              <p>{selected} is gone.</p>
            ) : selected ? (
              <p className="muted">Loading {selected}…</p>
            ) : (
              <>
                <h2>Sleep cheap, wake invisibly</h2>
                <p className="muted">
                  Create a sandbox from a git repository, or pick one. End the agent's turn and it goes to sleep: its
                  files go to S3 and its pod and disk are deleted. When the user replies, a wake-hint starts the wake
                  while the LLM is still thinking, so the agent doesn't wait.
                </p>
              </>
            )}
          </section>
        )}
      </main>
    </div>
  );
}

function CreateForm({ api, onCreated }: { api: Api; onCreated: (w: Workspace) => void }) {
  const [repoURL, setRepoURL] = useState("");
  const [ref, setRef] = useState("");
  const [image, setImage] = useState("");
  const [demo, setDemo] = useState(true);
  const [afterTurn, setAfterTurn] = useState(5);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      const resp = await api.create({
        repoURL: repoURL.trim(),
        ref: ref.trim() || undefined,
        image: image.trim() || undefined,
        policy: demo ? demoPolicy(afterTurn) : undefined,
      });
      setRepoURL("");
      setRef("");
      onCreated(resp.workspace);
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="panel">
      <form className="create" onSubmit={submit}>
        <h2>New sandbox</h2>
        <label>
          Git repository
          <input
            type="url"
            required
            placeholder="https://github.com/owner/repo.git"
            value={repoURL}
            onChange={(e) => setRepoURL(e.target.value)}
          />
        </label>
        <div className="row">
          <label>
            Ref
            <input placeholder="default branch" value={ref} onChange={(e) => setRef(e.target.value)} />
          </label>
          <label>
            Image
            <input placeholder="from devcontainer.json" value={image} onChange={(e) => setImage(e.target.value)} />
          </label>
        </div>
        <label className="check">
          <input type="checkbox" checked={demo} onChange={(e) => setDemo(e.target.checked)} />
          <span>
            Demo policy: sleep{" "}
            <input
              type="number"
              min={1}
              max={600}
              className="tiny"
              aria-label="seconds after a turn ends"
              value={afterTurn}
              disabled={!demo}
              onChange={(e) => setAfterTurn(Math.max(1, Number(e.target.value) || 1))}
            />{" "}
            s after the agent's turn ends (else the cluster's policy)
          </span>
        </label>
        <button type="submit" className="primary" disabled={busy}>
          {busy ? "Creating…" : "Create"}
        </button>
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
      </form>
    </section>
  );
}
