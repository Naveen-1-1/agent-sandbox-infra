import { useCallback, useMemo, useState, type FormEvent } from "react";

import { Api, errorText } from "./api.ts";
import { useFleet, useRoute } from "./hooks.ts";
import { Sandboxes } from "./pages/Sandboxes.tsx";
import { Settings } from "./pages/Settings.tsx";
import { saveToken, takeToken } from "./session.ts";

export function App() {
  const [token, setToken] = useState(() => takeToken(location, history, sessionStorage));
  const [notice, setNotice] = useState<string>();
  const signOut = useCallback((why?: string) => {
    saveToken(sessionStorage, null);
    setNotice(why);
    setToken(null);
  }, []);
  if (!token) {
    return (
      <SignIn
        notice={notice}
        onToken={(t) => {
          saveToken(sessionStorage, t);
          setToken(t);
        }}
      />
    );
  }
  return <Shell token={token} signOut={signOut} />;
}

function Shell({ token, signOut }: { token: string; signOut: (why?: string) => void }) {
  const api = useMemo(() => new Api(token), [token]);
  const [route, go] = useRoute();
  const rejected = useCallback(() => signOut("The gateway rejected the token."), [signOut]);
  const fleet = useFleet(api, rejected);
  return (
    <div className="app">
      <header className="top">
        <a
          className="brand"
          href="/ui/"
          onClick={(e) => {
            e.preventDefault();
            go({ page: "sandboxes" });
          }}
        >
          sbx
        </a>
        <nav>
          <button type="button" className={route.page === "sandboxes" ? "on" : ""} onClick={() => go({ page: "sandboxes" })}>
            Sandboxes
          </button>
          <button type="button" className={route.page === "settings" ? "on" : ""} onClick={() => go({ page: "settings" })}>
            Settings
          </button>
        </nav>
        <span className="spacer" />
        <button type="button" className="quiet" onClick={() => signOut()}>
          Sign out
        </button>
      </header>
      {route.page === "settings" ? (
        <Settings api={api} />
      ) : (
        <Sandboxes
          api={api}
          fleet={fleet}
          selected={route.selected}
          select={(k) => go({ page: "sandboxes", selected: k })}
        />
      )}
    </div>
  );
}

function SignIn({ notice, onToken }: { notice?: string; onToken: (t: string) => void }) {
  const [value, setValue] = useState("");
  const [error, setError] = useState(notice);
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const t = value.trim();
    setBusy(true);
    try {
      await new Api(t).policy(); // admin only: checks the token
      onToken(t);
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <main className="signin">
      <form className="panel" onSubmit={submit}>
        <h1>sbx</h1>
        <p className="muted">
          Sign in with the gateway's admin token. <code>make ui</code> prints a link that signs you in.
        </p>
        <label>
          Admin token
          <input type="password" autoComplete="off" required value={value} onChange={(e) => setValue(e.target.value)} />
        </label>
        <button type="submit" className="primary" disabled={busy}>
          Sign in
        </button>
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
      </form>
    </main>
  );
}
