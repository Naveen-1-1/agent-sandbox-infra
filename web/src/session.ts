// The admin token and the page's own routes.
//
// `sbxctl ui` prints /ui/#token=…: the fragment never reaches a server or
// a Referer. The page moves it to session storage (gone with the tab) and
// drops it from the address bar and history.

const tokenKey = "sbx.token";

export function takeToken(loc: Location, history: History, storage: Storage): string | null {
  const m = /(?:^#|&)token=([^&]+)/.exec(loc.hash);
  if (m) {
    const token = decodeURIComponent(m[1]);
    history.replaceState(history.state, "", loc.pathname + loc.search);
    saveToken(storage, token); // with storage off, it lasts for this page only
    return token;
  }
  try {
    return storage.getItem(tokenKey);
  } catch {
    return null;
  }
}

export function saveToken(storage: Storage, token: string | null): void {
  try {
    if (token) storage.setItem(tokenKey, token);
    else storage.removeItem(tokenKey);
  } catch {
    // storage is off
  }
}

// Routes, under /ui/:
//
//	/ui/                      sandboxes
//	/ui/w/{namespace}/{name}  sandboxes, with one selected
//	/ui/settings              the sleep policy
export type Route = { page: "sandboxes"; selected?: string } | { page: "settings" };

export const base = "/ui/";

export function parseRoute(pathname: string): Route {
  const rest = pathname.startsWith(base) ? pathname.slice(base.length) : "";
  const parts = rest.split("/").filter(Boolean).map(decodeURIComponent);
  if (parts[0] === "settings") return { page: "settings" };
  if (parts[0] === "w" && parts.length === 3) return { page: "sandboxes", selected: `${parts[1]}/${parts[2]}` };
  return { page: "sandboxes" };
}

export function routePath(r: Route): string {
  if (r.page === "settings") return base + "settings";
  if (r.selected) {
    const [ns, name] = r.selected.split("/");
    return `${base}w/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`;
  }
  return base;
}
