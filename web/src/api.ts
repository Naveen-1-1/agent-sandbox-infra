// The gateway's REST API (internal/sbxapi): its JSON types and calls.

export interface Condition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
}

// Timeline is when each stage of the last wake finished.
export interface Timeline {
  trigger?: string;
  requestedAt?: string;
  podScheduledAt?: string;
  filesReadyAt?: string;
  readyAt?: string;
  depsReadyAt?: string;
}

export interface Deps {
  profile?: string; // npm, pip, uv; empty if none
  source?: string; // None, Present, Cache or Install
  pending: boolean;
  ready: boolean;
  message?: string;
}

export interface PolicyRef {
  name: string;
  params?: Record<string, string>;
}

export interface Sleep {
  policy?: string;
  reason: string;
  message?: string;
  suspendAt?: string;
}

export interface Snapshot {
  seq: number;
  bytes: number;
  createdAt?: string;
}

export interface Workspace {
  namespace: string;
  name: string;
  uid: string;
  repoURL: string;
  ref?: string;
  image?: string;
  imageSource?: string;
  mode: string;
  phase: string;
  epoch: number;
  ready: boolean;
  message?: string;
  node?: string;
  conditions?: Condition[];
  lastSnapshot?: Snapshot;
  timeline?: Timeline;
  policy?: PolicyRef;
  sleep?: Sleep;
  deps?: Deps;
  createdAt: string;
}

export interface CreateRequest {
  namespace?: string;
  name?: string;
  repoURL: string;
  ref?: string;
  image?: string;
  policy?: PolicyRef;
}

export interface CreateResponse {
  workspace: Workspace;
  token: string;
}

export interface ExecRequest {
  command: string[];
  cwd?: string;
  timeoutSeconds?: number;
  waitFor?: string[];
}

export interface ExecResponse {
  exitCode: number;
  stdout: string;
  stderr: string;
  waitedMillis: number; // held for a wake
  depsWaitedMillis: number; // then held for dependencies
}

export interface PolicyParam {
  name: string;
  default: string;
  help: string;
}

export interface PolicySpec {
  name: string;
  help: string;
  params?: PolicyParam[];
}

export interface PolicySettings {
  name: string;
  params?: Record<string, string>;
  source: string; // sbx-policy, sbx-policy-defaults or built-in
  invalid?: string;
  specs: PolicySpec[];
}

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

export type Fetch = (input: string, init?: RequestInit) => Promise<Response>;

export const key = (w: { namespace: string; name: string }) => `${w.namespace}/${w.name}`;

// Api calls the gateway with the admin token.
export class Api {
  readonly token: string;
  private readonly fetch: Fetch;

  constructor(token: string, fetchFn: Fetch = (input, init) => fetch(input, init)) {
    this.token = token;
    this.fetch = fetchFn;
  }

  async request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
    const headers: Record<string, string> = { Authorization: `Bearer ${this.token}` };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const resp = await this.fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    });
    if (!resp.ok) throw await apiError(resp);
    const text = await resp.text();
    return (text ? JSON.parse(text) : undefined) as T;
  }

  // events opens the workspace event stream.
  async events(signal: AbortSignal): Promise<ReadableStream<Uint8Array>> {
    const resp = await this.fetch("/api/v1/events", {
      headers: { Authorization: `Bearer ${this.token}`, Accept: "text/event-stream" },
      signal,
    });
    if (!resp.ok) throw await apiError(resp);
    if (!resp.body) throw new ApiError(resp.status, "", "the event stream has no body");
    return resp.body;
  }

  list = () => this.request<Workspace[]>("GET", "/api/v1/workspaces");
  create = (r: CreateRequest) => this.request<CreateResponse>("POST", "/api/v1/workspaces", r);
  get = (ns: string, name: string) => this.request<Workspace>("GET", wsPath(ns, name));
  remove = (ns: string, name: string) => this.request<void>("DELETE", wsPath(ns, name));
  exec = (ns: string, name: string, r: ExecRequest, signal?: AbortSignal) =>
    this.request<ExecResponse>("POST", wsPath(ns, name, "exec"), r, signal);
  wake = (ns: string, name: string) => this.request<Workspace>("POST", wsPath(ns, name, "wake"));
  wakeHint = (ns: string, name: string) => this.request<void>("POST", wsPath(ns, name, "wake-hint"));
  turnEnded = (ns: string, name: string) => this.request<void>("POST", wsPath(ns, name, "turn-ended"));
  suspend = (ns: string, name: string) => this.request<void>("POST", wsPath(ns, name, "suspend"));
  editor = (ns: string, name: string, port = 0) =>
    this.request<{ url: string }>("POST", wsPath(ns, name, "editor"), port ? { port } : {});

  policy = () => this.request<PolicySettings>("GET", "/api/v1/policy");
  setPolicy = (p: PolicyRef) => this.request<PolicySettings>("PUT", "/api/v1/policy", p);
  resetPolicy = () => this.request<PolicySettings>("DELETE", "/api/v1/policy");
}

function wsPath(ns: string, name: string, ...rest: string[]): string {
  return ["/api/v1/workspaces", ns, name, ...rest].map((s, i) => (i === 0 ? s : encodeURIComponent(s))).join("/");
}

async function apiError(resp: Response): Promise<ApiError> {
  let code = "";
  let message = resp.statusText || `HTTP ${resp.status}`;
  try {
    const e = (await resp.json()) as { error?: string; code?: string };
    code = e.code ?? "";
    message = e.error ?? message;
  } catch {
    // not JSON: keep the status text
  }
  return new ApiError(resp.status, code, message);
}

export function errorText(err: unknown): string {
  if (err instanceof ApiError) return err.code ? `${err.message} (${err.code})` : err.message;
  if (err instanceof Error) return err.message;
  return String(err);
}
