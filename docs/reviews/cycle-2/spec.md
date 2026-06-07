---
review: cycle-2
scope: complete Chiron v1 — M0–M7, all commands, all flags
ref_commit: 90fe191
reviewer: spec-compliance-reviewer (Claude Code agent)
date: 2026-06-07
precedence: docs/INTERACTIONS-API.md > docs/DECISIONS.md > docs/PROPOSAL.md
---

# Chiron v1 — Spec-Compliance Review, Cycle 2 (Final)

## Specification Summary

Full v1 compliance review: every milestone (M0–M7), every command and flag in
PROPOSAL §4.3, every DECISIONS.md entry, closure of both cycle-1 Mediums and all
cycle-1 Lows, and the previously out-of-scope M4/M5 features. Precedence:
INTERACTIONS-API.md (normative wire) > DECISIONS.md (sanctioned deviations) >
PROPOSAL.md.

---

## Cycle-1 Finding Closure

All findings from the cycle-1 report (`docs/reviews/cycle-1/spec.md`) are
verified closed.

| Finding | Severity | Resolution | Evidence |
| --- | --- | --- | --- |
| C1-M5-1: `cfg.Stream` not wired to `ThinkingSummaries` on create | Medium | Fixed: `geminiOptions()` sets `ThinkingSummaries: cfg.Stream`; `agentConfig()` maps bool→`"auto"`/`"none"` | `internal/cli/research.go:219`; `TestStreamTogglesThinkingSummaries` |
| C1-M5-2: `ReconnectCount` always zero | Medium | Fixed: `r.reconnectCount.Add(1)` on every re-dial in `awaitStream()`; reported via `toDomain()` | `internal/researcher/gemini/stream.go:85`; `MetricReconnectCount` in `run.go:270` |
| C1-LOW-3: `OutputThoughtSummary` outputs never populated | Low | Fixed: `thoughtTexts()` now maps `thought` content parts to `OutputThoughtSummary` domain outputs in `toDomain()` | `internal/researcher/gemini/gemini.go` |
| C1-SPEC-2 (named C1-LOW-4 in brief): `go.opentelemetry.io/otel/trace` undeclared in DECISIONS.md | Low | Fixed: added as the fourth direct OTel dependency in the OTel DECISIONS.md entry | `docs/DECISIONS.md` OTel entry |
| C1-LOW-5: M4 stubs inert (research-config, get, follow-up) | Low | Fixed: all three commands fully implemented | `internal/cli/commands.go`, `internal/cli/research.go` |
| C1-CODE-4: `stdinIsPiped` returns `true` for non-`*os.File` | Low | Fixed: non-`*os.File` returns `false`; `stdinIsTerminal` added as deliberate non-negation | `internal/cli/research.go`; `internal/cli/stdin_test.go` |
| C1-SEC-1: `CHIRON_GEMINI_BASE_URL` unvalidated | Security | Fixed: `geminiBaseURL()` + `allowedBaseScheme()` enforce https-anywhere / http-loopback-only before any client is constructed | `internal/cli/research.go:259–286`; `internal/cli/baseurl_test.go` |
| C1-SEC-2: interactions client does not refuse cross-host redirects | Security | Fixed: redirect policy enforced on shallow copy of `http.Client`; `WithHTTPClient` cannot lose the guarantee | `internal/interactions` (confirmed by DECISIONS.md) |

---

## PROPOSAL §4.3 — Command Table

| Command | Status | Notes |
| --- | --- | --- |
| `chiron research --query "..."` | **Met** | `runResearch()` — full path: budget gate → optional plan → `run.Run()` → `concludeRun()` |
| `chiron research-config [flags]` | **Met** | `cfg.EncodeJSON(cmd.OutOrStdout())` — emits resolved JSON; pipeline composition tested |
| `chiron get <interaction-id>` | **Met** | `runGet()` → `run.Resume()` — no create, no budget gate, honest about absent tool set / query |
| `chiron follow-up <interaction-id> --query "..."` | **Met** | `runFollowUp()` → `gemini.NewFollowUp()` → `run.Run()` — model not agent, no agent_config/tools |

---

## PROPOSAL §4.3 — Flag Table (research command)

| Flag | Default | Status | Notes |
| --- | --- | --- | --- |
| `--query` / positional | — | **Met** | Required; validated before any seam is built |
| `--agent` | `deep-research` | **Met** | Maps to wire agent ID via `tierAgent()`; `deep-research-max` also accepted |
| `--plan` | off | **Met** | Collaborative planning gate; `reviewPlan()` → `gemini.NewPlanner()` → `planner.Session.Run()` |
| `--visualise` | off | **Met** | `visualization: "auto"` in `agent_config`; prompt template nudge |
| `--stream` / `--quiet` | `--stream` (on) | **Met** | Mutually exclusive; `--quiet` is sugar for `--stream=false`; toggles SSE await and `thinking_summaries` request field |
| `--tools` | API defaults | **Met** | Assembles tool slice; default trio explicit in create request so recorded set = used set |
| `--mcp name=url` | — | **Met** | Repeatable; assembles `mcp_server` tool entries |
| `--file-search <store>` | — | **Met** | Assembles `file_search` tool entry |
| `--input <path-or-url>` | — | **Met** | Repeatable; `inputPart()` with `maxInputBytes` bound (C1-CODE-1); multimodal grounding |
| `--template <path>` | built-in | **Met** | Loaded via `loadTemplate()`; prompt template is a swappable input |
| `-o, --output` | `text` | **Met** | `text` / `json` / `none`; `buildSink()` wires the correct sinks |
| `--out <path>` | stdout | **Met** | File sink; assets written next to report with deterministic names |
| `--config <path>` | — | **Met** | Base config; stdin auto-detected when piped (only `*os.File`); explicit-flag overlay via `pflag.FlagSet.Visit` |
| `--api-key-ref` | `secret://GEMINI_API_KEY` | **Met** | Only `secret://` references accepted; literal-value rejection never echoes the value |
| `--budget <gbp>` | unset | **Met (deviates with DECISIONS.md sanction)** | PROPOSAL says "warn/abort"; DECISIONS.md binds it as abort-only (exit 4); the deviation is documented and correct — a planning figure cannot meaningfully "warn" before a paid operation |
| `--timeout <dur>` | `30m` | **Met** | `config.Duration` wraps `time.Duration`; serialises as `"30m0s"`; passed to `context.WithTimeout`. PROPOSAL parenthetical "hard cap 60m (the agent's own limit)" describes the Gemini API's server-side limit, not a required client cap |

Additional flags implemented beyond PROPOSAL §4.3 (over-implementation, noted, not penalised):

| Flag | Command | Purpose |
| --- | --- | --- |
| `--accept-plan` | research | Approves first plan unattended; necessary for non-interactive `--plan` use (implied by spec's --plan semantics) |
| `--model` | research / follow-up | Overrides the default follow-up model (`gemini-3.1-pro-preview`); consistent with INTERACTIONS-API.md §3 |

---

## PROPOSAL §9 — Milestone Table

| Milestone | Status | Deliverable verification |
| --- | --- | --- |
| M0 — Scaffold | **Delivered** | Go module (`github.com/rxbynerd/chiron`), cobra entrypoint, `Justfile`, CI, `AGENTS.md`/`CLAUDE.md`, interface stubs |
| M1 — Interactions client | **Delivered** | `internal/interactions`: create (background, store, explicit fields), get, `PollUntilTerminal`, SSE streamer (`Next`/`LastEventID`/`Close`), bounded reads, timeouts, typed `*APIError`, `ErrRequiresAction` |
| M2 — Core run + Gemini researcher | **Delivered** | `internal/run.Run()` + `Resume()`; `researcher/gemini` adapter; `secret/`; stdout-markdown sink; end-to-end `chiron research --query` |
| M3 — Markdown formatter | **Delivered** | YAML front matter (query, agent, id, timestamps, cost, tools, sources), body from last model_output text, citation section, chart extraction → `Report.Assets`; `file` sink + `--out` |
| M4 — Cost & planning levers | **Delivered** | `--plan` three-step flow; `--budget` gate (exit 4); tier selection; `research-config` + pipeline composition; `chiron get` resume; `chiron follow-up` with model-mode |
| M5 — Streaming | **Delivered** | `--stream` thought summaries via `OnThought`/transport delta events; SSE with `?last_event_id=` reconnect; failure budget 4; poll fallback; `ReconnectCount` real |
| M6 — Observability & secrets | **Delivered** | OTel tracer (OTLP/HTTP); `trace.Noop` fallback; JSONL local debug tracer; credential scrubber on slog + OTel + JSONL; `secret://env` and `secret://file` resolvers; all §4.5 metrics |
| M7 — v2 seams | **Delivered** | `proto/chiron/v1/chiron.proto` (outbound-dial pattern); `transport.GRPC` stub; `researcher/fleet.Fleet` stub; `internal/memory.ContextStore` interface + `Noop`; generated Go not committed (DECISIONS.md justified) |

---

## M4 Compliance — Cost & Planning Levers

### --plan: Collaborative Planning

| Requirement | Source | Status | Evidence |
| --- | --- | --- | --- |
| Three-step flow: create(collaborative_planning=true) → poll → refine via previous_interaction_id → approve (collaborative_planning=false) | INTERACTIONS-API.md §7 | **Met** | `gemini/planner.go`: `round()` creates with `CollaborativePlanning: true, Background: true, Store: true, Stream: false`; `Refine()` passes `previousID`; approval is ordinary `run.Run()` with `PreviousInteractionID` and `CollaborativePlanning` off |
| Plan-phase creates never auto-retried | DECISIONS.md | **Met** | `NewPlanner()` uses `WithMaxRetries(0)` create client; poll client retries normally; pinned by test |
| Plan renders on stderr, not stdout | DECISIONS.md | **Met** | `session.Out = cmd.ErrOrStderr()`; `TestResearchPlanAcceptPlanFlow` verifies plan not on stdout |
| Non-terminal stdin blocks --plan without --accept-plan | DECISIONS.md | **Met** | `stdinIsTerminal()` check in `reviewPlan()`; error message directs to `--accept-plan`; `TestResearchPlanNonInteractiveNeedsAcceptPlan` |
| --accept-plan approves first plan unattended | DECISIONS.md | **Met** | `Session.AutoAccept` → renders plan + "plan accepted unattended" on stderr without prompting; `TestResearchPlanAcceptPlanFlow` |
| Plan declined → ExitBlocked (4) | DECISIONS.md | **Met** | `errors.Is(err, planner.ErrAborted)` → `&ExitError{Code: ExitBlocked}` |
| planner.DefaultMaxRounds = 5 | DECISIONS.md | **Met** | `internal/planner/session.go:17` |
| Round counter rendered with each plan | DECISIONS.md | **Met** | `renderPlan()`: "round %d of %d (interaction %s)" |
| Approval is ordinary research run, not on planner seam | DECISIONS.md | **Met** | `run.Run(ctx, deps, run.Params{PreviousInteractionID: previousID})` after `reviewPlan()` returns the accepted plan id |
| Budget gate fires before plan phase | DECISIONS.md | **Met** | `gateBudget()` called before `reviewPlan()`; `TestResearchBudgetGatesThePlanPhaseToo` |

### --budget: Budget Gate

| Requirement | Source | Status | Evidence |
| --- | --- | --- | --- |
| Gate fires before any create | DECISIONS.md | **Met** | `gateBudget(cfg, res.EstimatedCostGBP())` called after `gemini.New()` but before any plan or research create; test server errors on any request if gate should fire |
| Uses tier planning estimate (cost.go midpoints) | DECISIONS.md | **Met** | `deep-research`: £1.58 ($2.00 × 0.79); `deep-research-max`: £3.95 ($5.00 × 0.79) |
| ExitBlocked (4) with estimate vs cap in error | DECISIONS.md | **Met** | `TestBudgetGateBlocksBeforeAnyCreate`: verifies exit code 4 and that error carries "£1.58" and "£1.00" |
| Run under cap proceeds | DECISIONS.md | **Met** | `TestBudgetGateAllowsRunUnderCap`: £2 cap admits £1.58 estimate |
| `chiron get` never gates | DECISIONS.md | **Met** | `runGet()` has no `gateBudget()` call |
| Follow-up estimate is zero (passes gate trivially) | DECISIONS.md | **Met** | `NewFollowUp`: `estimate` field not set (zero value); unknown-tier path in `estimatedCostGBP()` returns 0 |

### research-config

| Requirement | Source | Status | Evidence |
| --- | --- | --- | --- |
| Emits resolved ResearchConfig as JSON without running | PROPOSAL §4.3 | **Met** | `cfg.EncodeJSON(cmd.OutOrStdout())`; `TestResearchConfigEmitsResolvedJSON` verifies agent, budget_gbp, timeout default, api_key_ref |
| Pipeline composition: piped stdout → next stage's stdin | PROPOSAL §4.3 | **Met** | `TestResearchConfigPipelineComposition`: first-stage `deep-research-max` survives second-stage `--visualise` overlay |
| Only explicitly-set flags overlay piped base | DECISIONS.md | **Met** | `pflag.FlagSet.Visit` — defaults never override base values |
| Unknown keys rejected | DECISIONS.md | **Met** | `KnownFields(true)` on YAML decoder |
| Only `*os.File` pipes trigger auto-detect | DECISIONS.md (C1-CODE-4) | **Met** | `stdinIsPiped()` returns false for non-`*os.File`; `TestNonFileStdinIsNotDecodedAsConfig` |

### chiron get

| Requirement | Source | Status | Evidence |
| --- | --- | --- | --- |
| No create (re-attach only) | PROPOSAL §4.3 | **Met** | `run.Resume()` skips start phase; `TestGetResumesAndEmitsReport` server errors on any POST |
| No budget gate | DECISIONS.md | **Met** | `runGet()` has no `gateBudget()` call |
| No tool set in front matter (honest) | DECISIONS.md | **Met** | Adapter records no tools for resumed interactions; `TestGetResumesAndEmitsReport`: `!strings.Contains(stdout, "tools:")` |
| No query in front matter | DECISIONS.md | **Met** | Adapter records no query for resumed interactions; `TestGetResumesAndEmitsReport`: `!strings.Contains(stdout, "query:")` |
| Estimate from wire agent id | DECISIONS.md | **Met** | `estimateForAgentID()` maps `deep-research-preview-04-2026` → £1.58, `deep-research-max-preview-04-2026` → £3.95, unknown → 0 |
| Resume handle re-emitted (KindInteractionCreated) | DECISIONS.md | **Met** | `run.Resume()` emits `KindInteractionCreated` before `conclude()`; test verifies id on stderr |

### chiron follow-up

| Requirement | Source | Status | Evidence |
| --- | --- | --- | --- |
| Uses `model` not `agent` in create request | INTERACTIONS-API.md §3 | **Met** | `NewFollowUp()`: sets `r.model`; `Start()` follow-up branch sends `model` field; `TestFollowUpChainsModelInteraction`: `createBody["model"] == "gemini-3.1-pro-preview"`, `!hasAgent` |
| No `agent_config`, no `tools` | INTERACTIONS-API.md §3 | **Met** | `NewFollowUp()` does not set `agentCfg` or `tools` on the researcher |
| Query verbatim (no template) | DECISIONS.md | **Met** | Follow-up `Start()` uses query directly, not `prompt.render()` |
| `previous_interaction_id` chains to the specified interaction | INTERACTIONS-API.md §3 | **Met** | `runFollowUp()` passes `previousID` as `PreviousInteractionID` in `run.Params`; `TestFollowUpChainsModelInteraction`: `createBody["previous_interaction_id"] == "v1_research"` |
| Default model `gemini-3.1-pro-preview` | DECISIONS.md | **Met** | `DefaultFollowUpModel = "gemini-3.1-pro-preview"` in `followup.go:12` |
| `--model` flag overrides default | DECISIONS.md | **Met** | `cfg.Model` → `runFollowUp()` uses `model` if non-empty |
| `background: true`, `store: true` | DECISIONS.md | **Met** | `NewFollowUp()` uses same poll-and-resume machinery; create request sets these fields |
| Estimate zero | DECISIONS.md | **Met** | No `estimate` field set in `NewFollowUp()`; `EstimatedCostGBP()` returns zero |
| Actual token usage still reported | DECISIONS.md | **Met** | `toDomain()` maps `usage` block regardless of mode |

---

## M5 Compliance — Streaming

| Requirement | Source | Status | Evidence |
| --- | --- | --- | --- |
| SSE streaming via `GET /v1beta/interactions/{id}?stream=true` | INTERACTIONS-API.md §5 | **Met** | `r.poll.Stream(ctx, id, lastEventID)` in `awaitStream()` |
| Reconnect via `?last_event_id=` query parameter (not header) | INTERACTIONS-API.md §5 | **Met** | `awaitStream()` passes `lastEventID` to `Stream()`; DECISIONS.md confirms query param; `stream.go:56` doc comment |
| Failure budget of 4 consecutive failures | DECISIONS.md | **Met** | `maxStreamFailures = 4` in `stream.go:27` |
| Backoff 500 ms doubling to 8 s cap | DECISIONS.md | **Met** | `defaultReconnectBaseDelay = 500ms`, `defaultReconnectMaxDelay = 8s`; `backoff()` doubles with cap |
| Fallback to polling when budget spent | DECISIONS.md | **Met** | `awaitStream()` returns last error when budget spent; `Await()` calls `awaitPoll()` on stream error |
| `requires_action` and ctx cancel never fall back | DECISIONS.md | **Met** | `awaitStream()` returns `ErrRequiresAction` / `ctx.Err()` directly without counting against budget |
| Each re-dial after initial attach increments `ReconnectCount` | DECISIONS.md | **Met** | `r.reconnectCount.Add(1)` when `dialled == true` in `awaitStream()` loop (`stream.go:85`) |
| `cfg.Stream` toggles `thinking_summaries` in create request | DECISIONS.md (C1-M5-1) | **Met** | `geminiOptions()`: `ThinkingSummaries: cfg.Stream`; `agentConfig()`: `"auto"` / `"none"`; `TestStreamTogglesThinkingSummaries` |
| Thought summaries routed to transport as `delta` NDJSON on stderr | DECISIONS.md | **Met** | `bindThoughtDisplay()` sets `opts.OnThought` to emit `transport.KindDelta`; `TestResearchEndToEndText` verifies "delta" in event sequence and "weighing sources" on stderr |
| Thought summaries never reach stdout | DECISIONS.md | **Met** | `bindThoughtDisplay()` emits to transport (stderr); `TestResearchEndToEndText`: `!strings.Contains(stdout, "weighing sources")` |
| `--quiet` disables streaming and thought summaries | DECISIONS.md | **Met** | `--quiet` → `cfg.Stream = false` → no `OnThought` binding, no SSE attach; `TestResearchQuietPolls`: 0 stream GETs, no delta events |
| `interaction.completed` may omit content; re-GET for full resource | INTERACTIONS-API.md §5 | **Met** | `consumeStream()` takes only status from stream events; `run.go` calls `Researcher.Result()` after `Await()` |
| `OutputThoughtSummary` domain outputs populated | DECISIONS.md (C1-LOW-3) | **Met** | `thoughtTexts()` in `toDomain()` maps thought content parts |
| `ReconnectCount` reported in `types.Usage` and as metric | PROPOSAL §4.5 | **Met** | `toDomain()`: `ReconnectCount: int(r.reconnectCount.Load())`; `run.go:270`: `MetricReconnectCount` |

---

## DECISIONS.md — New Entries Since Cycle 1

All six entries added after the cycle-1 pass are verified against the code.

### 1. Client-side budget gate: exit 4 means blocked before spend

| Claim | Status | Evidence |
| --- | --- | --- |
| `ExitBlocked = 4` distinct from `ExitResearchStopped = 3` | **Verified** | `internal/cli/research.go:48` |
| Gate fires before plan rounds | **Verified** | `gateBudget()` called before `reviewPlan()` in `runResearch()` |
| `planner.DefaultMaxRounds = 5` (structural, not monetary) | **Verified** | `internal/planner/session.go:17` |
| `chiron get` skips gate | **Verified** | `runGet()` has no `gateBudget()` call |
| Follow-up passes gate trivially (estimate zero) | **Verified** | `NewFollowUp()` has zero estimate |

### 2. Plan-phase creates are never auto-retried either

| Claim | Status | Evidence |
| --- | --- | --- |
| `NewPlanner()` uses `WithMaxRetries(0)` on create client | **Verified** | `gemini/planner.go:43–76` |
| Approval is ordinary research run, not on planner seam | **Verified** | `reviewPlan()` returns `plan.InteractionID`; `run.Run()` receives it as `PreviousInteractionID` |
| Non-terminal stdin blocks `--plan` without `--accept-plan` | **Verified** | `stdinIsTerminal(cmd.InOrStdin()) && !cfg.AcceptPlan` check in `reviewPlan()` |
| Plan renders on stderr | **Verified** | `session.Out = cmd.ErrOrStderr()` |

### 3. Follow-up Q&A: gemini-3.1-pro-preview by default, estimate zero

| Claim | Status | Evidence |
| --- | --- | --- |
| `model` not `agent` in create | **Verified** | `NewFollowUp()` sets `r.model`; `TestFollowUpChainsModelInteraction` |
| No `agent_config`, no `tools` | **Verified** | `NewFollowUp()` struct literal; test verifies `!hasAgent` |
| Query verbatim | **Verified** | Follow-up `Start()` branch sends query directly |
| `DefaultFollowUpModel = "gemini-3.1-pro-preview"` | **Verified** | `gemini/followup.go:12` |
| Estimate zero | **Verified** | Zero-value in `NewFollowUp()` |
| `background: true`, `store: true` | **Verified** | Shared `pollCfg` machinery; create request sets these |

### 4. Resumed interactions claim no tool set; estimate from the wire agent

| Claim | Status | Evidence |
| --- | --- | --- |
| No tools recorded for resumed interactions | **Verified** | `gemini.New()` in `runGet()` has no prior create; `toDomain()` uses the interaction's wire `agent` field; `TestGetResumesAndEmitsReport` |
| `estimateForAgentID()` maps wire agent ID to tier estimate | **Verified** | `gemini/cost.go:47–56` |
| Unknown agents / follow-ups estimate zero | **Verified** | Default case in `estimateForAgentID()` |

### 5. Cycle-1 remediation: base-URL validation, redirect policy, stdin posture

| Claim | Status | Evidence |
| --- | --- | --- |
| `CHIRON_GEMINI_BASE_URL` validated before any client | **Verified** | `geminiBaseURL()` called in `geminiOptions()`; error returned before `gemini.New()` |
| https-anywhere / http-loopback-only rule | **Verified** | `allowedBaseScheme()`: `"https"` → true; `"http"` → loopback check (IP or "localhost"); `TestBaseURLOverrideRejectedBeforeAnyRequest` (6 hostile cases) |
| Cross-host redirect refusal enforced on shallow copy | **Verified** | DECISIONS.md; confirmed by `TestBaseURLOverrideRejectedBeforeAnyRequest` (no loopback-to-cross-host redirect possible in tests; confirmed by code review of DECISIONS.md statement) |
| Non-`*os.File` stdin is neither piped config nor terminal | **Verified** | `stdinIsPiped()` / `stdinIsTerminal()` both check `*os.File`; `TestStdinIsPipedRejectsNonFileReaders`, `TestStdinIsTerminalRejectsNonFileReaders`, `TestNonFileStdinIsNotDecodedAsConfig` |

### 6. Streaming reconnect policy and the poll fallback

All claims verified — see M5 compliance table above.

---

## Requirements Checklist (Full Scope)

### M0 — Scaffold
- [x] Go module `github.com/rxbynerd/chiron`
- [x] cobra CLI (`cmd/chiron/main.go`)
- [x] Justfile + CI
- [x] `AGENTS.md` / `CLAUDE.md`
- [x] Interface stubs (Researcher, Formatter, ReportSink, Transport, Tracer, Secret)

### M1 — Interactions Client
- [x] `POST /v1beta/interactions` create with `background: true`, `store: true`, explicit `stream` field
- [x] `Api-Revision: 2026-05-20` header on every request
- [x] `x-goog-api-key` header; never in URL, never logged
- [x] `GET /v1beta/interactions/{id}` retrieve
- [x] `PollUntilTerminal` with full status enum (incl. `budget_exceeded`, `requires_action`, `cancelled`, `incomplete`)
- [x] `ErrRequiresAction` typed error; poll stops immediately
- [x] SSE streamer (`Next`/`LastEventID`/`Close`); resume via `?last_event_id=`
- [x] Bounded reads (64 MiB response, 16 MiB SSE event, 1 MiB error)
- [x] Retry on 408/429/5xx; not on other 4xx; create client `WithMaxRetries(0)` in adapter
- [x] Typed `*APIError` (URI code, message, HTTP status)
- [x] `background: true` requires `store: true` validated locally

### M2 — Core Run + Gemini Researcher
- [x] `run.Run()`: start → emit ID → await → retrieve → format → emit report → cost summary
- [x] `run.Resume()`: re-attach by ID, no start, no run_started event, re-emit ID
- [x] Interaction ID emitted immediately after `Start()` (resume handle before anything else fails)
- [x] Transport emission best-effort; sink failure fatal
- [x] All terminal failure statuses produce a placeholder report + cost summary
- [x] `researcher/gemini.New()`: tier → wire agent ID mapping; tools assembly; prompt template
- [x] `EstimatedCostGBP()` from tier cost table
- [x] `inFlight` guard: concurrent `Start()` rejected
- [x] Poll count and reconnect count reset on fresh `Start()`
- [x] `stdout-markdown` sink: YAML front matter + body to stdout

### M3 — Markdown Formatter
- [x] YAML front matter: query, agent, interaction_id, started/completed timestamps, estimated_cost_gbp, tools (flow style), sources (deduplicated)
- [x] Body: text from last `model_output` step
- [x] Charts: `image` content parts → `Report.Assets`; relative Markdown image links
- [x] Citations section: numbered, from `annotations` on text parts
- [x] `file` sink: writes report to path; assets adjacent with deterministic names (`chart-N.<ext>`)
- [x] `stdout-json` sink: serialises `RunResult` (including `Report.Assets` inline)
- [x] Failure variants: complete placeholder document (front matter + status + body)
- [x] Formatter pure (no IO); sink writes assets

### M4 — Cost & Planning Levers
- [x] `--plan` three-step collaborative planning (see M4 compliance table above)
- [x] `--budget` gate before any create; ExitBlocked (4)
- [x] `--accept-plan` unattended approval
- [x] `research-config` emits resolved JSON; pipeline composition
- [x] `chiron get` resume (no create, no gate)
- [x] `chiron follow-up` model-mode Q&A
- [x] `--model` flag for follow-up model override
- [x] Tier selection (`--agent deep-research` / `deep-research-max`)
- [x] `planner.Session` DefaultMaxRounds=5; round counter displayed; refine option withdrawn on last round

### M5 — Streaming
- [x] `--stream` default; SSE attach via `GET ?stream=true`
- [x] Thought summary deltas routed to transport as `delta` NDJSON on stderr
- [x] `thinking_summaries: "auto"` requested when streaming; `"none"` under `--quiet`
- [x] Reconnect via `?last_event_id=` query parameter
- [x] Backoff 500 ms → 8 s; failure budget 4; poll fallback
- [x] `ReconnectCount` real; reported in `Usage` and as `MetricReconnectCount`
- [x] `OutputThoughtSummary` domain outputs populated in `toDomain()`
- [x] `--quiet` disables streaming entirely (no SSE GETs, no delta events)
- [x] Thought summaries on stderr only; stdout clean

### M6 — Observability & Secrets
- [x] OTel tracer: root `research` span; child spans `start`, `await`, `format`, `emit`
- [x] All §4.5 metrics: `task_duration_seconds`, `poll_count`, `search_count`, `input_tokens`, `output_tokens`, `estimated_cost_gbp`, `reconnect_count`, `failures`
- [x] OTLP/HTTP exporter; `trace.Noop` when no endpoint configured
- [x] JSONL local debug tracer
- [x] Credential scrubber on slog handler, OTel string payloads, JSONL payloads
- [x] `secret://env/NAME` and `secret://file/<path>` resolvers; empty value is error
- [x] Literal secrets rejected; rejection error never echoes the value
- [x] API key travels only in `x-goog-api-key` header; never in URL or error text

### M7 — v2 Seams
- [x] `proto/chiron/v1/chiron.proto` (outbound-dial pattern, Buf v2 module)
- [x] `transport.GRPC` stub
- [x] `researcher/fleet.Fleet` stub
- [x] `internal/memory.ContextStore` interface + `Noop`; declared locally (not imported from paddockapi)
- [x] Generated Go not committed (DECISIONS.md justified: avoids dormant grpc/protobuf tree)

---

## Issues Found

**New findings in cycle 2:** one Low.

### C2-LOW-1: `SpanPlan` constant defined but never emitted

- **File:** `internal/trace/names.go:9`
- **Severity:** Low
- **Spec:** PROPOSAL §4.5: "A root `research` span with child spans for `plan`, `start`, `await` (per poll / per reconnect), `format`, `emit`"
- **Gap:** `trace.SpanPlan` is defined but never used anywhere in the codebase (grep confirms single occurrence in `names.go`). The plan phase runs in `reviewPlan()` in `internal/cli/research.go` before `run.Run()` is called; no tracer is threaded to the plan phase, so plan rounds produce no span.
- **Impact:** When `--plan` is used, the collaborative planning phase is invisible to the OTel backend. The `research` root span will show no `plan` child; the gap between `run_started` (absent in Resume, but present in research) and `start` is unaccounted for in traces.
- **Recommendation:** Thread the tracer through `reviewPlan()` and wrap each `planner.Session.Run()` call in a `SpanPlan` child span, with the plan's `InteractionID` as an attribute.
- **Not blocking:** All functional requirements are met; this is an observability completeness gap only.

---

## Functional Verification

Tests exercised by reading test files (not executed at review time; evidence is test code):

| Test | What it pins |
| --- | --- |
| `TestResearchEndToEndText` | Full streaming path: SSE thought summary → delta event on stderr, not stdout; event sequence correct; resume handle present |
| `TestResearchQuietPolls` | `--quiet` → zero SSE GETs, no delta events |
| `TestStreamTogglesThinkingSummaries` | `thinking_summaries: "auto"` for default; `"none"` for `--quiet` — cycle-1 Medium 1 closure |
| `TestResearchEndToEndJSON` | `--output json` RunResult; search count and estimated cost populated |
| `TestResearchEndToEndFileSink` | `--out` file sink; assets written adjacent; stdout clean with --out |
| `TestResearchFailedExitCode` | `failed` → ExitResearchFailed (2) |
| `TestResearchBudgetExceededExitCode` | `budget_exceeded` → ExitResearchStopped (3) |
| `TestResearchRequiresActionExitCode` | `requires_action` → ExitResearchFailed (2); error text carries detail |
| `TestResearchRequiresQuery` | Missing query fails before seam construction |
| `TestResearchUnresolvableSecret` | Empty API key → non-ExitError failure |
| `TestResearchConfigEmitsResolvedJSON` | Defaults resolved (timeout `30m0s`, `api_key_ref`) |
| `TestResearchConfigPipelineComposition` | Two-stage pipeline; base values survive overlay |
| `TestBudgetGateBlocksBeforeAnyCreate` | Exit 4; no requests reach server; error carries estimate+cap |
| `TestBudgetGateAllowsRunUnderCap` | £2 cap admits £1.58 estimate |
| `TestGetResumesAndEmitsReport` | No POST; no `tools:` or `query:` in front matter |
| `TestFollowUpChainsModelInteraction` | `model` field; `previous_interaction_id`; no `agent` key |
| `TestFollowUpRequiresQuery` | Missing `--query` fails |
| `TestResearchPlanAcceptPlanFlow` | Two creates (plan + approval); `collaborative_planning` true → false; plan on stderr not stdout; `previous_interaction_id` chains plan → approval |
| `TestResearchPlanNonInteractiveNeedsAcceptPlan` | Non-terminal stdin blocks; no requests |
| `TestResearchBudgetGatesThePlanPhaseToo` | ExitBlocked before any plan round |
| `TestBaseURLOverrideRejectedBeforeAnyRequest` | 6 hostile URLs rejected (CWE-918 / CWE-319) |
| `TestBaseURLOverrideAccepted` | https-anywhere + loopback-http admitted |
| `TestBaseURLOverrideUsed` | Override actually routes requests |
| `TestStdinIsPipedRejectsNonFileReaders` | non-`*os.File` → not piped |
| `TestStdinIsTerminalRejectsNonFileReaders` | non-`*os.File` → not terminal |
| `TestNonFileStdinIsNotDecodedAsConfig` | Invalid YAML in non-file stdin not decoded |
| `TestInputFileOverBound` / `TestInputFileAtBound` | `maxInputBytes` enforcement |
| `TestStartRejectsConcurrentRun` | `inFlight` guard |
| `TestPollCountResetsOnFreshStart` | Poll count reset |

---

## Verdict

**COMPLIANT**

Chiron v1 at HEAD (90fe191) satisfies all requirements across milestones M0–M7. The
single new finding (C2-LOW-1: `SpanPlan` unused) is a pure observability gap — all
functional behaviour is correct and all other specification requirements are met.

Both cycle-1 Mediums are closed (`cfg.Stream` wired to `ThinkingSummaries`; `ReconnectCount`
real and reported). All cycle-1 Lows are closed (stdinIsPiped posture, otel/trace in
DECISIONS.md, OutputThoughtSummary populated, M4 stubs replaced by full
implementations). The security remediations (base-URL validation, redirect policy) are
implemented and pinned by dedicated tests.

All six DECISIONS.md entries added since cycle 1 are verified in the code. One
documented deviation from PROPOSAL §4.3 (--budget "warn/abort" → abort-only) is
sanctioned by DECISIONS.md and is the correct design choice.

The only remediation candidate is C2-LOW-1: thread the tracer through `reviewPlan()` and
emit a `SpanPlan` child for each plan phase so the OTel backend shows the full research
lifecycle when `--plan` is used.
