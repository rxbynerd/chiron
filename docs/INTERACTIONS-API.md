# Gemini Interactions API — canonical reference for Chiron

> **Normative for this repository.** All code touching the Gemini API must follow this
> document, not memory and not the proposal. Where docs/PROPOSAL.md §3 disagrees with
> this file, **this file wins** — the proposal was verified against the 2026-04-28 docs
> and the API has drifted since (see "Drift from the proposal" at the end).
>
> `last_verified: 2026-06-07` against:
> - https://ai.google.dev/gemini-api/docs/deep-research
> - https://ai.google.dev/gemini-api/docs/interactions
> - https://ai.google.dev/api/interactions-api
>
> Status: public **beta/preview**. Schemas may change. Re-verify before trusting this
> file if the date above is stale.

## 1. Endpoints and auth

Base: `https://generativelanguage.googleapis.com`

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/v1beta/interactions` | Create an interaction (research task, plan, follow-up) |
| `GET` | `/v1beta/interactions/{id}` | Retrieve; add `?stream=true[&last_event_id=...]` to stream |
| `POST` | `/v1beta/interactions/{id}/cancel` | Cancel an in-flight interaction |
| `DELETE` | `/v1beta/interactions/{id}` | Delete a stored interaction |

Headers on every request:

- `x-goog-api-key: <key>` — auth. Never log this value.
- `Content-Type: application/json` on POST.
- `Api-Revision: 2026-05-20` — pin the schema revision (appears in Google's own curl
  examples; semantics under-documented, but pinning protects us against silent drift).

## 2. Agent identifiers (tiers)

| Chiron flag value | API `agent` id | Notes |
| --- | --- | --- |
| `deep-research` | `deep-research-preview-04-2026` | default; faster |
| `deep-research-max` | `deep-research-max-preview-04-2026` | more comprehensive, ≈2× cost |

(`deep-research-pro-preview-12-2025` also exists; not exposed by Chiron v1.)

## 3. Create request

```json
{
  "agent": "deep-research-preview-04-2026",
  "input": "Research query text",
  "agent_config": {
    "type": "deep-research",
    "thinking_summaries": "auto",
    "visualization": "auto",
    "collaborative_planning": false
  },
  "tools": [ {"type": "google_search"}, {"type": "url_context"}, {"type": "code_execution"} ],
  "background": true,
  "store": true,
  "stream": false,
  "previous_interaction_id": "optional"
}
```

Rules that shape Chiron:

- **`background: true` is mandatory** for deep research, and background **requires
  `store: true`**. (`store` defaults to true; set it explicitly anyway.)
- `input` is `string | Content[] | Step[]`. Multimodal grounding uses typed parts:
  `{"type":"text","text":...}`, `{"type":"image","mime_type":...,"uri":...}` (or
  base64 `data`), `{"type":"document","uri":...,"mime_type":"application/pdf"}`.
- `agent_config` fields: `type` (`"deep-research"`, required discriminator),
  `thinking_summaries` (`"auto"`|`"none"`, default `none`), `visualization`
  (`"auto"`|`"off"`, default `auto`), `collaborative_planning` (bool, default false).
- Default tools when `tools` omitted: `google_search`, `url_context`, `code_execution`.
- Additional tools: `{"type":"mcp_server","name":...,"url":...,"headers":{...},
  "allowed_tools":{"mode":"auto|any|none|validated","tools":[...]}}` and
  `{"type":"file_search","file_search_store_names":["fileSearchStores/..."],"top_k":...}`.
  (The deep-research guide shows `allowed_tools` as a plain array; the API reference
  shows the object form above. Prefer the object form; tolerate both when parsing.)
- **Custom `function` tools are NOT supported** by the deep-research agent; structured
  output (`response_format`) is NOT supported either.
- Follow-up Q&A against a completed interaction uses **`model`** (e.g.
  `gemini-3.1-pro-preview`), not `agent`, plus `previous_interaction_id`.

## 4. Interaction resource (responses)

```json
{
  "id": "v1_...",
  "object": "interaction",
  "agent": "deep-research-preview-04-2026",
  "status": "in_progress",
  "created": "2026-06-07T12:00:00Z",
  "updated": "2026-06-07T12:00:00Z",
  "steps": [
    { "type": "user_input",  "content": [ {"type": "text", "text": "..."} ] },
    { "type": "model_output", "content": [
        { "type": "thought", "text": "..." },
        { "type": "text", "text": "# Report...", "annotations": [
            { "type": "url_citation", "url": "https://...", "title": "...",
              "start_index": 0, "end_index": 42 } ] },
        { "type": "image", "data": "<base64>", "mime_type": "image/png" }
    ] }
  ],
  "usage": {
    "total_input_tokens": 0, "total_cached_tokens": 0, "total_output_tokens": 0,
    "total_tool_use_tokens": 0, "total_thought_tokens": 0, "total_tokens": 0,
    "grounding_tool_count": [ {"type": "google_search", "count": 80} ]
  },
  "previous_interaction_id": "optional"
}
```

- **`status` enum (full):** `in_progress | requires_action | completed | failed |
  cancelled | incomplete | budget_exceeded`. Treat everything other than
  `in_progress` and `completed` as terminal-failure variants for v1 (each with its own
  error message); `requires_action` should not occur for deep research but must not
  hang the poller if it does.
- **The response shape is `steps[]`, not `outputs[]`.** Each step has a `type`
  (`user_input`, `model_output`, tool-call/result types) and `content[]` parts.
- **The final report** is the `text` content of the **last `model_output` step**.
- **Citations** are `annotations` on text content: `url_citation` (url, title,
  byte-offset `start_index`/`end_index`) and `file_citation` (document_uri, file_name,
  page_number, ...). Dedupe by URL for the sources list.
- **Charts** are `image` parts (base64 `data` + `mime_type`) inside `model_output`
  steps when `visualization: "auto"`.
- **Cost signals** come from `usage`: token totals plus `grounding_tool_count` →
  search count.

## 5. Streaming (SSE)

`POST ... {"stream": true, ...}` streams the create; `GET /v1beta/interactions/{id}?stream=true`
re-attaches to a stored background interaction. Events (`text/event-stream`):

| `event_type` | Payload | Notes |
| --- | --- | --- |
| `interaction.created` | `interaction` resource | start of stream |
| `step.delta` | `index` + `delta{type,...}` | delta types include `text`, `thought_summary_delta`, `image`, tool call/result types |
| `interaction.status_update` | `interaction_id`, `status` | poll-free status changes |
| `interaction.completed` | `interaction` (may have empty content) | re-GET for the full resource |
| `error` | `error{code,message}` | `code` is a URI |

- Every event carries an **`event_id`**.
- **Resume after a drop:** `GET /v1beta/interactions/{id}?stream=true&last_event_id=<event_id>`
  resumes from the next chunk after that event. (Resumption is via query parameter —
  the docs do not promise the `Last-Event-ID` *header* is honoured; use the query param.)
- After `interaction.completed`, do a plain GET for the final resource (the completion
  event may omit content).

## 6. Errors

```json
{ "error": { "code": "<URI identifying the error type>", "message": "human-readable" } }
```

Standard HTTP status codes apply (4xx client, 429 rate limit, 5xx transient). Retry
with backoff on 429/5xx/transport errors; never retry 4xx other than 408/429.

## 7. Operational facts

- Tasks take minutes: most < 20, **hard max 60 minutes**.
- Pricing/effort envelope (Google's published estimates, USD):

| Tier | Est. cost | Searches | Input tokens | Output tokens |
| --- | --- | --- | --- | --- |
| deep-research | $1.00–$3.00 | ~80 | ~250k (50–70% cached) | ~60k |
| deep-research-max | $3.00–$7.00 | ~160 | ~900k (50–70% cached) | ~80k |

  Google labels these "estimates based on preview rates and subject to change".
  Chiron uses the envelope midpoints as its pre-run planning figures only
  (the `--budget` gate); a finished run is priced from its reported `usage`.

- **List rates** (`last_verified: 2026-08-06` against
  https://ai.google.dev/gemini-api/docs/pricing). The agents have no rate card of
  their own — "all model inference is charged at standard Gemini list rates,
  including input, output, and intermediate input / reasoning tokens generated
  during agentic loops", plus tools at their own rates. The applicable standard
  card is `gemini-3.1-pro-preview` (the same model §3 names for follow-ups), paid
  tier, per 1M tokens:

| Component | ≤ 200k prompt | > 200k prompt |
| --- | --- | --- |
| Input | $2.00 | $4.00 |
| Cached input | $0.20 | $0.40 |
| Output (including thinking) | $12.00 | $18.00 |
| Grounding with Google Search | \$14 per 1,000 requests (after 5,000 free/month, shared across Gemini 3.x) | — |

  The two columns are selected **per request**, which a client cannot reconstruct
  from a run's `usage` totals; pricing the totals at the standard column
  reproduces both per-tier envelopes above, pricing them at the higher column does
  not. `usage.total_cached_tokens` is a subset of `total_input_tokens`, not an
  addition to it.

- Collaborative planning (3 steps): create with `collaborative_planning: true` →
  returns a plan; refine by creating again with `previous_interaction_id` +
  `collaborative_planning: true`; approve by creating with `previous_interaction_id`
  + `collaborative_planning: false`. All with `background: true`.
- Because `store: true`, a crashed client re-attaches by interaction `id` — no local
  state needed.

## 8. Drift from the proposal (2026-04-28 → 2026-05-20 revision)

| docs/PROPOSAL.md §3 said | The API now says |
| --- | --- |
| `result.outputs[]`, report = last `text` output | `steps[]`; report = `text` content of last `model_output` step |
| SSE events `interaction.start`, `content.delta`, `interaction.complete` | `interaction.created`, `step.delta`, `interaction.status_update`, `interaction.completed` |
| delta types `thought_summary \| text \| image` | `thought_summary_delta \| text \| image \| ...` (plus tool call/result deltas) |
| status `in_progress → completed \| failed` | full enum incl. `cancelled`, `incomplete`, `budget_exceeded`, `requires_action` |
| reconnect "with the saved `last_event_id`" (header implied) | resume via `?last_event_id=` query parameter |
| citations as a response-level list | citations are `annotations` on text content parts |

Chiron's internal types model the **new** shapes.
