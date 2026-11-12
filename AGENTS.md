# Chiron — agent orientation

Chiron is the Equestrianism suite's researcher: a Go CLI that drives a
long-running research agent end-to-end (start → await → retrieve → format
→ emit) and produces a cited Markdown report. It is **research-only**: it
never mutates a workspace, runs no shell, applies no edits.

Read `docs/PROPOSAL.md` (the v1 design record) before changing anything
structural, and `docs/V2-PLAN.md` and `docs/V2-RESEARCH-AGENT.md` for v2
work; record notable decisions in `docs/DECISIONS.md`. For anything
touching the Gemini API, `docs/INTERACTIONS-API.md` is normative and wins
over the proposal. Documentation uses en-GB spelling and no emojis.

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
| `internal/config` | `ResearchConfig`: the single declarative config. JSON/YAML, flag binding, base+overlay merge semantics for pipelines (`DecodeBase` refuses a base naming a fleet endpoint), validation (enums, timeout cap, secret:// rule, MCP URL schemes). |
| `internal/types` | Seam-level domain types: `Interaction`, `Output`, `Citation`, `Usage`, `Report`, `RunResult`. Wire schema lives in `internal/interactions`; `researcher/gemini` maps wire→domain (see DECISIONS.md, "Wire types vs domain types"). |
| `internal/interactions` | Hand-rolled Interactions API client: wire types (`last_verified` marker), create/get with retries and capped backoff, bounded reads everywhere, cross-host and https-to-http redirect refusal, poll-to-terminal, SSE streaming primitive with `?last_event_id=` resume. |
| `internal/httpx` | Stdlib-only leaf holding the HTTP rules every credential-bearing path shares: `ParseEndpoint` (https anywhere, http for `LoopbackHost` only; no userinfo, query or fragment; errors never echo the value), the `RefuseUnsafeRedirects`/`RefuseAllRedirects` `CheckRedirect` policies, `ReadAllBounded` (fails with `ErrBodyTooLarge`, never truncates), and `InternalAddr`, the table of addresses a fetch of an untrusted URL refuses (shared by the direct fetch's dial-time guard and the Exa fetch's pre-send check). Used by config, the CLI and every client; change a rule here, not in a caller. |
| `internal/run` | The pure-function research core: `Run` (start→await→retrieve→format→emit) and `Resume`. Depends only on the seam interfaces; takes a context and `Deps`, reads no environment. |
| `internal/researcher` | `Researcher` seam (Start/Await/Result) — the only model-bearing component. |
| `internal/researcher/gemini` | The Deep Research adapter: tier mapping and cost table, input grounding, prompt template, streaming await with reconnect and poll fallback, planner binding, follow-up mode. Creates are never auto-retried (money). |
| `internal/researcher/fleet` | v2 in-process research agents. `RunWorker` is the bounded search→fetch→final loop on one standard model, reaching the web only through `WorkerDeps.Search`/`Fetch`, the `web` seam's interfaces, and bounding every provider's titles, URLs, snippets and notes in the transcript (turn, token, GBP-ceiling and wall-clock caps; tool failures fed back to the model with a three-strike bound; citations restricted to URLs it saw and, with a knowledge store, locators it recalled), plus a `recall` action when `WorkerDeps.Knowledge` is set; `Worker` wraps it as a `Researcher` with opaque `wkr_` ids that `get`/`follow-up` refuse, and saves a completed finding back when `WorkerDeps.Remember` is set (docs/KNOWLEDGE.md). The remaining files implement `--agent fleet`, a lead over several workers. `fleet.go`: `Fleet`, the `Researcher` with opaque `flt_` ids that `get`/`follow-up` refuse; one `ContextStore` session per run; the lead runs detached from the caller's context, and `Await` cancels and waits out a bounded unwind. `lead.go`: one strict structured-output decompose call, never retried, into 3–5 briefs, persisted as the run's plan. `router.go`: the brief-target table, external web only, shared by the lead's validation and the pool. `pool.go`: runs at most `MaxWorkers` briefs, `Concurrency` at once, each a `RunWorker` under the shared `Caps`, and stores each finding by reference as it finishes. `synthesise.go`: reads the findings back by reference and writes the report in one plain-text call, stitching the findings on failure. `cite.go`: one structured call attributing the report's claims to worker-cited URLs. `conclude.go`: status rules and the run's single `Usage` rollup, with the lead priced from the workers' `Caps`. Fleet workers never save back and never receive the report template. |
| `internal/researcher/fleet/search` | The web-search tool's result-shape layer over `internal/mcpclient`: calls a configurable tool (`fleet.search_tool`/`Options.ToolName`, default `search`) with the query under a configurable argument key (`fleet.search_query_arg`/`Options.QueryArgKey`, default `query`) and maps `{results:[{title,url,snippet}]}`, degrading to a prose snippet rather than erroring; `Close` ends its MCP session. `Options.Endpoint` and `Options.APIKey` are security-sensitive for the same reason as the model client. |
| `internal/researcher/fleet/search/searchtest` | `search.Client`'s scripted `FakeServer`, exported for downstream tests. Separate from `search` so `net/http/httptest` does not link into the `chiron` binary. |
| `internal/researcher/fleet/web` | The web seam, stdlib only: `Searcher` (`Search(ctx, Query) (SearchResponse, error)`) and `Fetcher` (`Fetch(ctx, rawURL) (Page, error)`), with `Spend` returned on every call and valid beside an error (zero means nothing billed, `Unknown` means billed by an unreported amount), provider `Notes` for the model, and the `ErrRefusedDestination` and `ErrAccountUnusable` sentinels. `search.Client`, `fetch.Client` and `exa.Client` implement it; the composition root binds one per role. A new provider is a leaf package under `web`, one `fleet.search_provider`/`fleet.fetch_provider` enum value per role it serves, and, when it carries a credential, a provider-scoped endpoint and key pair plus one `config.FleetEndpoints` row. |
| `internal/researcher/fleet/web/exa` | Exa provider for both roles over REST (`POST /search`, `POST /contents`): no default endpoint, key in `x-api-key` only, all redirects refused, bounded reads, errors scrubbed of the key by exact match then `secret.Scrub`. One URL per fetch with exactly one status expected; URLs with userinfo, or naming internal, local, special-use or non-ASCII hosts, refused before sending, fragments dropped; the query and objective redacted from Exa's error messages. Spend from `costDollars.total`; 401/402-credit/403-`TEAM_BLOCKED` match `ErrAccountUnusable`. Never retried except one re-send of an enveloped 503 `SERVICE_OVERLOADED`. Smoke-run bodies are its `testdata` fixtures. |
| `internal/researcher/fleet/web/exa/exatest` | `exa.Client`'s scripted `FakeServer`, exported for downstream tests; challenges a keyless request with a 402 like Exa. Separate from `exa` so `net/http/httptest` does not link into the `chiron` binary. |
| `internal/mcpclient` | Hand-rolled Streamable-HTTP MCP transport shared by search and Billet: one session per `Client`, established by a single-flight `initialize` on first use and ended by `Close` with a best-effort DELETE; the negotiated version must be `2025-06-18` or `2025-03-26` (`ErrUnsupportedProtocolVersion`); `tools/call` with bounded JSON or SSE responses; a `tools/call` refused with a session 404 is re-initialised and re-sent once, nothing else retried; cross-host and https-to-http redirects refused; key and session id header-only and scrubbed. |
| `internal/mcpclient/mcpclienttest` | `mcpclient.Client`'s generic scripted `FakeServer`, exported for downstream tests. Separate from `mcpclient` so `net/http/httptest` does not link into the `chiron` binary. |
| `internal/researcher/fleet/fetch` | The direct `web_fetch` client: SSRF-guarded (private, loopback, link-local, CGNAT, NAT64/6to4, metadata ranges per `httpx.InternalAddr` refused at dial time with pinned IPs; no proxy; redirects re-validated), bounded reads with truncation reported, `AllowLoopback` for tests only (see `CHIRON_FETCH_ALLOW_LOOPBACK`). |
| `internal/researcher/fleet/model` | v2 standard-model adapter: hand-rolled `net/http` client for one OpenAI-compatible Chat Completions model (text + provider-native structured output), shared by the lead and workers. The paid POST is never auto-retried (money); the key is header-only and scrubbed from diagnostics. `Options.Endpoint` and `Options.APIKey` (an already-resolved key; the adapter never resolves `secret://` references) are security-sensitive — the credential travels to the configured endpoint. |
| `internal/researcher/fleet/model/modeltest` | `model.Client`'s scripted `FakeServer`, exported for downstream tests. Separate from `model` so `net/http/httptest` does not link into the `chiron` binary. |
| `internal/planner` | `Planner` seam (Propose/Refine) + the interactive plan-review `Session` for `--plan`; renders on stderr, bounded at `DefaultMaxRounds`. |
| `internal/formatter` | `Formatter` seam: `Interaction` → Markdown `Report` (provenance comment, front matter, body, charts as assets, numbered sources). Pure — no IO; golden-file tested. |
| `internal/version` | The single `Version` var, stamped at build time via `-X` ldflags (see Justfile); `"dev"` otherwise. Read by `--version` and the report's provenance comment, so both report the same build identically. |
| `internal/sink` | `ReportSink` seam: stdout-markdown, file (0600, writes assets), stdout-json, multi. |
| `internal/transport` | `Transport` seam: run events out of the core. `Stdio` NDJSON on stderr (v1); `grpc` stub for v2. Event kinds mirror `proto/chiron/v1` one-to-one. |
| `internal/trace` | `Tracer` seam. The CLI binds OTel (OTLP/HTTP) when an endpoint is configured and Noop otherwise; the JSONL tracer (`NewJSONL`, `NewJSONLFile`) is used by tests and the CLI cannot enable it. `names.go` fixes the span/metric vocabulary, including the reserved fleet span names (`decompose`, `delegate`, `synthesise`, `cite`, `control_plane`) and attribute keys (`worker_id`, `brief_id` on a delegate span; `brief_count` on decompose), pinned by a vocabulary test; all payloads scrubbed. |
| `internal/secret` | `secret://` resolver (env, file backends), the credential-pattern `Scrub` primitive, and the scrubbing slog handler. Literal keys never appear in config, logs, traces, or stderr. |
| `internal/memory` | The agentic-memory seam, declared locally (never imported from any store's module; see DECISIONS.md). `Recaller`/`Rememberer` are long-term memory, bound to external knowledge stores by the subpackages below and consumed by the worker's recall action and save-back. `ContextStore` embeds both plus the session and artifact plane, bound to `Noop` or `InMemory`. `InMemory` is the in-process store the fleet passes its plan and findings through by reference (`fleet.memory: inmemory`, required for `--agent fleet`; the composition root binds one per fleet run and none for any other agent): SHA-256 content-addressed, immutable artifacts in session-scoped namespaces with an absolute TTL, a per-artifact size bound, and copies in and out. Its `Remember`/`Recall` return `ErrNotImplemented`. `Recaller`/`Rememberer` never gain a lifecycle method: an adapter that holds a connection exposes its own `Close() error`, which the composition root returns beside the two halves (as `buildKnowledge` does). That `Close` takes no context because its best-effort network cleanup runs on its own bounded background context, independent of run cancellation; the session plane's `Session.Close(ctx)` takes the caller's context because a session is short-lived and TTL'd. |
| `internal/memory/billet` | Billet adapter over `internal/mcpclient`: `Recall` calls `search_memory` (no degrade-to-prose: a reply without `records` is an error; each record bounded on read), `Remember` calls `save_memory` (content over 256 KiB refused before any request). Locators are `billet://memory/<id>`; `Close` ends its MCP session. Ships the `CHIRON_BILLET_BIN` interop test. |
| `internal/memory/billet/billettest` | `billet.Client`'s scripted `FakeServer`, exported for downstream tests. Separate from `billet` so `net/http/httptest` does not link into the `chiron` binary. |
| `internal/memory/alexandria` | Alexandria adapter over REST `GET /v1/search` (the path its own plugin uses): bearer token, optional Cloudflare Access headers, space slug validated, redirects never followed, body bounded, `degraded` surfaced as a label. `Recaller` only. |
| `internal/memory/alexandria/alexandriatest` | `alexandria.Client`'s scripted `FakeServer`, exported for downstream tests. Separate from `alexandria` so `net/http/httptest` does not link into the `chiron` binary. |
| `proto/chiron/v1` | The v2 control-plane contract as a Buf module. Generated Go is deliberately not committed in v1 (see DECISIONS.md). |

## Ground rules

- The run core must depend only on the seam interfaces; concrete types
  are injected from `ResearchConfig` at the CLI composition root.
- Dependency surface is minimal and auditable: stdlib, cobra (+pflag),
  yaml.v3, and the OpenTelemetry SDK (the one justified exception —
  see DECISIONS.md), plus testify, which only tests import.
  **No vendor AI SDKs** — adapters are hand-rolled
  `net/http`. Justify any new dependency in `docs/DECISIONS.md` before
  adding it.
- Secrets are `secret://` references end to end; every output path
  (logs, traces, stderr, thought deltas) routes through `secret.Scrub`.
- stdout belongs to the report; events, prompts, and diagnostics go to
  stderr.
- Keep commits in logical units; explain rationale in the message body.

## Money safety for the Gemini path

This section covers `deep-research` and `deep-research-max`, and the
`get`, `follow-up` and `--plan` paths that share them. Research tasks
cost £1–7 each, so spend paths have hard rules:

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

## Money safety for the in-process agents

`--agent worker` and `--agent fleet` create no Gemini interaction, so the
`--budget` gate and the plan phase do not apply to them: `budget` and the
other deep-research levers are rejected at validation
(`TestConfig_Validate_RejectsDeepResearchLeversForInProcessAgents`). A
`wkr_` or `flt_` id is a local label, not a resume handle: `get` and
`follow-up` refuse it (`TestGetAndFollowUp_RefuseWorkerIDs`,
`TestFleetID_GetAndFollowUp_Refused`). The rules that do apply, as
implemented today:

- No paid model call is retried. A worker turn is a single attempt
  (`TestRunWorkerNoRetryOnPaidTurn`, `TestNoRetryOn5xx` in the model
  client); so are the lead's decomposition (`TestDecompose_ModelErrorIsNotRetried`),
  synthesis (`TestSynthesise_FailureFallsBackToFindings`) and citation
  call (`TestCite_FallsBackToEveryWorkerCitation`), the last two falling
  back to the stitched findings and every worker citation.
- Each worker runs under a turn cap (`fleet.max_turns`, default 8) and a
  wall-clock timeout (`fleet.worker_timeout`, default five minutes), both
  always on. The token cap (`fleet.max_tokens`, default 400,000) is off
  when set to 0. The GBP ceiling (`fleet.ceiling_gbp`) is off by default,
  counts model tokens only, and needs a price: validation rejects a
  ceiling when both prices are 0. The turn, token and GBP caps are
  checked before each turn, so a run can pass the token or GBP cap by up
  to one turn; the timeout also cancels a call in flight. Reaching any
  cap ends the worker `incomplete`
  (`TestRunWorkerCapsStopDeterministically`, `TestRunWorkerTokenCapStops`,
  `TestRunWorker_CostCeilingStops`,
  `TestRunWorker_TimeoutMidTurnIsIncomplete`). The fleet's pool gives
  every worker the same caps (`TestPool_Run_AppliesPerWorkerCaps`).
- Fan-out is bounded by `fleet.max_workers` and `fleet.concurrency`
  (`TestPool_Run_BoundsConcurrencyAndWorkerCount`).
- Web-provider spend is reported in USD, with a count of calls of unknown
  cost (`TestWorker_Run_AccumulatesWebSpend`, `TestFleet_RollsUpWebSpend`,
  `TestRun_RecordsWebCost`). It is never added to the GBP figures.
- A provider account that refuses every call ends the worker at once and
  starts none of a fleet's queued workers
  (`TestWorker_Run_EndsAtOnceWhenAccountUnusable`,
  `TestPool_Run_StopsStartingWorkersOnUnusableAccount`).
- Findings pass between workers and the lead by reference through the
  in-process store, not by copy (`TestPool_Run_ReturnsStoredFindingReferences`).
- There is no run-level GBP ceiling across a fleet: the worst case is
  `fleet.max_workers` times one worker's caps, plus the lead's
  decompose, synthesise and cite calls, which no ceiling counts and
  which are bounded only by a 16,384-token completion cap each
  (`docs/V2-PLAN.md`, decision D6).

## Test infrastructure conventions

- Tests never hit the real network: fakes are `httptest.Server`
  handlers, routed to the client via `CHIRON_GEMINI_BASE_URL` (CLI
  e2e) or `Options.BaseURL`/`WithBaseURL` (package tests).
- Create `httptest.Server` at the call site with `defer server.Close()`;
  do not hide server lifecycle inside helper functions — the call site
  owns and varies the handler.
  The exported `modeltest.FakeServer`, `searchtest.FakeServer`,
  `mcpclienttest.FakeServer`, `billettest.FakeServer`,
  `alexandriatest.FakeServer` and `exatest.FakeServer` are the one
  carve-out: they are scripted
  protocol doubles shared across packages, still created and closed at
  the call site, but each lives in its own `<pkg>test` sibling package
  rather than beside its client, so `net/http/httptest` never links into
  the `chiron` binary (see DECISIONS.md, 2026-09-25 entry).
  `billettest.FakeServer` records what a save-back sent
  (`SavedContents()`); `alexandriatest.FakeServer` records the query
  string, bearer and Access headers; `exatest.FakeServer` records each
  request body and its `x-api-key`.
- The fleet package tests the worker against in-test function adapters for
  `memory.Recaller`/`memory.Rememberer`, never an adapter package, and
  against in-test `web.Searcher`/`web.Fetcher` adapters where a test needs
  spend or a failure no fake scripts.
- Two tests talk to a real server, both skipped by default. `TestLiveBillet`
  runs only when `CHIRON_BILLET_BIN` names a `billet` binary, which it starts
  on a free loopback port. `TestLiveExa` runs only when `CHIRON_EXA_LIVE_KEY`
  holds an Exa API key; it makes three metered calls (about one cent) against
  `api.exa.ai` and never prints the key.
- For new SSE tests in any package, use an `sseWrite`-style helper
  (`t.Helper()`; calls `http.Flusher.Flush()` after writing) rather
  than raw `io.WriteString` literals, so a handler that keeps the
  connection open cannot leave the scanner blocked.
- Table-test loop variables are named `tt`.

## Writing tests

The conventions match Stirrup's, so tests read the same in both
repositories (see DECISIONS.md, 2026-11-12 entry).

- **Assert with testify through a per-scope object.** Every function or
  closure that asserts starts with `must := require.New(t)` (and
  `check := assert.New(t)` where a soft assertion is wanted), then asserts
  as `must.Equal(want, got)`. The object captures the `t` it was built
  from, so each `t.Run` closure builds its own `must` from the subtest's
  `t`; reusing the parent's object inside a subtest fails the parent
  instead, and no linter catches it. The names `must` and `check` are
  fixed across the suite. Pick the most specific form (`NoError`,
  `ErrorIs`, `ErrorContains`, `Len`, `Empty`, `Contains`, `ElementsMatch`,
  `EqualValues`, `JSONEq`, `WithinDuration`) rather than `True` over a
  hand-written comparison. Hand-rolled `if got != want { t.Fatalf(...) }`
  blocks and `reflect.DeepEqual` are not accepted in new tests.
  `testifylint` runs under `just lint` and rejects swapped arguments,
  `Equal` on errors and other common misuse. It also rejects `Equal` on
  floats, so an exact float comparison (spend, prices, metric values) is
  `InDelta(want, got, 0)`; a non-zero delta is a deliberate tolerance.
- **`require` stops the test; keep it on the test goroutine.** Inside
  `go func` bodies, `httptest` handlers and any other callback that may
  run off the test goroutine, use `check` or hand the failure back over a
  channel; `must` there cannot stop the test correctly. Inside
  `must.EventuallyWithT` the condition asserts through the `CollectT` it
  receives (`require.NoError(c, err)`), never through the outer `must`.
- **Name each test so it reads on its own.** Test names follow
  `Test<Subject>_<Behaviour>[_<Detail>]`, for example
  `TestConfig_Validate_RejectsUnknownAgent`. JUnit output loses the
  package, so the name has to carry the subject. Subtest names are short
  lowercase phrases. Names referenced from documentation or CI, such as
  `TestLiveBillet` and `TestLiveExa`, are kept.
- **Prefer subtests over loops with `t.Errorf`.** A table-driven test runs
  each case under `t.Run` with the case's name and uses the subtest's own
  `t` and its own `must`, so the first failure names the case and stops
  there.
- **Helpers build fixtures; assertions stay in the test.** A helper that
  only wraps an assertion hides what the test checks. Keep helpers for
  setup and teardown, mark them with `t.Helper()`, and use `t.TempDir`,
  `t.Setenv` and `t.Cleanup` instead of manual equivalents.
- **Comments describe the behaviour protected**, not the incident, pull
  request or session that motivated the test.

## Security-sensitive environment variables

- `CHIRON_GEMINI_BASE_URL` — overrides the Gemini API endpoint. The API
  key is sent in a header on every request to this base, so whoever
  controls the variable receives the key. Its absence is the safe
  default; it exists for the httptest smoke tests and must never be set
  in production. Values are validated at startup: absolute `https://`
  required, `http://` admitted for loopback hosts only, and userinfo, a
  query or a fragment refused (`internal/httpx`).
- `CHIRON_FETCH_ALLOW_LOOPBACK` — set to exactly `1`, lets the worker's
  direct `web_fetch` reach loopback hosts so the CLI tests can serve pages
  from httptest. It is read only when `fleet.fetch_provider` is `direct`.
  Any other non-empty value is a startup error. It never
  relaxes the private-network, link-local or metadata refusals, and must
  never be set in production; absence is the safe default.
- `CHIRON_FLEET_MODEL_ENDPOINT` / `CHIRON_FLEET_SEARCH_ENDPOINT` /
  `CHIRON_FLEET_KNOWLEDGE_ENDPOINT` / `CHIRON_FLEET_EXA_ENDPOINT` — supply
  the worker's standard-model, search-MCP, knowledge store and Exa
  endpoints when the matching `--fleet-*-endpoint` flag is unset; a set
  flag wins. The matching key is
  sent in a header on every request to that endpoint, so whoever controls
  the variable receives the key. A base config can never name these
  endpoints, so the flag and the variable are their only sources. Values
  are validated at startup: absolute `https://` required, `http://`
  admitted for loopback hosts only, and userinfo, a query or a fragment
  refused (`internal/httpx`); the error names the variable and at most the
  value's scheme and host, never its path, query or credentials. Read only
  at the composition root on the worker path and only when the run uses
  the endpoint: the search variable with MCP search, the knowledge variable
  with a knowledge provider, the Exa variable when either web role uses
  Exa. Treat them as operator-supplied deployment configuration, never a
  user-settable knob.
- `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` —
  standard OTel configuration; binding either sends spans carrying
  research queries and interaction ids to the named collector. The
  scrubber guarantees the API key never transits a span, but the queries
  themselves are visible to whatever endpoint is configured: treat the
  collector address as deployment configuration, not a user-settable
  knob. An explicit `--otlp-endpoint` flag or the Langfuse key pair
  (below) takes precedence over these variables. A malformed value is
  refused on every path before the OTel SDK parses it (see below).
- `OTEL_EXPORTER_OTLP_HEADERS` / `OTEL_EXPORTER_OTLP_TRACES_HEADERS` —
  request headers, usually a credential, for the collector the variables
  above name, and they go only to that collector: a run with
  `--otlp-endpoint` refuses to start while either is set, and the Langfuse
  key pair replaces them. An authenticated generic collector is therefore
  configured entirely through the environment, endpoint and headers
  together. A malformed list is refused on every path before the OTel SDK
  parses it, since the SDK's own logger would print it raw to stderr; the
  error names the variable, never its value. The endpoint variables above
  get the same refusal, for the same reason: the SDK's logger prints an
  unparseable endpoint raw too, userinfo included.

## Security-sensitive configuration (in-process research agents)

The `--agent worker`/`fleet` paths add a `fleet` config block
(`internal/config` `FleetConfig`) whose endpoint and key fields are as
security-sensitive as `CHIRON_GEMINI_BASE_URL`, for the same reason: the
standard-model, search-MCP, knowledge store and Exa keys travel to
whatever endpoint is named, so whoever controls those fields receives the
credentials. A base config may name the key references but never an
endpoint.

- `fleet.model_endpoint` / `fleet.search_endpoint` — set only by
  `--fleet-model-endpoint` / `--fleet-search-endpoint` or the
  `CHIRON_FLEET_*_ENDPOINT` variables above. `config.DecodeBase` refuses
  a base config (`--config`, `-` or piped stdin) naming any fleet
  endpoint before any flag is applied or secret resolved, and
  `research-config` refuses the endpoint flags, so a pipeline never emits
  one. Validated with the `CHIRON_GEMINI_BASE_URL` rule:
  absolute `https://`, `http://` for loopback hosts only, so a cleartext
  or internal-network endpoint can never receive a key; userinfo, a query
  and a fragment are refused too. Config, the CLI and each client's
  constructor all apply `internal/httpx`'s `ParseEndpoint`, so the rule
  has one definition. Before the first model call the worker path emits
  the endpoint hosts (scheme and host only) as one `delta` event.
- `fleet.model_key_ref` / `fleet.search_key_ref` — must be `secret://`
  references; literals are rejected and never echoed in the error.
- `fleet.knowledge_endpoint` / `fleet.knowledge_key_ref` — the knowledge
  store pair, validated exactly like the search pair. The composition root
  requires the endpoint when `fleet.knowledge_provider` is set and the key
  for `alexandria`. With no provider every other `fleet.knowledge_*` field
  must be empty or default. `fleet.knowledge_remember` (Billet and
  `--agent worker` only; rejected for `fleet`) sends
  each completed finding, built from public-web content, to the store; it
  is off by default and named on the Interaction's tool list. The endpoint
  follows the same provenance rule as the model and search endpoints.
- `fleet.search_provider` / `fleet.fetch_provider` — who serves each web
  role: `mcp` (the default; `fleet.search_endpoint`) or `exa` for search,
  `direct` (the default; Chiron's SSRF-guarded client) or `exa` for fetch.
  Selecting `exa` sends every search query, and every fetched URL with its
  query string, to Exa, and pages are then fetched by Exa's `ExaUser`
  agent rather than Chiron's own; the tool list records `web_search:exa`
  and `web_fetch:exa`. With `exa` search, the MCP search fields must be
  unset or default.
- `fleet.exa_endpoint` / `fleet.exa_key_ref` — the Exa pair, scoped to the
  provider so the key is bound by name to its destination, and validated
  like the model pair. There is no default endpoint: the composition root
  requires `--fleet-exa-endpoint https://api.exa.ai` or
  `CHIRON_FLEET_EXA_ENDPOINT` when either role uses Exa, and the pair must
  be unset otherwise. The endpoint follows the same provenance rule as the
  model endpoint, but the provider selection and key reference may come
  from a base config: with the variable exported, a base config can route
  queries and fetched URLs to Exa and choose which secret is sent there, as
  it can for the knowledge pair. Export the variable per invocation where
  that matters.
- `fleet.search_send_objective` — off by default; when set, each Exa
  search also carries the research question (cut to 4,000 UTF-16 units),
  which otherwise never leaves the process. Rejected unless search is
  `exa`. Only `--fleet-search-send-objective` can set it: `DecodeBase`
  refuses it in a base config and `research-config` refuses the flag.
- `fleet.model_name` is required for `worker`/`fleet`; the model client
  never sends an empty identifier.
- Spend caps: `fleet.max_turns`, `fleet.max_tokens` and
  `fleet.worker_timeout` bound a run deterministically. `fleet.ceiling_gbp`
  is honoured only when `fleet.price_input_gbp_per_mtok` /
  `fleet.price_output_gbp_per_mtok` are set; a ceiling with no prices is a
  validation error rather than a silently inert cap; it covers model tokens
  only. Web-provider spend is reported in USD (`web_cost_usd`, with
  `web_cost_unknown_calls` counting calls of unreported cost) and has no
  ceiling: it is bounded structurally at one web call per turn, so at most
  `max_turns × $0.007` of Exa searches plus `$0.001` per fetched page per
  worker, times `max_workers` in a fleet. A provider account that refuses
  every call (no credit, rejected key) ends the worker at once, and a
  fleet then starts none of its queued workers.
  `fleet.max_page_bytes` bounds each fetched page after HTML-to-text
  reduction; with Exa fetch it is also Exa's character bound, at most
  1,000,000.
- The Gemini-only levers (`budget`, `plan`, `accept_plan`, `model`,
  `visualise`, `tools`, `mcp`, `file_search`, `inputs`) are
  rejected for `worker`/`fleet` at validation so a caller never believes a
  cap or feature applied when the loop ignores it. `stream` is accepted:
  for `worker`/`fleet` it gates whether per-turn `worker_turn` progress
  events reach the transport (`--quiet` disables them), but it carries no
  spend meaning there. `template` is honoured: it replaces the worker's
  output-format block, or for `fleet` the lead's synthesis format
  (`fleet.LoadReportTemplate`, bounded at 8 KiB).

These are config fields — per-run research configuration, not
process-wide test hooks — except the endpoints, which come per run from a
flag or per deployment from an environment variable, never from a base
config (docs/DECISIONS.md, 2026-09-25 "Fleet endpoints come only from
flags or the environment").

The `telemetry` config block (`internal/config` `TelemetryConfig`) is
separate from `fleet` and validated for every agent, not only
`worker`/`fleet`:

- `telemetry.otlp_endpoint` / `telemetry.langfuse_endpoint` — validated
  with the same rule as `CHIRON_GEMINI_BASE_URL`. The collector or
  Langfuse endpoint receives every span, including research queries and
  the run's spend. These fields are settable per run from flags and from
  a base config (`--config` or piped stdin). The rule that keeps a config
  file from choosing both a credential and its destination, which holds
  for the fleet endpoints, is not yet extended to these fields (tracked
  as #47), so base configs are trusted input for `telemetry.*`, for every
  agent and not only `worker`/`fleet`. `telemetry.langfuse_endpoint` needs
  both key refs below; with neither endpoint set, the standard
  `OTEL_EXPORTER_OTLP_*` variables still apply, but an explicit endpoint
  here wins over their endpoint. `telemetry.otlp_endpoint` is for
  collectors that need no request headers and is refused while an OTLP
  header variable is set (see above). Whenever these fields or the
  Langfuse key refs choose the destination, the exporter refuses
  redirects, and the `OTEL_EXPORTER_OTLP_*` certificate, client-certificate
  and timeout variables do not configure the connection (an `http://`
  loopback endpoint is refused while a certificate variable is set).
- `telemetry.langfuse_public_key_ref` / `telemetry.langfuse_secret_key_ref`
  — `secret://` references, required together, literals rejected without
  being echoed. Resolved once at the composition root and handed to the
  exporter only, as an `Authorization: Basic` header; nothing else reads
  them. `internal/secret.Scrub` recognises the `pk-lf-`/`sk-lf-` key
  shape and Basic credentials on every output path Chiron writes (spans,
  stderr, logs). The OTel SDK's own logger bypasses Scrub; its
  credential-bearing messages, a malformed OTLP header or endpoint
  variable, are prevented by refusing the malformed value before the SDK
  parses it.
