import { useCallback, useEffect, useReducer, useState } from "react";

import { Api, ApiError, errorText, type Workspace } from "./api.ts";
import { emptyFleet, fleetReducer, type Fleet } from "./fleet.ts";
import { backoff, readEvents } from "./sse.ts";
import { parseRoute, routePath, type Route } from "./session.ts";

// useFleet keeps the workspace list live from the event stream, and
// reconnects with backoff when the stream drops. onUnauthorized is called
// if the gateway rejects the token.
export function useFleet(api: Api, onUnauthorized: () => void): Fleet {
  const [fleet, dispatch] = useReducer(fleetReducer, emptyFleet);
  useEffect(() => {
    const ctl = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    let attempt = 0;
    const connect = async () => {
      dispatch({ type: "connecting" });
      let closed = "the event stream closed";
      try {
        const body = await api.events(ctl.signal);
        await readEvents(body, (ev) => {
          const data = JSON.parse(ev.data) as unknown;
          switch (ev.event) {
            case "workspace":
              dispatch({ type: "workspace", workspace: data as Workspace });
              break;
            case "deleted":
              dispatch({ type: "deleted", ...(data as { namespace: string; name: string }) });
              break;
            case "ready":
              attempt = 0;
              dispatch({ type: "ready" });
              break;
            case "error":
              closed = (data as { error: string }).error;
              break;
          }
        });
        dispatch({ type: "disconnected", error: closed });
      } catch (err) {
        if (ctl.signal.aborted) return;
        if (err instanceof ApiError && err.status === 401) {
          onUnauthorized();
          return;
        }
        dispatch({ type: "disconnected", error: errorText(err) });
      }
      if (!ctl.signal.aborted) timer = setTimeout(connect, backoff(attempt++));
    };
    void connect();
    return () => {
      ctl.abort();
      clearTimeout(timer);
    };
  }, [api, onUnauthorized]);
  return fleet;
}

// useRoute follows the address bar.
export function useRoute(): [Route, (r: Route) => void] {
  const [route, setRoute] = useState(() => parseRoute(location.pathname));
  useEffect(() => {
    const onPop = () => setRoute(parseRoute(location.pathname));
    addEventListener("popstate", onPop);
    return () => removeEventListener("popstate", onPop);
  }, []);
  const go = useCallback((r: Route) => {
    const path = routePath(r);
    if (path !== location.pathname) history.pushState(null, "", path);
    setRoute(r);
  }, []);
  return [route, go];
}

// useNow ticks while active, for countdowns and running bars.
export function useNow(active: boolean, every = 250): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    setNow(Date.now());
    const t = setInterval(() => setNow(Date.now()), every);
    return () => clearInterval(t);
  }, [active, every]);
  return now;
}
