## Summary

- Over 2 sessions, never sleeping costs $0.13; the baseline (fixed 15m, no hint) costs $0.06, 49.9% less, with 1 visible waits (0.0 h in all).
- fixed 15m + hint: 21.4% dearer vs the baseline, visible waits 100.0% fewer, and 100.0% of the wakes for a person's message finish before the agent's first call.
- fixed 5m + hint: 53.7% cheaper vs the baseline, visible waits 100.0% fewer, and 100.0% of the wakes for a person's message finish before the agent's first call.
- fixed 2m + hint: 79.7% cheaper vs the baseline, visible waits 100.0% fewer, and 100.0% of the wakes for a person's message finish before the agent's first call.
- adaptive (15m mid-turn, 2m after the turn) + hint: 77.1% cheaper vs the baseline, visible waits 100.0% fewer, and 100.0% of the wakes for a person's message finish before the agent's first call.

## Sessions

| | Claude Code | Codex | Total |
|---|---:|---:|---:|
| Raw sessions in the trace | 2 | 1 | 3 |
| of which subagents | 1 | 0 | 1 |
| Root sessions (subagents grouped) | 1 | 1 | 2 |

Expected 2 roots: matches. Simulated 2 (0 have no timestamps); 0 subagents had no parent in the trace.

## Where the time goes

| Class | Hours | Share of session time | Gaps | Median | p90 |
|---|---:|---:|---:|---:|---:|
| Tool running | 0.0 | 1.5% | 4 | 1.0s | 20.0s |
| AI thinking | 0.0 | 5.3% | 8 | 2.0s | 59.0s |
| Human wait | 0.5 | 93.2% | 2 | 9.8 min | 20.0 min |
| Session tail | 1.0 | (30 min per session) | 2 | 30.0 min | 30.0 min |

A person's input to the agent's first sandbox call (the window a hinted wake hides in): median 5.0s, p90 10.0s over 3 inputs; 33.3% are at least a wake to serving (10.0s), 0.0% at least a wake with dependencies (20.0s).

## Policies

| Config | Total cost | Saved vs baseline | Saved vs never | $ / session | Visible waits | Δ vs baseline | Total wait | Hidden wakes | Visible wait p50 / p90 / max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| never sleep | $0.13 | -99.7% | +0.0% | $0.06256 | 0 | -100.0% | 0.0 h | – | – |
| fixed 15m, no hint (baseline) | $0.06 | +0.0% | +49.9% | $0.03133 | 1 | +0.0% | 0.0 h | 0.0% (0 of 1) | 10.0s / 10.0s / 10.0s |
| fixed 15m + hint | $0.08 | -21.4% | +39.2% | $0.03803 | 0 | -100.0% | 0.0 h | 100.0% (1 of 1) | – |
| fixed 5m + hint | $0.03 | +53.7% | +76.8% | $0.01451 | 0 | -100.0% | 0.0 h | 100.0% (1 of 1) | – |
| fixed 2m + hint | $0.01 | +79.7% | +89.9% | $0.00635 | 0 | -100.0% | 0.0 h | 100.0% (1 of 1) | – |
| adaptive (15m mid-turn, 2m after the turn) + hint | $0.01 | +77.1% | +88.5% | $0.00716 | 0 | -100.0% | 0.0 h | 100.0% (1 of 1) | – |

A hint costs something too: it wakes a sleeping sandbox even when no sandbox call follows (unused hints, below), and, like any turn starting, restarts the idle timer.

## Policy detail

| Config | Awake h | Suspending h | Waking h | Asleep h | Suspends | Aborted | Hint wakes | Request wakes | Unused hints | Waits: user message / mid-turn / deps / abort |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| never | 1.5 | 0.0 | 0.0 | 0.0 | 0 | 0 | 0 | 0 | 0 | 0 / 0 / 0 / 0 |
| fixed-15m-nohint | 0.8 | 0.0 | 0.0 | 0.8 | 3 | 0 | 0 | 1 | 0 | 1 / 0 / 0 / 0 |
| fixed-15m | 0.9 | 0.0 | 0.0 | 0.6 | 3 | 0 | 1 | 0 | 0 | 0 / 0 / 0 / 0 |
| fixed-5m | 0.3 | 0.0 | 0.0 | 1.2 | 4 | 0 | 2 | 0 | 1 | 0 / 0 / 0 / 0 |
| fixed-2m | 0.1 | 0.0 | 0.0 | 1.4 | 4 | 0 | 2 | 0 | 1 | 0 / 0 / 0 / 0 |
| adaptive | 0.2 | 0.0 | 0.0 | 1.4 | 4 | 0 | 2 | 0 | 1 | 0 / 0 / 0 / 0 |

Visible waits of user-message wakes that weren't hidden, by length:

| Config | ≤1s | ≤2s | ≤5s | ≤10s | ≤20s | ≤30s | ≤60s | >60s |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| never | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| fixed-15m-nohint | 0 | 0 | 0 | 1 | 0 | 0 | 0 | 0 |
| fixed-15m | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| fixed-5m | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| fixed-2m | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| adaptive | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |

By agent:

- claude: 1 sessions, baseline $0.04, best config saves 84.3%.
- codex: 1 sessions, baseline $0.02, best config saves 70.5%.

## At scale

Simulated at $0.0816/h (one m7g.large on demand); the benched sandbox costs $0.0408/h, so savings scale by 0.500. Storage and requests are 0.1% of the baseline's cost.

| Config | Saved / session (reference) | Saved / session (benched sandbox) | 1M sessions/yr | 10M | 100M |
|---|---:|---:|---:|---:|---:|
| never | $-0.03123 | $-0.01561 | $-15.6k | $-156.1k | $-1.56M |
| fixed-15m | $-0.00670 | $-0.00335 | $-3.3k | $-33.5k | $-334.9k |
| fixed-5m | $0.01682 | $0.00841 | $8.4k | $84.1k | $841.2k |
| fixed-2m | $0.02498 | $0.01249 | $12.5k | $124.9k | $1.25M |
| adaptive | $0.02417 | $0.01208 | $12.1k | $120.8k | $1.21M |

## Assumptions

- One sandbox per session group (a session and its subagents, grouped by session id), awake from its first event; deleted 30m0s after its last event (the session tail), snapshot included.
- Only what the gateway sees is activity: sandbox tool calls (Bash, Read, Edit, exec_command, apply_patch, ...) starting and ending, wake-hints, and wakes finishing. Model text, reasoning, web and MCP tools are invisible to it.
- A running sandbox call keeps the sandbox awake (the policy's command veto), however long it runs; the emitted-to-result span counts, approval prompts included.
- A person's input is the user message that starts a user-initiated round, or an answer to AskUserQuestion; with hints the harness sends a wake-hint then, which also counts as activity. Turn-ended is sent at a turn's last agent-side instant in every config.
- Wakes take the bench's averages: 10.0s to serving, 20.0s to dependencies; a suspend takes 5.0s and is billed awake. A call during a suspend cuts it short at no wait.
- Calls needing dependencies: auto (the trace has no executables, so shell calls can't be told apart).
- Holds are closed-loop: a held call delays everything after it in its session.
- Hints are always delivered; the policy decides instantly (the controller polls every few seconds); the operation limiter and cluster capacity are ignored.
- Asleep, a sandbox pays for 50.0 MB in S3 (snapshot plus dependency archive, not shared); each suspend makes 2 PUT-class and each wake 8 GET-class requests.
- Prices: $0.0816/h awake (one m7g.large on demand), S3 $0.023/GB-month, $0.0050 per 1k PUT, $0.0004 per 1k GET (AWS us-east-1 list prices). No provisioning cost.
- Extrapolation multiplies per-session savings at the reference price by the benched sandbox's price over the reference; storage and request costs are scaled with it although they don't depend on the sandbox's size (see their share).

## Data quality

- 0 tool calls never returned (taken as instant), 0 returned before they started, 0 duplicate call ids.
- Tools counted as the model's own time (not sandbox, subagent or human), most common: .
