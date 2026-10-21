import { beforeEach, describe, expect, it } from "vitest";

import { parseRoute, routePath, saveToken, takeToken, type Route } from "../src/session.ts";

describe("takeToken", () => {
  beforeEach(() => {
    sessionStorage.clear();
    history.replaceState(null, "", "/ui/");
  });

  it("moves the token from the fragment to session storage", () => {
    history.replaceState(null, "", "/ui/settings?x=1#token=s3cr%2Bt");
    expect(takeToken(location, history, sessionStorage)).toBe("s3cr+t");
    expect(location.hash).toBe("");
    expect(location.pathname + location.search).toBe("/ui/settings?x=1");
    expect(takeToken(location, history, sessionStorage)).toBe("s3cr+t"); // from storage now
  });

  it("is empty until one is given, and after signing out", () => {
    expect(takeToken(location, history, sessionStorage)).toBeNull();
    saveToken(sessionStorage, "t");
    expect(takeToken(location, history, sessionStorage)).toBe("t");
    saveToken(sessionStorage, null);
    expect(takeToken(location, history, sessionStorage)).toBeNull();
  });
});

describe("routes", () => {
  it("round-trip", () => {
    const routes: Route[] = [{ page: "sandboxes" }, { page: "settings" }, { page: "sandboxes", selected: "team a/ws-1" }];
    for (const r of routes) expect(parseRoute(routePath(r))).toEqual(r);
  });

  it("send anything unknown to the sandboxes", () => {
    expect(parseRoute("/ui/w/only-namespace")).toEqual({ page: "sandboxes" });
    expect(parseRoute("/elsewhere")).toEqual({ page: "sandboxes" });
  });
});
