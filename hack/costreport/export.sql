-- Export TraceLab v0.0.1 for internal/costsim: one JSON line per raw
-- session, with its events and tool calls in order and its root (the
-- session a subagent belongs to). Run from the repository root:
--   duckdb -readonly data/tracelab/syfi_coding_trace.duckdb -c ".read hack/costreport/export.sql"
COPY (
WITH r AS (
  SELECT round_pk, provider, session_id, "user" AS usr, project, round_index, ingest_seq,
         coalesce(first_input_event_type = 'user_message', false) AS user_initiated
  FROM rounds),
-- The prompt of a user-initiated round is its last user message before any
-- model output: meta and command records can come just before it.
first_out AS (
  SELECT round_pk, min(event_index) AS idx FROM timing_events
  WHERE event_type IN ('reasoning', 'text', 'tool_call') GROUP BY round_pk),
prompt AS (
  SELECT e.round_pk, max(e.event_index) AS idx
  FROM timing_events e LEFT JOIN first_out f USING (round_pk)
  WHERE e.event_type = 'user_message' AND (f.idx IS NULL OR e.event_index < f.idx)
  GROUP BY e.round_pk),
ev AS (
  SELECT r.provider, r.session_id,
         {'t': epoch_us(e."timestamp"), 'u': e.event_type = 'user_message',
          'h': e.event_type = 'user_message' AND r.user_initiated AND e.event_index = prompt.idx} AS x,
         row_number() OVER (PARTITION BY r.provider, r.session_id ORDER BY r.round_index, r.ingest_seq, e.event_index) AS seq
  FROM timing_events e JOIN r USING (round_pk) LEFT JOIN prompt USING (round_pk)
  WHERE e.event_type IN ('user_message', 'reasoning', 'text') AND e."timestamp" IS NOT NULL),
tl AS (
  SELECT r.provider, r.session_id,
         {'id': tc.tool_call_id, 'name': tc.tool_name, 's': epoch_us(tc.emitted_at), 'e': epoch_us(tc.result_at),
          'internalMs': tc.tool_internal_latency_ms} AS x,
         row_number() OVER (PARTITION BY r.provider, r.session_id ORDER BY r.round_index, r.ingest_seq, tc.tool_index) AS seq
  FROM tool_calls tc JOIN r USING (round_pk)
  WHERE tc.emitted_at IS NOT NULL),
s AS (
  SELECT provider, session_id, min(usr) AS usr, min(project) AS project,
         count(*) AS rounds, count(*) FILTER (WHERE user_initiated) AS user_rounds
  FROM r GROUP BY ALL),
evs AS (SELECT provider, session_id, list(x ORDER BY seq) AS events FROM ev GROUP BY ALL),
tls AS (SELECT provider, session_id, list(x ORDER BY seq) AS tools FROM tl GROUP BY ALL)
-- Session ids already start with the provider ("claude:…", "codex:…").
SELECT s.session_id AS "key",
       s.provider,
       s.session_id AS "session",
       split_part(s.session_id, '/subagents', 1) AS root,
       s.usr AS "user",
       s.project,
       s.rounds,
       s.user_rounds AS "userRounds",
       coalesce(evs.events, []) AS events,
       coalesce(tls.tools, []) AS tools
FROM s LEFT JOIN evs USING (provider, session_id) LEFT JOIN tls USING (provider, session_id)
ORDER BY "key"
) TO 'data/costsim/trace.jsonl' (FORMAT json);
