# Chiron — agent orientation

Chiron is the Equestrianism suite's researcher: a Go CLI that drives a
long-running research agent end-to-end (start → await → retrieve → format
→ emit) and produces a cited Markdown report. It is **research-only**: it
never mutates a workspace, runs no shell, applies no edits.

Read `docs/PROPOSAL.md` before changing anything structural; record
notable decisions in `docs/DECISIONS.md`. For anything touching the
Gemini API, `docs/INTERACTIONS-API.md` is normative and wins over the
proposal. Documentation uses en-GB spelling and no emojis.

## Build and verify

```sh
just build        # go build -o bin/chiron ./cmd/chiron
just test         # go test ./...
just vet          # go vet ./...
just lint         # golangci-lint if installed, else go vet
just ci           # everything CI runs
just proto-lint   # lint + compile-check proto/chiron/v1 (needs buf)
just proto        # generate Go from the proto module (output not committed)
```

CI (`.github/workflows/ci.yml`) runs build, vet, test, and golangci-lint,
with actions pinned to full commit SHAs.

## Per-package map

| Package | Role |
| --- | --- |
| `cmd/chiron` | Entrypoint; `os.Exit(cli.Execute())` and nothing else. |
| `internal/cli` | Cobra command tree (`research`, `research-config`, `get`, `follow-up`), flag→config resolution, and the composition root: the only place environment is read, seams are bound, and exit codes (0–4) are assigned. |
| `internal/config` | `ResearchConfig`: the single declarative config. JSON/YAML, flag binding, base+overlay merge semantics for pipelines, validation (enums, timeout cap, secret:// rule, MCP URL schemes). |
| `internal/types` | Seam-level domain types: `Interaction`, `Output`, `Citation`, `Usage`, `Report`, `RunResult`. Wire schema lives in `internal/interactions`; `researcher/gemini` maps wire→domain (see DECISIONS.md, "Wire types vs domain types"). |
| `internal/interactions` | Hand-rolled Interactions API client: wire types (`last_verified` marker), create/get with retries and capped backoff, bounded reads everywhere, cross-host redirect refusal, poll-to-terminal, SSE streaming primitive with `?last_event_id=` resume. |
| `internal/run` | The pure-function research core: `Run` (start→await→retrieve→format→emit) and `Resume`. Depends only on the seam interfaces; takes a context and `Deps`, reads no environment. |
| `internal/researcher` | `Researcher` seam (Start/Await/Result) — the only model-bearing component. |
| `internal/researcher/gemini` | The Deep Research adapter: tier mapping and cost table, input grounding, prompt template, streaming await with reconnect and poll fallback, planner binding, follow-up mode. Creates are never auto-retried (money). |
| `internal/researcher/fleet` | v2 in-process research agents. `RunWorker` is the bounded search→fetch→final loop on one standard model (turn, token, GBP-ceiling and wall-clock caps; tool failures fed back to the model with a three-strike bound; citations restricted to fetched URLs); `Worker` wraps it as a `Researcher` with opaque `wkr_` ids that `get`/`follow-up` refuse. `--agent fleet` (lead over several workers) still returns `ErrNotImplemented`. |
| `internal/researcher/fleet/search` | Hand-rolled Streamable-HTTP MCP client for the web-search tool: `initialize` handshake, `tools/call` with bounded JSON or SSE responses, key header-only and scrubbed. Ships an exported fake server for downstream tests. `Options.Endpoint`/key ref are security-sensitive for the same reason as the model client. |
| `internal/researcher/fleet/fetch` | The `web_fetch` client: SSRF-guarded (private, loopback, link-local, CGNAT, NAT64/6to4, metadata ranges refused at dial time with pinned IPs; no proxy; redirects re-validated), bounded reads with truncation reported, `AllowLoopback` for tests only (see `CHIRON_FETCH_ALLOW_LOOPBACK`). |
| `internal/researcher/fleet/model` | v2 standard-model adapter: hand-rolled `net/http` client for one OpenAI-compatible Chat Completions model (text + provider-native structured output), shared by the lead and workers. The paid POST is never auto-retried (money); the key is header-only and scrubbed from diagnostics. Ships an exported `FakeServer` for downstream tests. `Options.ModelEndpoint`/`ModelKeyRef` (via config) are security-sensitive — credentials travel to the configured endpoint. |
| `internal/planner` | `Planner` seam (Propose/Refine) + the interactive plan-review `Session` for `--plan`; renders on stderr, bounded at `DefaultMaxRounds`. |
| `internal/formatter` | `Formatter` seam: `Interaction` → Markdown `Report` (front matter, body, charts as assets, numbered sources). Pure — no IO; golden-file tested. |
| `internal/sink` | `ReportSink` seam: stdout-markdown, file (0600, writes assets), stdout-json, multi. |
| `internal/transport` | `Transport` seam: run events out of the core. `Stdio` NDJSON on stderr (v1); `grpc` stub for v2. Event kinds mirror `proto/chiron/v1` one-to-one. |
| `internal/trace` | `Tracer` seam with three bindings: OTel (OTLP/HTTP), JSONL (local debug), Noop. `names.go` fixes the span/metric vocabulary; all payloads scrubbed. |
| `internal/secret` | `secret://` resolver (env, file backends), the credential-pattern `Scrub` primitive, and the scrubbing slog handler. Literal keys never appear in config, logs, traces, or stderr. |
| `internal/memory` | `ContextStore` seam + `Noop` only — deliberately unimplemented (PROPOSAL §5); fulfilled externally by Paddock in v2. Declared locally, never imported from paddockapi (see DECISIONS.md). |
| `proto/chiron/v1` | The v2 control-plane contract as a Buf module. Generated Go is deliberately not committed in v1 (see DECISIONS.md). |

## Ground rules

- The run core must depend only on the seam interfaces; concrete types
  are injected from `ResearchConfig` at the CLI composition root.
- Dependency surface is minimal and auditable: stdlib, cobra (+pflag),
  yaml.v3, and the OpenTelemetry SDK (the one justified exception —
  see DECISIONS.md). **No vendor AI SDKs** — adapters are hand-rolled
  `net/http`. Justify any new dependency in `docs/DECISIONS.md` before
  adding it.
- Secrets are `secret://` references end to end; every output path
  (logs, traces, stderr, thought deltas) routes through `secret.Scrub`.
- stdout belongs to the report; events, prompts, and diagnostics go to
  stderr.
- Keep commits in logical units; explain rationale in the message body.

## Money safety

Research tasks cost £1–7 each, so spend paths have hard rules:

- Nothing creates an interaction before the `--budget` gate has run;
  the gate precedes the plan phase too, because plan rounds also spend.
- `POST /interactions` is never auto-retried by the spending callers
  (gemini adapter and planner binding): an ambiguous 5xx may already
  have started billing. GETs retry freely.
- The interaction id is emitted the moment `Start` returns, before
  anything else can fail, so a crashed run is always recoverable with
  `chiron get <id>`.
- A non-terminal stdin can never approve `--plan` spend; only a real
  terminal or an explicit `--accept-plan`.
- A broken event stream must not abort a paid run: transport emission
  is best effort, and a failed SSE await falls back to polling rather
  than abandoning a running task.

## Test infrastructure conventions

- Tests never hit the real network: fakes are `httptest.Server`
  handlers, routed to the client via `CHIRON_GEMINI_BASE_URL` (CLI
  e2e) or `Options.BaseURL`/`WithBaseURL` (package tests).
- Create `httptest.Server` at the call site with `defer server.Close()`;
  do not hide server lifecycle inside helper functions — the call site
  owns and varies the handler.
  The exported `model.FakeServer` and `search.FakeServer` are the one
  carve-out: they are scripted protocol doubles shared across packages,
  still created and closed at the call site (see DECISIONS.md,
  2026-07-01 standard-model adapter entry, for why they live beside the
  clients rather than in test-support packages).
- For new SSE tests in any package, use an `sseWrite`-style helper
  (`t.Helper()`; calls `http.Flusher.Flush()` after writing) rather
  than raw `io.WriteString` literals, so a handler that keeps the
  connection open cannot leave the scanner blocked.
- Table-test loop variables are named `tt`.

## Security-sensitive environment variables

- `CHIRON_GEMINI_BASE_URL` — overrides the Gemini API endpoint. The API
  key is sent in a header on every request to this base, so whoever
  controls the variable receives the key. Its absence is the safe
  default; it exists for the httptest smoke tests and must never be set
  in production. Values are validated at startup: absolute `https://`
  required, `http://` admitted for loopback hosts only. For v2 GKE
  deployments, consider disabling it entirely in release builds via a
  build tag.
- `CHIRON_FETCH_ALLOW_LOOPBACK` — set to exactly `1`, lets the worker's
  `web_fetch` reach loopback hosts so the CLI tests can serve pages from
  httptest. Any other non-empty value is a startup error. It never
  relaxes the private-network, link-local or metadata refusals, and must
  never be set in production; absence is the safe default.
- `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` —
  standard OTel configuration; binding either sends spans carrying
  research queries and interaction ids to the named collector. The
  scrubber guarantees the API key never transits a span, but the queries
  themselves are visible to whatever endpoint is configured: treat the
  collector address as deployment configuration, not a user-settable
  knob. In GKE v2, restrict both to operator-supplied Secrets.

## Security-sensitive configuration (in-process research agents)

The `--agent worker`/`fleet` paths add a `fleet` config block
(`internal/config` `FleetConfig`) whose endpoint and key fields are as
security-sensitive as `CHIRON_GEMINI_BASE_URL`, for the same reason: the
standard-model and search-MCP keys travel to whatever endpoint the config
names, so whoever controls those fields receives the credentials.

- `fleet.model_endpoint` / `fleet.search_endpoint` — validated at
  `ResearchConfig.Validate` with the `CHIRON_GEMINI_BASE_URL` rule:
  absolute `https://`, `http://` for loopback hosts only, so a cleartext
  or internal-network endpoint can never receive a key. The check is a
  mirror of `internal/cli`'s `allowedBaseScheme` (the reverse import would
  cycle); keep the two in step.
- `fleet.model_key_ref` / `fleet.search_key_ref` — must be `secret://`
  references; literals are rejected and never echoed in the error.
- `fleet.model_name` is required for `worker`/`fleet`; the model client
  never sends an empty identifier.
- Spend caps: `fleet.max_turns`, `fleet.max_tokens` and
  `fleet.worker_timeout` bound a run deterministically. `fleet.ceiling_gbp`
  is honoured only when `fleet.price_input_gbp_per_mtok` /
  `fleet.price_output_gbp_per_mtok` are set; a ceiling with no prices is a
  validation error rather than a silently inert cap. `fleet.max_page_bytes`
  bounds each fetched page after HTML-to-text reduction.
- The Gemini-only levers (`budget`, `plan`, `accept_plan`, `model`,
  `visualise`, `tools`, `mcp`, `file_search`, `inputs`, `template`) are
  rejected for `worker`/`fleet` at validation so a caller never believes a
  cap or feature applied when the loop ignores it. `stream` is accepted
  and ignored.

These are config fields, not environment variables — they are per-run
research configuration, not process-wide test hooks. If a later wave adds
a model/search endpoint override *env var*, validate it exactly like
`CHIRON_GEMINI_BASE_URL` and list it in the section above.
