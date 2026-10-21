import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";

import { Api, type PolicySettings } from "../src/api.ts";
import { formParams, Settings } from "../src/pages/Settings.tsx";

afterEach(cleanup);

const specs: PolicySettings["specs"] = [
  {
    name: "fixed",
    help: "Suspend after a fixed time without activity.",
    params: [
      { name: "idleTimeout", default: "10m", help: "Suspend after this long without activity." },
      { name: "afterTurnEnded", default: "2m", help: "Suspend this soon after the turn ends." },
    ],
  },
  { name: "never", help: "Never suspend on its own." },
];

// gateway fakes /api/v1/policy and records what was sent.
function gateway(initial: PolicySettings) {
  let cur = initial;
  const sent: { method: string; body?: unknown }[] = [];
  const api = new Api("admin", async (path, init) => {
    expect(path).toBe("/api/v1/policy");
    expect((init?.headers as Record<string, string>).Authorization).toBe("Bearer admin");
    const method = init?.method ?? "GET";
    const body = init?.body ? JSON.parse(init.body as string) : undefined;
    sent.push({ method, body });
    if (method === "PUT") {
      if (body.params?.idleTimeout === "soon") {
        return Response.json({ error: 'idleTimeout: invalid duration "soon"', code: "invalid" }, { status: 400 });
      }
      cur = { ...cur, name: body.name, params: body.params, source: "sbx-policy", invalid: undefined };
    }
    if (method === "DELETE") cur = { ...cur, name: "never", params: undefined, source: "sbx-policy-defaults" };
    return Response.json(cur);
  });
  return { api, sent };
}

describe("Settings", () => {
  it("saves the chosen policy with the parameters filled in", async () => {
    const { api, sent } = gateway({ name: "never", source: "sbx-policy-defaults", specs });
    render(<Settings api={api} />);
    const user = userEvent.setup();
    expect(await screen.findByText("(the chart's default)")).toBeTruthy();
    expect((screen.getByRole("button", { name: /Reset/ }) as HTMLButtonElement).disabled).toBe(true);

    await user.click(screen.getByRole("radio", { name: /fixed/ }));
    const idle = screen.getByRole("textbox", { name: /idleTimeout/ }) as HTMLInputElement;
    expect(idle.placeholder).toBe("10m");
    await user.type(idle, "30s");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText(/Saved/)).toBeTruthy();
    expect(sent.at(-1)).toEqual({ method: "PUT", body: { name: "fixed", params: { idleTimeout: "30s" } } });
    expect(screen.getByText("(set here)")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: /Reset/ }));
    expect(await screen.findByText(/Reset to the chart's default\./)).toBeTruthy();
    expect(sent.at(-1)?.method).toBe("DELETE");
    expect((screen.getByRole("radio", { name: /never/ }) as HTMLInputElement).checked).toBe(true);
  });

  it("shows why the gateway refused a policy", async () => {
    const { api } = gateway({ name: "fixed", params: { idleTimeout: "5m" }, source: "sbx-policy", specs });
    render(<Settings api={api} />);
    const user = userEvent.setup();
    const idle = (await screen.findByRole("textbox", { name: /idleTimeout/ })) as HTMLInputElement;
    expect(idle.value).toBe("5m");
    await user.clear(idle);
    await user.type(idle, "soon");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(/invalid duration "soon" \(invalid\)/)).toBeTruthy();
  });

  it("warns about a config the controller skipped", async () => {
    const { api } = gateway({ name: "never", source: "built-in", invalid: "sbx-policy: unknown policy \"nap\"", specs });
    render(<Settings api={api} />);
    expect((await screen.findByRole("alert")).textContent).toContain('unknown policy "nap"');
  });
});

describe("formParams", () => {
  it("keeps the policy's filled-in parameters only", () => {
    expect(formParams(specs[0], { idleTimeout: " 30s ", afterTurnEnded: "", other: "x" })).toEqual({ idleTimeout: "30s" });
    expect(formParams(undefined, { idleTimeout: "1m" })).toEqual({});
  });
});
