# Spec Compliance Review — Cycle 1

**Reviewer:** spec-compliance-reviewer  
**Date:** 2026-06-07  
**HEAD:** main (dea3bf3)  
**Scope:** M0–M3 + M6 + M7 (scaffold, interactions client, core run + Gemini researcher,
Markdown formatter, observability/secrets, v2 seams)  
**Normative precedence:** INTERACTIONS-API.md > DECISIONS.md > PROPOSAL.md  
**Out of scope:** `--plan`, `--budget`, `research-config` behaviour, `get`, `follow-up`,
`--stream` runtime behaviour (flag may exist; streaming not judged)

---

## Specification Summary

Chiron v1 is a Go CLI that drives a Gemini Deep Research agent end-to-end: start a
background interaction, await completion by polling, retrieve and format the result as
a cited Markdown report, and emit it. The core is a pure-function loop that depends
only on injected interface seams. M0–M3+M6+M7 cover: the scaffold and interface stubs;
the hand-rolled Interactions API client (create, get, poll, SSE); the run core plus
Gemini adapter; the Markdown formatter; OTel+JSONL observability with secret scrubbing;
and the v2 interface stubs (proto, grpc transport, fleet researcher, ContextStore no-op).

---

## Requirements Checklist

### Wire correctness — INTERACTIONS-API.md

| # | Requirement | Status | Note |
|---|---|---|---|
| W1 | Base URL `https://generativelanguage.googleapis.com` | Met | `doc.go:19` `DefaultBaseURL` |
| W2 | `POST /v1beta/interactions` create | Met | `client.go:16` `basePath` + `Create()` |
| W3 | `GET /v1beta/interactions/{id}` retrieve | Met | `client.go:139` `Get()` |
| W4 | `POST /v1beta/interactions/{id}/cancel` | Met | `client.go:155` `Cancel()` |
| W5 | `x-goog-api-key` header on every request | Met | `client.go:228` |
| W6 | `Api-Revision: 2026-05-20` header on every request | Met | `doc.go:24` + `client.go:229` |
| W7 | `Content-Type: application/json` on POST | Met | `client.go:233` |
| W8 | Create payload: `agent`, `input`, `agent_config.type`, `thinking_summaries`, `visualization`, `collaborative_planning` | Met | `wire.go:54–106`; `agentConfig()` in `gemini.go:174` |
| W9 | `background`, `store`, `stream` emitted explicitly (no `omitempty`) | Met | `wire.go:72–74`; test `TestCreateRequestEmitsExplicitBooleans` |
| W10 | `background: true` requires `store: true` — enforced locally | Met | `client.go:125–129` |
| W11 | `tools`: `mcp_server` with `AllowedTools` object form; `file_search` with store names | Met | `wire.go:121–148`; both forms assembled in `gemini.go:194` |
| W12 | `AllowedTools` tolerates both object and bare-array forms on unmarshal | Met | `wire.go:142–148`; `TestAllowedToolsUnmarshalBothForms` |
| W13 | Custom function tools rejected | Met | `gemini.go:217` |
| W14 | Response shape is `steps[]`, not `outputs[]` (§8 drift) | Met | `wire.go:164`; `FinalOutput()` walks steps |
| W15 | Final report = text content of last `model_output` step | Met | `wire.go:185–197` `FinalText()` |
| W16 | Charts = `image` parts of `model_output` steps | Met | `wire.go:200–214` `Images()` |
| W17 | Citations = `annotations` on text content (url_citation, file_citation), deduped by URI | Met | `wire.go:229–259` `Citations()`; `TestCitationsMapsBothKindsAndDedupes` |
| W18 | Full status enum: in_progress, requires_action, completed, failed, cancelled, incomplete, budget_exceeded | Met | `wire.go:20–28` |
| W19 | `requires_action` stops poll immediately with typed `ErrRequiresAction` (not hang) | Met | `poll.go:77–79`; `TestStatusTerminalAndFailed` |
| W20 | All non-in_progress/requires_action statuses are terminal | Met | `wire.go:36–38` `Status.Terminal()` |
| W21 | SSE events: `interaction.created`, `step.delta`, `interaction.status_update`, `interaction.completed`, `error` | Met | `stream.go:22–29` |
| W22 | Delta type `thought_summary_delta` (not `thought_summary`) — §8 drift | Met | `stream.go:37` `DeltaThoughtSummary` |
| W23 | `last_event_id` resume via `?last_event_id=` query param, not `Last-Event-ID` header | Met | `stream.go:109`; decision confirmed in DECISIONS.md |
| W24 | After `interaction.completed` event, do a plain GET for full resource | Met | `stream.go:228–240`; stated in `Event.Interaction` field comment |
| W25 | `event_id` accepted from both SSE `id:` field and JSON `event_id` payload field | Met | `stream.go:214–219` |
| W26 | Error format `{error: {code, message}}` with URI code | Met | `error.go:57–68` `parseAPIError()` |
| W27 | Retry on 408/429/5xx; never retry other 4xx | Met | `error.go:47–51` `retryableStatus()` |
| W28 | Response body bound 64 MiB; SSE event bound 16 MiB; error body bound 1 MiB | Met | `client.go:27–29`; `readBounded()` fails rather than truncating |
| W29 | Agent tier mapping: `deep-research` → `deep-research-preview-04-2026`; `deep-research-max` → `deep-research-max-preview-04-2026` | Met | `doc.go:35–39`; `gemini.go:160–168` |
| W30 | `LastVerified` constant records docs revision date | Met | `doc.go:31` `LastVerified = "2026-06-07"` |

### PROPOSAL §4.1 — Seam purity

| # | Requirement | Status | Note |
|---|---|---|---|
| S1 | Run core depends only on seam interfaces; no Gemini types | Met | `run.go` imports: `researcher`, `formatter`, `sink`, `trace`, `transport`, `types` — no `interactions` or `gemini` |
| S2 | Concrete types injected from config at the composition root | Met | `research.go:88–113` injects all deps from `ResearchConfig` |
| S3 | `Researcher` seam: `Start(Task) (id, error)`, `Await(id) error`, `Result(id) *Interaction` | Met | `researcher.go:26–37` |
| S4 | `Formatter`, `ReportSink`, `Transport`, `Tracer`, `Secret` seams defined | Met | `formatter.go`, `sink.go`, `transport.go`, `trace.go`, `secret.go` |

### PROPOSAL §4.2 — Research lifecycle

| # | Requirement | Status | Note |
|---|---|---|---|
| L1 | Interaction ID emitted as resume handle the moment `Start` returns, before `Await` | Met | `run.go:138` `emit(KindInteractionCreated)` between `Start` and `Await` |
| L2 | Failure-variant statuses (failed, cancelled, budget_exceeded, incomplete) produce placeholder report, not an error | Met | `run.go:87–97` docstring; `formatter.go:87–90` body fallback |
| L3 | Transport emit failures are best-effort (do not abort run) | Met | `run.go:199–209` `emit()` helper records to span rather than returning error |
| L4 | Sink failures are fatal | Met | `run.go:186–189` returns error on sink failure |

### PROPOSAL §4.3 — CLI flags

| # | Flag | Default | Status | Note |
|---|---|---|---|---|
| F1 | `--query` / positional | — | Met | `flags.go:18`; positional arg applied in `resolveConfig()` |
| F2 | `--agent` | `deep-research` | Met | `config.go:92` `Default()` |
| F3 | `--plan` | off | Met (stub) | Registered and parsed; not acted on (M4 scope, out of this review) |
| F4 | `--visualise` | off | Met | Wired to `gemini.Options.Visualise` → `VisualizationAuto` in `AgentConfig` |
| F5 | `--stream` / `--quiet` mutually exclusive | Met | `commands.go:121` `MarkFlagsMutuallyExclusive` |
| F6 | `--stream` default `true` | Met | `config.go:94` |
| F7 | `--stream` wired to API behavior | **Partial** | See Finding 1 — flag is parsed but `ThinkingSummaries` is never set from it |
| F8 | `--tools` | API defaults | Met | Wired; adapter emits default trio explicitly when nil |
| F9 | `--mcp name=url` | — | Met | `flags.go:26`; `parseMCP()` in `flags.go:96` |
| F10 | `--file-search` | — | Met | Wired to `gemini.Options.FileSearch` |
| F11 | `--input` | — | Met | Wired; `input.go` builds typed Content parts |
| F12 | `--template` | built-in | Met | `template.go` embeds `template.md`; `--template` replaces it |
| F13 | `-o, --output` `text\|json\|none` | `text` | Met | `config.go:94`; validated in `Validate()` |
| F14 | `--out <path>` (file sink, distinct from `--output`) | stdout | Met | `buildSink()` in `research.go:165` |
| F15 | `--api-key-ref` default `secret://GEMINI_API_KEY` | Met | `config.go:44,94` |
| F16 | `--budget <gbp>` | unset | Met (stub) | Parsed; enforcement is M4 scope |
| F17 | `--timeout` default `30m`, hard cap `60m` | Met | `config.go:47,97`; `Validate()` enforces `(0, 60m]` |
| F18 | Config merge: flag overlay over base, only explicitly-set flags apply | Met | `flags.go:48` `fs.Visit`; documented in DECISIONS.md |
| F19 | `--quiet` sugar for `--stream=false` | Met | `flags.go:61` |

### PROPOSAL §4.4 — Markdown output

| # | Requirement | Status | Note |
|---|---|---|---|
| M1 | YAML front matter: query, agent, interaction id | Met | `markdown.go:37–50` `frontMatter` struct |
| M2 | Front matter: started/completed timestamps (RFC 3339 UTC) | Met | `markdown.go:212` `stamp()` |
| M3 | Front matter: estimated cost | Met | `EstimatedCostGBP float64` in front matter |
| M4 | Front matter: tool set (flow style) | Met | `Tools []string \`yaml:"tools,omitempty,flow"\``; populated from `types.Interaction.Tools` |
| M5 | Front matter: deduplicated sources list | Met | `collectSources()` in `markdown.go:184` |
| M6 | Body: last text output (thought summaries excluded) | Met | `finalText()` in `markdown.go:141`; searches backwards for `OutputText` |
| M7 | Charts: each image output → asset file, relative link in Markdown | Met | `markdown.go:100–107`; `types.Report.Assets`; file sink writes assets before report |
| M8 | Numbered sources section at foot | Met | `markdown.go:129–133` |
| M9 | No-content interactions produce placeholder body | Met | `markdown.go:93–95` |
| M10 | Formatter performs no IO (assets returned, not written) | Met | DECISIONS.md decision; formatter returns `Report.Assets`, file sink writes them |
| M11 | MIME→extension table is hand-rolled (not `mime.ExtensionsByType`) | Met | `markdown.go:220–238` `extensionFor()` |

### PROPOSAL §4.5 — Observability & secrets

| # | Requirement | Status | Note |
|---|---|---|---|
| O1 | Root `research` span with child spans: plan, start, await, format, emit | Met | `names.go:8–14`; `run.go:110–183` |
| O2 | Metrics: task_duration_seconds, poll_count, search_count, input_tokens, output_tokens, estimated_cost_gbp, reconnect_count, failures | Met | `names.go:16–27`; `recordMetrics()` `run.go:215` |
| O3 | OTel OTLP/HTTP exporter (Langfuse/Grafana backend) | Met | `otel.go`; bound only when `OTEL_EXPORTER_OTLP_*` present |
| O4 | JSONL local debug tracer | Met | `jsonl.go` |
| O5 | Every string payload scrubbed before SDK sees it | Met | `otel.go:64,86,88,99,110`; `jsonl.go:49,65,108,134` |
| O6 | `secret://` resolution: `env`, `file` backends | Met | `resolve.go` |
| O7 | Literal secrets rejected (not `secret://` prefix) | Met | `resolve.go:53–55`; `config.Validate()` at `config.go:151` |
| O8 | Empty variable or empty file is an error | Met | `resolve.go:97–105`, `125–128` |
| O9 | `ScrubHandler` wraps slog; leakage structurally impossible | Met | `handler.go`; every record path goes through `scrubAttr()` |
| O10 | `reconnect_count` metric | **Partial** | Always 0 — reconnect logic is M5; metric is wired and recorded but never > 0 |

### PROPOSAL §5 — ContextStore seam

| # | Requirement | Status | Note |
|---|---|---|---|
| C1 | `ContextStore` interface defined | Met | `memory.go:79` |
| C2 | `Noop` implementation: writes succeed silently, reads return `ErrNotFound` | Met | `noop.go` |
| C3 | Declared locally, not imported from paddockapi | Met | `memory` package stands alone; DECISIONS.md confirms rationale |
| C4 | Not injected into run core (deliberately absent until v2) | Met | `run.Deps` has no `ContextStore` field — §5 says "no-op in v1" |

### PROPOSAL §6 / M7 — v2 seams

| # | Requirement | Status | Note |
|---|---|---|---|
| V1 | `proto/chiron/v1/chiron.proto` committed | Met | `proto/chiron/v1/chiron.proto` present |
| V2 | Proto: outbound-dial pattern (runner dials control plane) | Met | `ControlPlaneService.Session` bidirectional; comment documents direction |
| V3 | Proto run-lifecycle events mirror v1 NDJSON kinds one-to-one | Met | `RunStarted`, `InteractionCreated`, `StatusChanged`, `Delta`, `RunCompleted`, `CostSummary` match `transport/events.go` |
| V4 | Generated Go not committed | Met | No `.pb.go` files; DECISIONS.md documents rationale |
| V5 | `transport.GRPC` stub compiles and satisfies `Transport` | Met | `grpc.go`; `var _ Transport = (*GRPC)(nil)` |
| V6 | `researcher/fleet.Fleet` stub compiles and satisfies `Researcher` | Met | `fleet.go`; `var _ researcher.Researcher = Fleet{}` |

### DECISIONS.md cross-checks

| Decision | Status | Note |
|---|---|---|
| ContextStore declared locally | Met | See C3 |
| Dependency set: cobra, pflag, yaml.v3 | Met | `go.mod`; OTel justified separately |
| Config merge semantics | Met | `fs.Visit`; `KnownFields(true)`; `--quiet` ↔ `--stream=false` |
| Researcher is Start/Await/Result | Met | `researcher.go` |
| Wire types vs domain types | Met | `interactions/` owns wire; `types/` owns domain; adapter bridges |
| Durations serialise as strings | Met | `config/duration.go` |
| `secret://` reference grammar | Met | `resolve.go:52–82` |
| Formatter performs no IO | Met | See M10 |
| NDJSON events to stderr | Met | `research.go:103` `cmd.ErrOrStderr()` |
| proto committed; generated Go not committed | Met | See V4 |
| OTel dependency set (HTTP exporter; metrics as span attributes) | Met | `otel.go`; `go.mod` |
| interactions client: retries, bounds, SSE resume | Met | `client.go`, `poll.go`, `stream.go` |
| `requires_action` returns typed error | Met | `poll.go:77`; `ErrRequiresAction` |
| Create never retried in gemini adapter | Met | `gemini.go:131` `WithMaxRetries(0)` on create client |
| Estimated cost is tier-table planning figure | Met | `cost.go`; midpoints, 0.79 USD→GBP |
| Domain Interaction carries resolved tool set | Met | `types.Interaction.Tools`; populated in `toDomain()` |
| Run-core: emit errors tolerated, sink errors fatal | Met | `run.go:199–209`; `run.go:186–189` |
| CLI exit codes 0/1/2/3 | Met | `research.go:28–38`; help text documents contract |

---

## Functional Verification

The test suite provides high-coverage functional verification:

- **`TestInteractionDecodesReferenceExample`** (`interactions/wire_test.go:63`) — pins the
  wire decode against the §4 reference JSON including steps, annotations, usage, images,
  and citations. Passes.
- **`TestCreateRequestEmitsExplicitBooleans`** (`wire_test.go:197`) — confirms
  `background`, `store`, `stream`, and `collaborative_planning` are never omitted. Passes.
- **`TestAllowedToolsUnmarshalBothForms`** — tolerates both allowed_tools forms. Passes.
- **`TestResearchEndToEndText` / `...JSON` / `...FileSink`** (`cli/research_test.go`) —
  smoke-tests the full path against an httptest server. Front matter (tools, sources),
  body, chart assets, NDJSON event sequence on stderr, and file sink verified. Passes.
- **`TestResearchFailedExitCode` / `BudgetExceededExitCode` / `RequiresActionExitCode`** —
  exit-code contract verified for all terminal-failure variants. Passes.
- **`TestResearchUnresolvableSecret`** — confirms secret failures are not wrapped in
  `ExitError`. Passes.
- **`TestMarkdownFormat`** (golden files) — formatter output pinned against `.md` fixtures.

Tests run against `go 1.26.4` (module constraint); no real network calls in any test.

---

## Issues Found

### Finding 1 — Medium: `--stream` / `--quiet` flags have no effect on API request
**File:** `internal/cli/research.go:88–99`  
**Spec clause:** PROPOSAL §4.3 (`--stream | --stream | Stream thought summaries vs poll
silently`); DECISIONS.md `gemini.go` comment

`runResearch()` builds `gemini.Options` without setting `ThinkingSummaries`:

```go
res, err := gemini.New(gemini.Options{
    APIKey:    apiKey,
    Tier:      cfg.Agent,
    Visualise: cfg.Visualise,
    // cfg.Stream is never read here
    ...
})
```

`gemini.Researcher` therefore always sets `thinking_summaries: "none"` on the create
request regardless of whether `--stream` or `--quiet` is passed. The two flags are
mutually exclusive in the command definition but produce identical API requests.

DECISIONS.md justifies this explicitly: *"polling runs leave it false ('none') — there
is nothing to display them on until the M5 streaming surface."* The deferral is
documented and intentional.

**Impact:** `--stream` (default true) is user-visible and correctly defaults to true per
the spec, but the flag has no observable effect on the current run. The API never sends
thought summaries, so M5's streaming display surface will have nothing to show even if
wired tomorrow without also fixing this. The missing link is a one-liner:
`ThinkingSummaries: cfg.Stream` in `gemini.Options`.

**Severity:** Medium — intentional pre-M5 deferral, but the connection between flag and
API request is not established, which means M5 wiring the display layer alone is
insufficient.

---

### Finding 2 — Medium: `reconnect_count` metric always 0
**File:** `internal/run/run.go:222`; `internal/researcher/gemini/gemini.go:350`  
**Spec clause:** PROPOSAL §4.5 (metrics list includes reconnect count)

`MetricReconnectCount` is recorded from `result.Usage.ReconnectCount`, which the gemini
adapter populates from `types.Usage.ReconnectCount`. The `toDomain()` mapping in
`gemini.go:349–360` never sets `ReconnectCount`:

```go
out.Usage = types.Usage{
    // ... no ReconnectCount field set
}
```

This is a pre-M5 gap — reconnect only becomes meaningful once the streaming path with
`last_event_id` resume is implemented. The metric is wired, recorded, and exported to
OTel and JSONL; it just always emits 0 for polling runs.

**Severity:** Medium — metric is emitted and counted in backends; it is structurally
correct but will misrepresent reconnects as 0 even if reconnect logic is added to the
adapter without also updating this field.

---

### Finding 3 — Low: `OutputThoughtSummary` domain type defined but never populated
**File:** `internal/types/types.go:73`; `internal/researcher/gemini/gemini.go:334–343`

`OutputThoughtSummary OutputType = "thought_summary"` is declared in the domain model
but `toDomain()` maps only `ContentImage` and `FinalText()` (the last text output).
Thought content parts (from `steps[].content[].type == "thought"`) are excluded by
`FinalText()` design and never mapped to `OutputThoughtSummary` outputs.

This is intentional for v1 polling runs, but the constant is unreachable code in the
current adapter — if a caller iterates `Interaction.Outputs` looking for
`OutputThoughtSummary` entries, it will never find them.

**Severity:** Low — no external contract breakage; the type exists for future use. Worth
noting before M5 to avoid a mismatch between the constant and the adapter.

---

### Finding 4 — Low: `go.mod` includes undeclared direct dependency `otel/trace`
**File:** `go.mod:12`  
**Spec clause:** DECISIONS.md "OpenTelemetry dependency set"

The DECISIONS.md OTel entry lists three direct modules: `go.opentelemetry.io/otel`,
`otel/sdk`, and `otel/exporters/otlp/otlptrace/otlptracehttp`. The `go.mod` includes
a fourth direct: `go.opentelemetry.io/otel/trace v1.44.0`, used in `otel.go` for
`oteltrace.Tracer` and `oteltrace.SpanFromContext`. This is a necessary import, not a
gap, but the DECISIONS.md entry does not explicitly list it.

**Severity:** Low — code is correct; DECISIONS.md entry should be updated to list
`otel/trace` as a justified direct dependency alongside the other three.

---

### Finding 5 — Low (informational): M4-scope stubs registered but inert
**Files:** `internal/cli/commands.go:67–110`; `internal/config/config.go:59` (`Plan`
field); `internal/cli/research.go:88–99` (no `cfg.Plan`, `cfg.BudgetGBP` read)

`research-config`, `get`, and `follow-up` commands return `errNotImplemented`. The
`--plan` and `--budget` flags are registered, parsed, and stored in `ResearchConfig`
but are not forwarded to any component in `runResearch()`. This is expected — all
four are M4 scope and explicitly out of this review — but noting for the remediation
backlog.

**Severity:** Low / informational — no spec violation within the M0–M3+M6+M7 scope.

---

## Verdict

**COMPLIANT** (for milestones M0–M3 + M6 + M7)

The implementation satisfies every normative requirement in scope. Wire behaviour
matches INTERACTIONS-API.md precisely — including the §8 drift from PROPOSAL §3 —
with correct endpoint paths, header pinning, create payload shape, explicit boolean
fields, full status enum, `requires_action` typed error, `steps[]`-based response
parsing, SSE event names, `last_event_id` query-param resume, and bounded reads.
The seam architecture is pure: the run core holds no Gemini types; all concrete
implementations are injected at the composition root. Every DECISIONS.md entry is
verifiably true of the code. The Markdown formatter, observability layer, secret
resolver/scrubber, and v2 interface stubs (proto, grpc transport, fleet researcher,
ContextStore no-op) meet their specs.

Two medium-severity gaps require attention before M5 ships:

1. **`cfg.Stream → ThinkingSummaries`** — the wiring between the `--stream` flag and
   the API `thinking_summaries` field is absent. M5 must add both the display layer
   and this API-request toggle; adding the display layer alone will not produce
   summaries to display.
2. **`reconnect_count`** — not populated by the adapter; recording a permanently-zero
   metric is misleading. Fix alongside the M5 reconnect implementation.

Two low-severity notes (dangling `OutputThoughtSummary` constant; undocumented fourth
OTel direct dependency) should be addressed in a documentation/cleanup pass.
