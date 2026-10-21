import { useEffect, useState, type FormEvent } from "react";

import { errorText, type Api, type PolicySettings, type PolicySpec } from "../api.ts";

const sources: Record<string, string> = {
  "sbx-policy": "set here",
  "sbx-policy-defaults": "the chart's default",
  "built-in": "built in: no policy is configured",
};

// formParams keeps the parameters the policy has that were filled in;
// empty ones fall back to their defaults.
export function formParams(spec: PolicySpec | undefined, values: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const p of spec?.params ?? []) {
    const v = values[p.name]?.trim();
    if (v) out[p.name] = v;
  }
  return out;
}

// Settings switches the cluster's sleep policy: the one every sandbox uses
// unless it names its own. The controller picks a change up at once.
export function Settings({ api }: { api: Api }) {
  const [current, setCurrent] = useState<PolicySettings>();
  const [name, setName] = useState("");
  const [values, setValues] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState<{ ok: boolean; text: string }>();

  const show = (p: PolicySettings) => {
    setCurrent(p);
    setName(p.name);
    setValues(p.params ?? {});
  };
  useEffect(() => {
    api.policy().then(show, (err: unknown) => setStatus({ ok: false, text: errorText(err) }));
  }, [api]);

  if (!current) {
    return <section className="panel">{status ? <p className="error">{status.text}</p> : <p className="muted">Loading…</p>}</section>;
  }
  const spec = current.specs.find((s) => s.name === name);
  const pick = (n: string) => {
    setName(n);
    setValues(n === current.name ? (current.params ?? {}) : {});
    setStatus(undefined);
  };
  const run = async (f: () => Promise<PolicySettings>, done: string) => {
    setBusy(true);
    setStatus(undefined);
    try {
      show(await f());
      setStatus({ ok: true, text: done });
    } catch (err) {
      setStatus({ ok: false, text: errorText(err) });
    } finally {
      setBusy(false);
    }
  };
  const save = (e: FormEvent) => {
    e.preventDefault();
    void run(
      () => api.setPolicy({ name, params: formParams(spec, values) }),
      "Saved. Every sandbox without a policy of its own follows it now.",
    );
  };

  return (
    <section className="panel settings">
      <header className="panel-head">
        <h2>Sleep policy</h2>
        <p className="muted">
          Decides when an idle sandbox goes to sleep: its files go to S3 and its pod and disk are deleted. Sandboxes
          created with a policy of their own keep it.
        </p>
      </header>
      <p>
        Now: <strong>{current.name}</strong> <span className="muted">({sources[current.source] ?? current.source})</span>
      </p>
      {current.invalid && (
        <p className="warn" role="alert">
          A policy was skipped, and the controller keeps the last good one: {current.invalid}
        </p>
      )}
      <form onSubmit={save}>
        <fieldset className="choices">
          <legend>Policy</legend>
          {current.specs.map((s) => (
            <label key={s.name} className={`choice${s.name === name ? " on" : ""}`}>
              <input type="radio" name="policy" value={s.name} checked={s.name === name} onChange={() => pick(s.name)} />
              <span>
                <strong>{s.name}</strong>
                <span className="muted">{s.help}</span>
              </span>
            </label>
          ))}
        </fieldset>
        {spec?.params?.length ? (
          <fieldset className="params">
            <legend>Parameters</legend>
            {spec.params.map((p) => (
              <label key={p.name} className="param">
                <span className="param-name">{p.name}</span>
                <input
                  name={p.name}
                  value={values[p.name] ?? ""}
                  placeholder={p.default || "(none)"}
                  onChange={(e) => setValues({ ...values, [p.name]: e.target.value })}
                />
                <span className="muted">
                  {p.help}
                  {p.default && ` Default: ${p.default}.`}
                </span>
              </label>
            ))}
          </fieldset>
        ) : null}
        <div className="row">
          <button type="submit" className="primary" disabled={busy || !spec}>
            Save
          </button>
          <button
            type="button"
            disabled={busy || current.source !== "sbx-policy"}
            onClick={() => void run(api.resetPolicy, "Reset to the chart's default.")}
          >
            Reset to the chart's default
          </button>
        </div>
        {status && (
          <p className={status.ok ? "ok" : "error"} role="status">
            {status.text}
          </p>
        )}
      </form>
    </section>
  );
}
