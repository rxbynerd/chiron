# Decision log

Running log of design and dependency decisions made while building Chiron,
so future sessions (human or agentic) can see *why* the code is shaped the
way it is rather than re-deriving or re-litigating it. Append new entries
at the end with a date and the deciding context; supersede old entries in
place with a note rather than deleting them.

---

## 2026-06-07 — ContextStore is declared locally, not imported from paddockapi

Chiron's `internal/memory` declares its own `ContextStore` interface and
supporting types, structurally matching the contract sketched in
`docs/PADDOCK.md` §4, rather than importing `paddockapi`. This follows the
recommendation in PADDOCK.md §8.4: a direct import would couple the two
projects' versioning, while a local interface keeps Chiron buildable
without Paddock, and Go's structural typing lets Paddock satisfy the seam
without either project depending on the other. v1 binds the `Noop` store
only — defining the seam and *not* implementing it is the deliberate
decision recorded in PROPOSAL.md §5.

## 2026-06-07 — Dependency set: cobra, pflag (transitive), yaml.v3

> **Extended** by "OpenTelemetry dependency set" below, which admits the
> OTel SDK as the one justified exception for PROPOSAL §4.5; the
> no-vendor-AI-SDK rule is unchanged.

- `github.com/spf13/cobra` — the sanctioned CLI framework, consistent with
  the rest of the suite.
- `github.com/spf13/pflag` — arrives transitively with cobra; used
  directly in `internal/config` for flag binding (no new module).
- `gopkg.in/yaml.v3` — config files and the YAML half of "JSON/YAML
  serialisable". One strict yaml.v3 decoder handles both forms, since JSON
  is a YAML subset; stdlib `encoding/json` handles emission.

No vendor AI SDKs, ever: the Gemini adapter (M1/M2) is hand-rolled
`net/http` + SSE per PROPOSAL.md §2, so every line is auditable.

## 2026-06-07 — Config merge semantics: explicit-flag overlay over a base

Resolution order is: documented defaults → base config (`--config` path,
`-` for stdin, or auto-detected piped stdin) → only the flags the user
explicitly set (via `pflag.FlagSet.Visit`). Flag *defaults* are never
applied over a base, so a piped `research-config` value survives later
pipeline stages that don't mention it. Decoding starts from the defaults,
so absent keys keep them; unknown keys are rejected (`KnownFields`)
because a silently misspelt key would silently change a paid run.
`--quiet` is sugar for `--stream=false`; the pair is marked mutually
exclusive at the command level.

## 2026-06-07 — Researcher is Start/Await/Result, not one Research call

PROPOSAL.md §6 names the contract explicitly ("the core still calls
start / await / result"). The split keeps the run core a deterministic
state machine with natural span boundaries for the tracer, and because
`Await`/`Result` accept a bare interaction ID, crash-resume
(`chiron get <id>`) and follow-ups fall out of the same surface with no
local state.

## 2026-06-07 — internal/types follows INTERACTIONS-API.md, not PROPOSAL §3

> **Superseded** the same day by "Wire types vs domain types" below:
> wire-exact shapes moved out of `internal/types` into
> `internal/interactions`. The premise stands — where the proposal and
> the API reference disagree, the reference wins — but it now binds the
> adapter, not the domain model.

The live Interactions API has drifted from the shapes the proposal was
verified against (2026-04-28 → 2026-05-20 revision), as documented in
`docs/INTERACTIONS-API.md` §8: responses are `steps[]` of typed
`content[]` parts (not `outputs[]`), citations are `annotations` on text
parts (not a response-level list), the status enum is wider
(`requires_action`, `cancelled`, `incomplete`, `budget_exceeded`), and
the usage block carries token totals plus `grounding_tool_count`.
`internal/types` models the **new** shapes with wire-exact JSON tags, and
a test pins the decode against the reference's §4 example. Where the
proposal and the API reference disagree, the reference wins.

## 2026-06-07 — Wire types vs domain types

`internal/types` is the seam-level **domain model**: the stable shapes
the core, formatter, and sinks program against (`Interaction` with
`Outputs`/`Citations`, the full status enum plus `Terminal()`, and a
flat `Usage` of cost signals). The **wire schema** — `steps[]`, typed
`content[]` parts, `annotations`, per docs/INTERACTIONS-API.md §4 — is
owned by `internal/interactions`, and `researcher/gemini` maps wire to
domain.

Rationale: the Interactions API is beta and actively drifting (see
INTERACTIONS-API.md §8 for the drift it has already exhibited), so the
churn is contained in the adapter behind a `last_verified` marker while
core/formatter/sink stay stable. This supersedes the entry above, which
had put wire-exact shapes directly into `internal/types`.

## 2026-06-07 — Durations serialise as strings

`config.Duration` wraps `time.Duration` to marshal as `"30m"` in JSON and
YAML rather than a nanosecond integer, since ResearchConfig is a
user-facing, hand-editable document. Both string and integer forms are
accepted on decode.

## 2026-06-07 — secret:// reference grammar

The first path segment of a `secret://` reference selects the backend; a
bare single segment defaults to `env`, so the documented
`secret://GEMINI_API_KEY` form keeps working:

- `secret://NAME` and `secret://env/NAME` — environment variable `NAME`.
- `secret://file/<path>` — contents of the file at `<path>`, with the
  leading slash *implied*: `secret://file/etc/chiron/key` and
  `secret://file//etc/chiron/key` both mean `/etc/chiron/key`. Relative
  paths are deliberately unsupported — a reference that resolves
  differently depending on the working directory is a misconfiguration
  hazard; use `env` for ad-hoc local values. Trailing newlines are
  trimmed (secrets files conventionally end with one); an empty file or
  unset/empty variable is an error, never an empty value.

Anything without the `secret://` prefix is rejected as a literal, and the
rejection error never echoes the offending value — it may itself be the
credential, and errors end up in logs. A v2 GCP Secret Manager backend
slots in as a new segment (`secret://gcp/...`) without grammar changes.

## 2026-06-07 — Markdown formatter performs no IO; the sink writes assets

> **Partially superseded**: the "no tool set in front matter"
> consequence below was reversed by "The domain Interaction carries the
> resolved tool set" further down. The no-IO split itself stands.

The markdown Formatter returns chart images as `Report.Assets` (name,
MIME type, bytes) and references them from the document as relative
links, rather than writing files itself. The `types.Report`/`types.Asset`
shapes already encode this split, and it keeps the Formatter pure —
trivially testable against golden files, reusable by any ReportSink
(stdout-json embeds the assets; the file sink writes them next to the
report). Asset names are deterministic (`chart-N.<ext>`) with a
hand-rolled MIME→extension table, because `mime.ExtensionsByType`
consults platform databases and would make output non-reproducible
across machines. Front matter is emitted with `gopkg.in/yaml.v3` — an
existing dependency (see the dependency-set entry above), so no new
module is introduced.

Two consequences of formatting the DOMAIN model rather than the wire
shape: the front matter carries no tool set (the domain `Usage`
deliberately reduces grounding detail to `search_count`; the requested
tool list is a wire-level concern recorded in config, not the
interaction), and citations arrive already deduplicated from the
adapter, so the formatter just numbers them. Interactions with no final
text output (failed, cancelled, still in progress) still render a
complete document — front matter with status and `status_detail`, plus a
placeholder body — so `chiron get` of an unfinished or broken run never
produces nothing.

## 2026-06-07 — NDJSON run events go to stderr, not stdout

The stdio transport (`internal/transport.Stdio`) defaults to **stderr**.
stdout belongs to the report: the `stdout-markdown` and `stdout-json`
sinks write the run's primary artefact there, and
`chiron research --query ... | tee report.md` must produce a clean
document. Interleaving machine-readable lifecycle events into that
stream would corrupt a piped report, whereas stderr keeps progress
visible in a terminal and separable in a pipeline (`2>events.ndjson`).
This mirrors the wider Unix convention: data on stdout, diagnostics and
progress on stderr. The writer is injectable (`NewStdio(w)`), so a
future flag can redirect events to a file without a new transport.

Each event is one JSON line; a zero `time` is stamped by the transport
so consumers can always order events. Event kinds
(`internal/transport/events.go`) mirror the `RunEvent` payload kinds in
`proto/chiron/v1/chiron.proto` one-to-one, so the v1 NDJSON stream and
the v2 control-plane stream describe the same lifecycle.

## 2026-06-07 — proto/chiron/v1 is committed; generated Go is not (in v1)

The v2 control-plane contract lives in `proto/chiron/v1/chiron.proto`
as a Buf v2 module (`proto/buf.yaml`, `proto/buf.gen.yaml`), shaped for
the outbound-dial pattern of PROPOSAL.md §6: the runner dials
`ControlPlaneService` and holds one bidirectional `Session` stream
(hello → research requests down, run events and responses up).

Generation was verified working at the time of this entry: `just proto`
(`buf generate` with remote plugins pinned to
`buf.build/protocolbuffers/go:v1.36.6` and `buf.build/grpc/go:v1.5.1`)
produces `chiron.pb.go` and `chiron_grpc.pb.go`, and the output compiles
against `google.golang.org/protobuf` v1.36.11 and
`google.golang.org/grpc` v1.81.1. The generated code is nevertheless
**not committed**, because no v1 code path imports it: committing it
would pull the protobuf runtime and the full grpc-go dependency tree
into `go.mod` for dormant code, against the minimal-and-auditable
dependency ground rule (AGENTS.md). The v2 wave that first binds the
gRPC transport runs `just proto`, commits the output, and justifies the
runtime dependencies here in the same change. Note that remote plugins
need network access; an air-gapped build can install
`protoc-gen-go`/`protoc-gen-go-grpc` locally at the same versions and
swap `remote:` for `local:` in `buf.gen.yaml`.

## 2026-06-07 — OpenTelemetry dependency set, and metrics as span attributes

PROPOSAL §4.5 mandates OTLP emission to the suite's Langfuse/Grafana
backend, so the official SDK is the justified exception to the
hand-rolled-`net/http` rule (it is an observability SDK, not a vendor AI
SDK). Direct modules, all at the current stable release:

- `go.opentelemetry.io/otel` v1.44.0 — API (`attribute`, `codes`,
  `trace`).
- `go.opentelemetry.io/otel/sdk` v1.44.0 — `TracerProvider`, batch span
  processor, resource; also `sdk/trace/tracetest` for the scrub tests.
- `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp`
  v1.44.0 — OTLP over HTTP/protobuf.
- `go.opentelemetry.io/otel/trace` v1.44.0 — `oteltrace.Tracer` and
  `oteltrace.SpanFromContext` in `otel.go`. (Added to this entry during
  cycle-1 remediation, C1-SPEC-2: the module was always a direct
  dependency; the log simply failed to list it.)

The HTTP exporter was chosen over the gRPC one to keep the linked tree
smaller, but honesty compels a caveat: `go.opentelemetry.io/proto/otlp`
(v1.10.0) ships its collector stubs with gRPC service code and
grpc-gateway HTTP bindings in the same packages, so
`google.golang.org/grpc` v1.81.1, `grpc-gateway/v2` v2.29.0, and
`google.golang.org/protobuf` v1.36.11 are *linked*, not merely present
in the module graph, even on the HTTP path. This is the floor for any
conformant OTLP exporter today. (It also means the grpc/protobuf tree
the proto entry above kept out of `go.mod` has arrived via
observability; that entry's rationale — don't commit dormant generated
code — still stands.)

Metrics (`metric.<name>` attributes on the span in context) deliberately
bypass the OTel *metrics* SDK: Langfuse is trace-oriented, the §4.5
metrics are per-run rather than fleet-aggregated, and skipping
`sdk/metric` plus an OTLP metrics exporter keeps the tree at the floor
described above. `internal/trace/names.go` fixes the span and metric
name vocabulary so backends can rely on it. The JSONL binding writes the
same spans and metrics as newline-delimited JSON for local debugging;
both bindings route every string payload through `secret.Scrub`, so a
credential cannot transit traces any more than logs.

## 2026-06-07 — internal/interactions: client shape, retries, bounds, SSE resume

> **Partially superseded**: the polling bullet's treatment of
> `requires_action` as plain terminal was replaced by "requires_action
> returns a typed error" below, and the Create-retry trade-off was
> tightened for the spending callers by "Create is never retried in the
> gemini adapter". The remaining bullets stand.

The Interactions client is stdlib-only `net/http` + hand-rolled SSE — no
new dependencies. Decisions of note:

- The package owns the wire-format types per the "Wire types vs domain
  types" entry above, with a `LastVerified` constant (2026-06-07)
  recording the docs revision the shapes were verified against, beside
  the pinned `Api-Revision: 2026-05-20` header on every request.
- Retries cover transport errors and HTTP 408/429/5xx only, with capped
  exponential backoff; other 4xx surface immediately as a typed
  `*APIError` (URI code, message, HTTP status). Create is retried like
  everything else — INTERACTIONS-API.md §6 says to — which accepts a
  small duplicate-spend risk if a 5xx lands after the server actually
  accepted a create. v1 takes that trade because interaction IDs surface
  as soon as they are known and the budget levers arrive in M4; if it
  bites, `WithMaxRetries(0)` exists.
- `Create` rejects `background: true` without `store: true` locally
  (§3's rule) rather than burning a paid request to learn it, and
  `CreateRequest` emits `background`/`store`/`stream` explicitly — never
  `omitempty` — because their server-side defaults differ.
- Every body read is bounded via `io.LimitReader` (64 MiB responses,
  16 MiB per SSE event, 1 MiB error bodies, all configurable): a
  misbehaving endpoint cannot exhaust memory, and an over-bound document
  fails loudly instead of truncating silently.
- Polling treats every non-`in_progress` status as terminal (full §4
  enum), so `requires_action` or `budget_exceeded` stop the poll rather
  than hanging it; failure-variant mapping is the caller's concern via
  `Status.Failed`. Defaults back off 10s → 60s ×1.5 against tasks that
  run minutes (§7).
- SSE resume is via `?last_event_id=` query parameter, not the
  `Last-Event-ID` header, which the docs do not promise is honoured
  (§5). The streamer is a parsing primitive: `Next`/`LastEventID`/
  `Close` only, with reconnect policy left to the caller (M5). Because
  the reference does not pin down whether `event_type`/`event_id` travel
  as SSE fields or inside the JSON payload, the parser accepts both,
  preferring the SSE fields; unknown event and delta types are surfaced
  with their raw payload rather than dropped.


## 2026-06-07 — requires_action returns a typed error, not plain terminal

> Supersedes one bullet of the interactions-client entry above: the
> poller no longer treats `requires_action` as an ordinary terminal
> status.

`internal/interactions.Status.Terminal()` now matches
`types.Status.Terminal()` exactly — everything except `in_progress` and
`requires_action` — so the wire and domain enums share both strings and
semantics, keeping the adapter's status mapping a plain string
conversion. Because deep research supports no custom function tools, an
interaction that reports `requires_action` is in a state Chiron cannot
service; `PollUntilTerminal` returns the snapshot with a typed
`ErrRequiresAction` immediately rather than hanging until the server's
60-minute cap or pretending the run concluded. The wire `Interaction`
also gained `Citations()` — url_citation and file_citation annotations
deduplicated by URI in first-seen order — shaped to map directly onto
the domain `types.Citation{URI, Title}`.

## 2026-06-07 — Create is never retried in the gemini adapter

`internal/interactions` retries transient failures (408/429/5xx) by
default, and its own decision entry accepted the duplicate-spend risk on
`Create` "if it bites, `WithMaxRetries(0)` exists". For the adapter that
actually spends the money it bites pre-emptively: a 5xx on
`POST /interactions` is ambiguous — the server may have accepted, and
started billing, a research task costing £1–7 (INTERACTIONS-API.md §7)
before failing to say so. `researcher/gemini` therefore holds two
clients over one `http.Client`: the create client with
`WithMaxRetries(0)`, so a failed create surfaces to the user who retries
knowingly, and the poll client with the default retries, because GET is
idempotent. A test pins "exactly one create attempt" on a 502.

## 2026-06-07 — Estimated cost is a tier-table planning figure

`Usage.EstimatedCostGBP` is derived in `researcher/gemini/cost.go` from
the per-tier USD envelope Google publishes (INTERACTIONS-API.md §7:
deep-research $1.00–$3.00, deep-research-max $3.00–$7.00): the midpoint
of the envelope converted at a fixed planning rate of 0.79 USD→GBP,
chosen because it reproduces the £0.80–£5.50 envelope PROPOSAL §2 quotes
for the same range. It is a pre-run planning figure for the report front
matter and the M4 `--budget` lever — not billing. Real pricing, live FX
and attribution are Stint's (a suite concern, PROPOSAL §2); Chiron holds
no pricing tables beyond this constant pair.

## 2026-06-07 — The domain Interaction carries the resolved tool set

> Supersedes part of the formatter entry above ("the front matter
> carries no tool set"): PROPOSAL §4.4 lists the tool set among the
> front-matter fields, and the gap is now closed.

The API does not echo the requested tools back on the Interaction
resource, so `types.Interaction` gained `Tools []string`, populated by
the gemini adapter from the create request it actually sent — which is
also why the adapter emits the default tool trio explicitly rather than
relying on server-side defaults: the recorded set must be the used set
even if a beta API's defaults drift. Entries are wire tool-type names,
with MCP servers disambiguated as `mcp_server:<name>`. The formatter
renders the list (flow style) between the estimated cost and the
sources, matching §4.4's field order.

## 2026-06-07 — Run-core lifecycle: emit errors tolerated, sink errors fatal

`internal/run` treats the two output seams asymmetrically, on money
grounds. Transport emission is best effort: once a paid task is in
flight, a broken event stream (closed stderr, dead control-plane
connection) must not abort the run — failures are recorded on the root
span and the loop continues, because the report is the artefact and
events are observability. A sink failure, by contrast, fails the run:
a report that did not land where it was asked to is a failed emit, and
the already-emitted interaction id still allows recovery. For the same
reason the id is emitted the moment `Start` returns, before anything
else can fail, and failure-variant statuses (failed, cancelled,
budget_exceeded, incomplete) conclude the run normally — placeholder
report, cost summary, RunResult — with the outcome carried by the exit
code, not an error.

## 2026-06-07 — CLI exit codes and composition-root boundaries

> **Extended** by "Client-side budget gate" below, which adds exit 4
> (blocked client-side before any spend) to the contract described
> here.

`chiron research` exits 0 only when the task completed; 1 for usage,
configuration and infrastructure errors (including timeout — the run
could not conclude); 2 when the task ended `failed` or `incomplete`, or
reported `requires_action` — a research outcome, not an infrastructure
fault: deep research cannot legitimately request client action, so the
typed `interactions.ErrRequiresAction` maps to the failed-run code with
its detail in the error; 3 when it was `cancelled` or `budget_exceeded`
(stopped, not broken).
The contract is documented in the command's help so pipelines can act
on the outcome without parsing the report. Environment access lives
only in the CLI composition root: the OTel tracer binds only when the
standard `OTEL_EXPORTER_OTLP_*` variables name an endpoint (otherwise
the no-op tracer — an exporter with nowhere to send spans would buffer
and drop them), and `CHIRON_GEMINI_BASE_URL` overrides the API endpoint
so the smoke tests prove the research path end-to-end against httptest
without real network. The run core itself takes seams and a context
only.

## 2026-06-07 — Client-side budget gate: exit 4 means blocked before spend

`--budget` is enforced in the CLI before any create, by comparing the
tier's planning estimate — the existing constant pair in
`researcher/gemini/cost.go`, exposed as `EstimatedCostGBP()`; no second
cost table exists — against the cap. A blocked run exits **4**
(`ExitBlocked`), reporting estimate vs cap, and the same code covers a
declined `--plan` review: both mean "Chiron stopped the run client-side
before any research spend", which is deliberately distinct from exit 3
(a *running* task stopped server-side, `cancelled`/`budget_exceeded`).
The gate sits ahead of the plan phase too, because plan rounds also
spend. Follow-up Q&A passes the gate trivially — its estimate is zero
(see the follow-up entry below) — and `chiron get` never gates: it
creates nothing.

The plan refine loop is bounded structurally rather than monetarily
(`planner.DefaultMaxRounds`, five rounds including the proposal, the
counter rendered with every plan): the API publishes per-run envelopes
only (INTERACTIONS-API.md §7), no per-round planning price, and
inventing one to divide into the budget would be a fabricated figure
masquerading as a control.

## 2026-06-07 — Plan-phase creates are never auto-retried either

Extends "Create is never retried in the gemini adapter": the planner
binding (`gemini.NewPlanner`) holds the same client split — creates
with `WithMaxRetries(0)`, polls with the defaults. A plan round is a
paid create like any other, and an ambiguous 5xx on
`POST /interactions` may already have paid for the round; a test pins
exactly one create attempt on a 502. The planner seam itself
(`internal/planner`) covers propose and refine only: approval is
deliberately not on the seam, because an approved plan is an ordinary
research run — a create chained by `previous_interaction_id` with
`collaborative_planning` off — so the run core gained no planning mode,
just a `PreviousInteractionID` passthrough. The interactive session
renders plans and prompts on **stderr** (stdout belongs to the report),
and a non-terminal stdin can never approve spend: `--plan` aborts with
guidance unless `--accept-plan` explicitly approves the first plan
unattended, which still leaves the plan in the stored interaction chain
as an audit trail.

## 2026-06-07 — Follow-up Q&A: gemini-3.1-pro-preview by default, estimate zero

`chiron follow-up` creates with `model` + `previous_interaction_id` —
not `agent` — per INTERACTIONS-API.md §3, with no `agent_config` and no
`tools`, and the query travels **verbatim**: a follow-up questions an
existing report, so the research report template does not apply. The
default model is `gemini-3.1-pro-preview` because it is the model the
API reference itself names for follow-ups against the pinned
`Api-Revision: 2026-05-20`; it is overridable via `--model`/`model` in
config, so a cheaper or newer model needs no code change. `background`
and `store` stay true so the one poll-and-resume machinery serves
follow-ups too. The cost estimate in follow-up mode is **zero**:
model-priced Q&A sits outside the per-tier research envelope, and a
fabricated figure would pollute both the report front matter and the
budget gate. Actual token usage is still reported in the cost summary.

## 2026-06-07 — Resumed interactions claim no tool set; estimate from the wire agent

`chiron get` resumes interactions this adapter never started. The API
does not echo the create request's tool set back (the reason
`types.Interaction.Tools` is adapter-populated — see "The domain
Interaction carries the resolved tool set"), so for resumed
interactions the adapter records **no** tools rather than guessing the
default trio: the recorded set must be the used set, and absent is
honest where invented is not. Same for the query. The planning estimate
falls back to the interaction's own wire agent id
(`estimateForAgentID`), so resuming a max-tier run reports the max-tier
figure regardless of the locally configured tier; unknown agents (and
model-based follow-ups) estimate zero.

## 2026-06-07 — Cycle-1 remediation: base-URL validation, redirect policy, stdin posture

Three security-posture decisions from the cycle-1 review remediation
(findings in `docs/reviews/cycle-1-brief.md`; dispositions in
`docs/reviews/cycle-1-remediation.md`):

- **`CHIRON_GEMINI_BASE_URL` is validated before any client exists**
  (C1-SEC-1): the API key travels in a header on every request to this
  base, so an unvalidated override is a key-exfiltration and SSRF
  channel. The rule is https:// anywhere, http:// for loopback hosts
  only. The loopback exemption deviates from the brief's https-only
  recommendation deliberately: the CLI smoke tests drive the binary end
  to end against plain-HTTP `httptest` servers and the adapter exposes
  no TLS-trust injection point, while loopback http reaches neither a
  network path nor an internal metadata service — the threats named by
  the finding. The variable is documented as security-sensitive in
  AGENTS.md.
- **The interactions client refuses cross-host redirects** (C1-SEC-2):
  Go strips only its own sensitive headers on cross-domain redirects,
  never custom ones like `x-goog-api-key`. The policy is enforced on a
  shallow copy of whatever `http.Client` is supplied, so
  `WithHTTPClient` cannot lose the guarantee.
- **Unknown stdin reader types are neither config nor a terminal**
  (C1-CODE-4): `stdinIsPiped` previously treated any non-`*os.File`
  reader as piped config, so an embedding host's live reader would be
  drained and decoded as YAML. Now only a real pipe/file is implicit
  config, and the separate `stdinIsTerminal` — deliberately not the
  negation — keeps the earlier decision that a non-terminal stdin can
  never approve `--plan` spend.

## 2026-06-07 — Streaming reconnect policy and the poll fallback

The SSE streamer in `internal/interactions` is deliberately a parsing
primitive; the reconnect POLICY lives in the gemini adapter
(`researcher/gemini/stream.go`):

- Resume is via the `?last_event_id=` query parameter with the last
  seen event id, never the `Last-Event-ID` header, which the docs do
  not promise is honoured (INTERACTIONS-API.md §5). Reconnects back
  off 500ms doubling to an 8s cap, mirroring the client's retry
  backoff.
- A per-await failure budget of four counts dial failures, broken
  streams, clean EOFs without a terminal state, and server `error`
  events — and is deliberately never reset by progress: a stream
  alternating deltas with errors would otherwise hold the await
  captive forever. A long task that genuinely drops more than four
  times concludes less prettily but just as correctly by poll.
- When the budget is spent, `Await` FALLS BACK TO POLLING rather than
  failing the run: the task is still running — and still spending —
  server-side, and abandoning a £1–7 interaction over a broken event
  channel would waste the spend; the stream's error is absorbed, and
  if the API is truly unreachable the poll's own failure surfaces.
  `requires_action` and context cancellation never fall back, because
  polling would not change either answer.
- Each re-dial after the initial attach increments
  `Usage.ReconnectCount`, so the §4.5 reconnect_count metric is real
  (cycle-1 finding C1-M5-2). `interaction.completed` may omit content,
  so the await takes only the status from stream events and the run
  core's Result re-GETs the full resource (§5).

The display surface is the transport, not a bespoke renderer: thought
summaries arrive as `delta` NDJSON events on stderr beside the rest of
the lifecycle (stdout belongs to the report), keeping the v1 stream
and the v2 control-plane stream identical in shape. cfg.Stream also
toggles the request itself — thinking_summaries "auto" when streaming,
"none" under --quiet (C1-M5-1) — and wire thought content parts now
map to OutputThoughtSummary domain outputs, so --output json carries
the reasoning trail that streamed past.

## 2026-06-07 — v1 complete

All eight milestones (PROPOSAL §9, M0–M7) are implemented, and two full
review/remediation cycles have run to a clean close:

- **Cycle 1** (post-M3/M6/M7, brief at `docs/reviews/cycle-1-brief.md`):
  21 findings after deduplication — 3 High, 4 Medium, 10 Low, 2 Info,
  2 deferred to M5 by the brief itself. Every finding dispositioned in
  `docs/reviews/cycle-1-remediation.md`; the deferred pair was closed by
  the M5 streaming work.
- **Cycle 2** (full v1, brief at `docs/reviews/cycle-2-brief.md`):
  27 findings — 1 Critical, 5 High, 7 Medium, 14 Low — predominantly
  test-coverage work; production code arrived with no Critical/High/
  Medium code findings. Every finding dispositioned in
  `docs/reviews/cycle-2-remediation.md`, including one reviewer
  assertion refuted by test (C2-TEST-3).

At the closing commit, `go build ./...`, `go vet ./...` and
`go test -count=1 ./...` pass; total statement coverage is **89.5%**
(`go tool cover -func`), with the untested remainder concentrated in
`cmd/chiron` (a two-line main) and the deliberately inert
`internal/memory` no-op. The v2 seams sit ready as designed: the gRPC
transport stub, the fleet researcher stub, `proto/chiron/v1`, and the
locally declared `ContextStore` awaiting Paddock.

## 2026-06-07 — Lint contract committed: .golangci.yml plus pinned linter version

The first push to main after the v1 close failed CI lint with 28
errcheck findings — none of them new code. The workflow ran
golangci-lint with `version: latest` and no committed configuration;
golangci-lint v2 removed the v1 default exclusions, so a release of the
linter changed what main was held to without any change in this
repository. Triage confirmed every finding was an idiomatic ignore
(deferred `Close` on read paths, `fmt.Fprint*` to the CLI writer,
httptest handler writes in `_test.go`) rather than a real defect.

Remediation makes the lint contract explicit instead of inherited:

- `.golangci.yml` (v2 schema) enables the `std-error-handling`
  exclusion preset, restoring the standard errcheck suppressions, and
  relaxes errcheck in `_test.go` files where unchecked handler writes
  are conventional.
- The workflow pins `version: v2.12.2` for the same reason the action
  SHAs are pinned (C1-SEC-3): main must not break without a commit in
  this repository. Linter bumps are now deliberate and travel with any
  config adjustment they require.

Peppering the 28 sites with `_ =` assignments was rejected — it adds
noise at call sites the Go ecosystem conventionally leaves bare, and
the next linter default change would simply produce a different batch.

## 2026-07-01 — Config surface for the in-process research agents (worker/fleet)

V2-RESEARCH-AGENT §4 extends `ResearchConfig.Agent` with two Chiron-owned
in-process agents beside the Gemini Deep Research stopgap: `worker` (a
single search → read → synthesise loop) and `fleet` (a lead orchestrator
over bounded workers). This chunk adds the config and CLI surface only; the
researchers themselves are wired in a later chunk, so the composition root
recognises the two values and returns a clear typed "not yet wired" error
rather than a nil researcher or a silent Gemini fallback. No new dependency
is introduced.

The Wave 3/4 knobs live in a nested `Fleet FleetConfig` block rather than
being flattened onto `ResearchConfig`, so the deep-research paths ignore
them wholesale and a zero `Fleet` never invalidates a deep-research run.
`Fleet.validate` runs only when `Agent` is `worker`/`fleet`. Fields:

- Standard model: `ModelEndpoint` (URL), `ModelName`, `ModelKeyRef`
  (`secret://`). `ModelName` is deliberately distinct from the pre-existing
  `ResearchConfig.Model`, which selects the Gemini *follow-up* model
  (docs/INTERACTIONS-API.md §3) — conflating them would couple two
  unrelated model choices.
- Search MCP: `SearchEndpoint` (URL), `SearchKeyRef` (`secret://`).
- Worker caps: `MaxTurns` (int, positive — the primary runaway guard),
  `MaxTokens` (int) and `CeilingGBP` (float) as the spend ceiling,
  `WorkerTimeout` (the config `Duration`).
- Fleet caps: `MaxWorkers` and `Concurrency` (both positive; concurrency
  must not exceed max-workers), enforced for `fleet` only — a lone worker
  has no fan-out to bound.
- `Memory`: `noop` | `inmemory` (default `noop`); `paddock-embedded` waits
  for Wave 6.

Both a token ceiling *and* a GBP ceiling are kept rather than picking one.
`MaxTokens` bounds a single worker loop deterministically in tests with no
price table (the CI fakes have no cost), while `CeilingGBP` expresses the
operator's spend intent in the same GBP unit and zero-means-uncapped
semantics as the existing `BudgetGBP`. Both default to zero (uncapped on
that dimension); the turn and time caps still bound the loop, so a bare
`--agent worker` run is never unbounded. Defaults: `MaxTurns` 8,
`WorkerTimeout` 5m, `MaxWorkers` 5, `Concurrency` 3, `Memory` noop.

Endpoint overrides are validated with the exact `CHIRON_GEMINI_BASE_URL`
rule (C1-SEC-1): an absolute `https://` URL, `http://` admitted for
loopback hosts only. Credentials travel to whatever endpoint is
configured, so an unvalidated override is a key-exfiltration and SSRF
channel (CWE-918, CWE-319). The scheme check is mirrored in
`internal/config` rather than shared with the `internal/cli` copy, because
`internal/cli` imports `internal/config` and the reverse import would
cycle; both are kept in step by comment. Key references, when set, must be
`secret://` and the "literal key" error never echoes the value, matching
the `api_key_ref` rule. Endpoint and key fields are optional at the config
layer (a later wave resolves and requires them); the caps are validated
eagerly.

Wave 3 prefers config fields to new environment variables for the model
and search overrides — the endpoints are per-run research configuration,
not process-wide test hooks like `CHIRON_GEMINI_BASE_URL`. No new
security-sensitive env var is added; `AGENTS.md` records the `Fleet`
endpoint/key fields as security-sensitive configuration on the same
rationale (credentials are sent to the configured endpoint).

## 2026-07-01 — Standard-model adapter: OpenAI-compatible Chat Completions

Wave 3 needs a model substrate for the in-process research lead and workers
(docs/V2-RESEARCH-AGENT §5). `internal/researcher/fleet/model` is a small,
hand-rolled `net/http` client for one standard frontier model. No vendor AI
SDK; standard library `net/http` + `encoding/json` only. No new dependency.

**Wire choice: minimal OpenAI-compatible Chat Completions, not a full
Responses adapter.** The adapter targets the bare `POST /chat/completions`
request/response: a `model`, a `messages` array of `{role, content}`, an
optional `max_tokens`, and an optional `response_format` for structured
output; the reply is read from `choices[0].message.content`,
`choices[0].finish_reason`, and `usage`. This satisfies the "one standard
frontier model" deliverable while staying inside the V2-RESEARCH-AGENT
non-negotiable "no full OpenAI Responses adapter in Chiron": the Responses
API's item/output/tool-call surface, streaming event taxonomy, and
stateful conversation objects are all absent. All wire structs are
unexported and internal to the package; callers see only `Request`,
`Response`, `Usage`, `Message`/`Role`, and `Generate`. Adding provider
surface beyond what the lead/worker need would re-open this decision.

**Base-URL/path convention.** `Options.Endpoint` is a base URL (e.g.
`https://api.openai.com/v1`); the client appends `/chat/completions`. This
mirrors the Gemini adapter's `BaseURL` treatment and lets tests point at an
`httptest.Server` root. Auth is `Authorization: Bearer <key>` header only —
never a URL, query, log, error, or trace. `Options.APIKey` is the
already-resolved literal value; this adapter never dereferences `secret://`
(resolution stays at the CLI composition root), and it does not import
`internal/secret` for resolution — only `secret.Scrub` for diagnostics.

**Structured output is provider-native, via a `Request.JSONSchema` field.**
Setting it (with `SchemaName`) emits `response_format: {type: json_schema,
json_schema: {name, schema, strict: true}}`; `Response.Content` is then the
JSON string the model returns, which the caller parses. A single `Generate`
method covers both text and structured paths so the hardening lives in one
place; the lead's decompose/cite calls set the field, worker
reasoning/synthesis calls leave it nil.

**No auto-retry of the paid POST.** An ambiguous 5xx may already have billed
a model turn, so `Generate` makes exactly one attempt — the same reasoning
the Gemini adapter applies to `POST /interactions`. This adapter only POSTs,
so there is no retry path at all (contrast the idempotent GETs in
`internal/interactions`, which do retry). A test asserts request count == 1
on a 5xx.

**Duplicated hardening is accepted pending a shared helper.** Three pieces
of security logic are reimplemented here rather than shared:
`allowedEndpointScheme` (absolute `https://`, `http://` loopback only) is
copied from `internal/config` / `internal/cli` — the C3A precedent for the
endpoint validator — because this seam cannot import the CLI/config layer
without inverting the dependency; and `refuseCrossHostRedirects` +
`readBounded` are reimplemented from `internal/interactions`, where they are
unexported. Config already validates the endpoint, but the adapter
re-validates in `New` because it is a reusable seam that must not trust its
caller to have checked. Extracting a shared `internal/httpx` (or similar)
helper for redirect policy, bounded reads, and the loopback scheme rule is
deferred; when a third consumer lands it should be revisited.

**Credential-scrub belt-and-braces.** The key is only ever in the
`Authorization` header, so Chiron never puts it in a request body or URL.
But a provider *error body* (a 401 in particular) can echo the submitted
key back, and `secret.Scrub`'s high-entropy backstop is heuristic — an
`sk-`-style key with structured segments can fall below its entropy bar. So
diagnostics are scrubbed by exact match against the client's own key first,
then through `secret.Scrub` for any other credential shape. A test feeds a
401 body containing the key and asserts it is absent from the returned
error.

**Test fake shipped as exported package code, not a `_test.go` helper.**
`FakeServer` (an `httptest.Server`-backed fake with scripted replies and
request recording) lives in `fake.go` — non-test build — so the later
worker and lead chunks can reuse one fake model transport for CI
(docs/V2-RESEARCH-AGENT §8 requires a shared fake). The cost is that
`net/http/httptest` becomes an import of the package's normal build; this is
the deliberate trade for cross-chunk reuse of a single scripted transport,
and the package is a test-support-heavy adapter. Callers own the fake's
lifecycle (build at the call site, `defer Close`).

## 2026-07-01 — Search MCP client and its assumed tool-result shape

Wave 3 needs the first of the worker's two read-only network tools
(docs/V2-RESEARCH-AGENT §5): `internal/researcher/fleet/search`, a
hand-rolled `net/http` client for a web-search tool exposed over MCP
(JSON-RPC 2.0 over "Streamable HTTP"). No vendor SDK; standard library
`net/http` + `encoding/json` + `bufio` (for the SSE path) only. No new
dependency.

**Minimal flow, not a general MCP client.** The client implements only
`initialize` → `notifications/initialized` → `tools/call` for one configured
search tool. It captures any `Mcp-Session-Id` from the initialize response
and echoes it on the following requests. There is no resources/prompts/
sampling surface, no server-initiated request handling, and no session
resumption beyond echoing the id — adding any of that would re-open this
decision. `initialize`/`initialized`/`tools/call` are each single-attempt:
`tools/call` may drive a billable upstream search, so it is not auto-retried
(the same reasoning the model adapter applies to its paid POST); the cheap
handshake calls are not retried either, keeping the flow's cost ceiling
obvious.

**Two reply framings, both bounded.** A Streamable-HTTP `tools/call` POST may
return `application/json` (one JSON-RPC message) or `text/event-stream` (SSE
frames carrying JSON-RPC messages). Both are handled: the JSON path is the
common case; the SSE path is read with a minimally reimplemented reader that
mirrors the line-oriented scan / `data:` accumulation / blank-line dispatch
discipline of `internal/interactions.Stream` but only extracts the single
response frame (this transport needs one reply, not a reconnecting feed).
Every read — JSON body and SSE aggregate — is bounded by `MaxBodyBytes`; an
oversize reply is an error, not a silent truncation (contrast the fetch
client below, where truncation is acceptable).

**Assumed tool-result shape.** The search tool's `tools/call` result is
assumed to carry a JSON document shaped
`{"results":[{"title","url","snippet"}, ...]}`, either as `structuredContent`
or serialised inside a `text` content block. The client prefers
`structuredContent`, then scans text blocks. **Graceful degradation:** if
neither carries a recognised results document, the first non-empty text
content block is surfaced as a single `Result{Snippet: ...}` rather than
erroring, so a differently-shaped-but-useful reply still feeds the worker
something. An empty `results` array is treated as a valid zero-hit success
(distinguished from "no results key" so arbitrary JSON is not mistaken for a
zero-hit reply); `isError: true` on the tool result is surfaced as an error.
Downstream (worker/lead) code must respect this shape and this
degrade-don't-error posture.

**Security mirrors the sibling adapters.** The endpoint is validated in `New`
(absolute `https://`, `http://` loopback only) defensively even though config
validates it too, because the seam must not trust its caller; the search key
travels only in the `Authorization: Bearer` header and is scrubbed from every
diagnostic by exact match plus `secret.Scrub`; cross-host redirects carrying
the credential are refused. `allowedEndpointScheme`, `refuseCrossHostRedirects`
and `readBounded` are reimplemented here, the same accepted duplication noted
for the model adapter pending a shared `internal/httpx` helper.

**Fake shipped in `fake.go`, not `_test.go`.** As with the model adapter, an
`httptest.Server`-backed `FakeServer` lives in the non-test build so the later
worker/lead chunks reuse one fake search MCP for CI (docs/V2-RESEARCH-AGENT
§8). It scripts results, records requests, exposes total and `tools/call`-only
call counts, and has options for an assigned session id, an SSE reply framing,
and a verbatim tool result (for unexpected-shape / oversize cases). Callers
own its lifecycle.

## 2026-07-01 — web_fetch client: SSRF posture and oversize truncation

Wave 3's second read-only worker tool (docs/V2-RESEARCH-AGENT §5) is
`internal/researcher/fleet/fetch`, a hand-rolled `net/http` client that
retrieves the content of an UNTRUSTED external URL discovered by the search
tool. No vendor SDK; `net/http` only. No new dependency. This is the first
Chiron component to fetch arbitrary URLs, so the security posture is recorded
in full.

**SSRF guard (CWE-918).** Unlike the model and search clients — which POST a
credential to one configured, validated endpoint — web_fetch dials hosts
chosen by an upstream search result and carries NO Chiron credential, so the
dominant risk is server-side request forgery. Only `http`/`https` schemes are
accepted (`file:`, `ftp:`, `data:`, `javascript:`, `gopher:`, ... are
rejected). Before the request and again after every redirect, the destination
host is resolved and refused if any resolved address is loopback, private
(RFC1918 / RFC4193), link-local (unicast or multicast, covering 169.254/16 and
fe80::/10), or unspecified — refusing on the union of resolved addresses is
the safe default. Re-checking on redirect is essential: a public URL that
302s to `http://169.254.169.254/…` must be caught.

**`AllowLoopback` narrows, it does not blanket-disable.** The `AllowLoopback`
option (default false; tests set it true to reach loopback `httptest` servers)
exempts *only* loopback and the unspecified address. It deliberately does NOT
relax the refusal of private or link-local addresses, so a loopback test
harness — or a loopback page that redirects onward — still cannot reach an
internal production or cloud-metadata address. Production configuration leaves
it false.

**Connection-time IP pinning was a deliberate follow-up here (superseded
2026-09-23).** As first built, the guard was resolve-then-check: it did not
pin the checked IP for the actual dial, so between the guard's `LookupIP` and
the connection the name could re-resolve to a different address (DNS
rebinding), and a redirect target was re-checked but likewise not pinned. The
2026-09-23 entry "web_fetch: connection-time SSRF pinning and the
refused-destination error" closes that race with a guarded `DialContext` and
records the current guard.

**Oversize body is truncated-and-marked, not an error.** `MaxContentBytes`
bounds the body read. A source exceeding it yields `Page.Content` = the
bounded prefix with `Page.Truncated = true`, rather than the error the model
and search adapters return on oversize. The justification: those adapters read
a structured message whose clipped form is meaningless or a smuggling risk,
whereas a partial page is still useful research text for the worker to reason
over, and truncation is flagged so the caller knows it is a prefix. A redirect
depth cap and a per-call `RequestTimeout` (yielding to a tighter caller
deadline) bound the call otherwise.

**No credentials, but userinfo is still scrubbed.** web_fetch sends no Chiron
key. But a fetched URL may carry userinfo (`user:pass@host`); that is stripped
from the returned `Page.URL` and from every diagnostic (parse-and-restrip,
with a coarse fallback for an unparseable URL) so an embedded credential
cannot leak through a log, citation, or error. Reads are idempotent, so a
caller MAY retry a failed fetch knowingly, but the client itself does not
retry — keeping one call's cost and time ceiling obvious.

## 2026-07-01 — In-process worker loop: action schema, Wave 4 factoring, cost signal

Wave 3's capstone (docs/V2-RESEARCH-AGENT §5) is the in-process research
worker: `internal/researcher/fleet/worker.go` plus its prompt
(`worker_prompt.go`), action schema (`worker_action.go`) and single-query
Researcher binding (`worker_researcher.go`). It ties the landed model, search
and web_fetch clients into a bounded search -> read -> reason -> synthesise
loop and maps the result onto one `types.Interaction` the existing formatter
renders. No new dependency; no vendor SDK. This entry records the three
decisions the plan called out.

**The action schema is a closed three-verb vocabulary, enforced twice.** Each
turn the model is asked for exactly one action via provider-native structured
output (`model.Request.JSONSchema` + `SchemaName`, i.e. `response_format`
`json_schema` with `strict: true`). The schema is an object with a required
`action` discriminator constrained by `enum` to exactly three values, plus the
per-action fields:

- `search` — `{ "action": "search", "query": string }`
- `fetch` — `{ "action": "fetch", "url": string }`
- `final` — `{ "action": "final", "answer": string, "citations": [ { "url": string, "title"?: string } ] }`

`additionalProperties` is false at the top level and on each citation. The
prompt restates the same contract in prose so a model that only reads
instructions and one that only obeys the schema agree. This is the ONLY surface
through which the model can influence the world, and it is enforced twice: the
provider `enum` bars a fourth kind on the wire, and `parseAction` decodes
strictly (`DisallowUnknownFields`) and rejects any unrecognised kind — `shell`,
`write`, `exec`, or anything else — and any known kind missing its required
field. The loop's dispatch is a closed `switch` with no default execution
branch, so a side-effecting or malformed action can only be *refused* (the run
ends `failed` with a scrubbed detail and no side effect), never run. This is
the research-only-by-construction guarantee (V2-RESEARCH-AGENT §1), pinned by
`TestRunWorkerRefusesSideEffectingActions`. `fetch` is further scoped to URLs
that appeared in a prior search result (a per-run allow-list), defence in depth
over the fetch client's own SSRF guard; a fetch of an unseen URL is a
recoverable nudge back to the model, not a fatal error.

**`RunWorker(ctx, WorkerDeps, Brief) Finding` is factored for Wave 4 reuse.**
The plan requires the lead to reuse the worker per brief, so the loop is a
free function over three small types, not a method on the single-query adapter:

- `Brief{ Objective, OutputFormat, SourceGuidance, Boundaries string }` — one
  unit of work; blank fields fall back to instructive defaults so a minimal
  brief still yields a coherent prompt.
- `Finding{ Text string; Citations []types.Citation; Usage types.Usage; Status types.Status; Detail string }`
  — the outcome; already-deduplicated citations, accumulated usage, a terminal
  status and a diagnostic detail.
- `WorkerDeps{ Model *model.Client; Search *search.Client; Fetch *fetch.Client; Tracer trace.Tracer; Caps Caps }`
  — the shared collaborators; the lead and every worker share one set of
  clients, only the `Brief` and `Caps` differ per run.
- `Caps{ MaxTurns int; MaxTokens int; CeilingGBP float64; Timeout time.Duration }`
  — the structural caps.

`RunWorker` never returns an error: every outcome — a final answer
(`completed`), a cap stop (`incomplete`), or a tool/model failure (`failed`) —
is expressed as a `Finding`, so a caller gets a uniform result to map or store.
Partial citations gathered before any stop are preserved on all outcomes, so a
bounded or failed run never discards the sources it found. Wave 4's lead
dispatches one `RunWorker` per decomposed brief under its own fan-out and
concurrency caps, stores each `Finding` by reference, and synthesises over the
references — it does not need the single-query `Worker` type, which is one
caller of the same loop. The `Worker` Researcher allocates an opaque local
`wkr_<128-bit-hex>` id, launches `RunWorker` in a goroutine, returns the id
immediately (§3), and maps the `Finding` onto one `types.Interaction`
(`agent="worker"`, `tools=[web_search, web_fetch]`, one text output, the
citations, the accumulated usage). The local id is a handle, not a durable
resume token: a crashed in-process run cannot be recovered by `chiron get`
(§3), the accepted limitation until control-plane durability lands.

**The cost signal is tokens and search count; GBP is best-effort.** Usage
accumulates across turns: input/output tokens summed from each model turn, and
a search count incremented per `search` action. `EstimatedCostGBP` stays 0
unless a rate is wired in — there is no price table in CI, and inventing one
would make the cost cap non-deterministic. The token and search counters are
therefore the primary spend signal; the token cap (`MaxTokens`) bounds a run
deterministically without any pricing, and the GBP ceiling (`CeilingGBP`)
expresses operator intent for when a rate exists (both are honoured: exceeding
either ends the loop `incomplete`). A single turn's completion is capped to the
remaining token budget so no one turn overshoots the accumulated cap by a full
max-completion. Best-effort worker spans and search/token/cost metrics are
emitted through the shared trace vocabulary (`SpanWorker` added to
`internal/trace/names.go`; the fixed run-core span names are unchanged) when a
tracer is injected; a failed emission never fails the run.

**Composition-root wiring.** `--agent worker` is flipped to real construction
in `internal/cli/research.go`; `--agent fleet` still returns the not-yet-wired
error (Wave 4). The seam lifecycle is split so a worker run resolves only the
fleet key references (`fleet.model_key_ref`, and `fleet.search_key_ref` when
set) at the composition root — never inside the loop — and does not require the
Gemini key. `fleet.worker_timeout` maps onto both the whole-run cap and each
per-call timeout (no single call outlasts the worker's budget). The fetch
client keeps `AllowLoopback` false in production; only tests flip it on to
reach loopback fakes. The model client mandates an explicit model identifier,
so `fleet.model_name` is required at the root rather than sent empty.

## 2026-09-23 — web_fetch: connection-time SSRF pinning and the refused-destination error

Remediates the cycle-3 review findings against
`internal/researcher/fleet/fetch` (C3-SEC-1, C3-SEC-3, C3-SEC-8, C3-CODE-11,
C3-CODE-12, C3-TEST-4, and the fetch side of C3-CODE-3 and C3-CODE-4). It
supersedes the "Connection-time IP pinning is a deliberate follow-up"
paragraph of the 2026-07-01 web_fetch entry. No new dependency.

**The SSRF check runs when the connection is dialled.** The client's
transport is a clone of `http.DefaultTransport` (or of a caller-supplied
`*http.Transport`) with a `DialContext` installed by the guard. That dialer
resolves the host itself through the injectable `Resolver` (`LookupIPAddr`
with the request context; `net.DefaultResolver` by default), refuses if any
answer is internal, and passes only the checked `IP:port` to the underlying
dialer. The address connected to is therefore the address checked: neither a
TTL-0 rebinding answer nor a redirect hop can reach an unchecked address. The
request URL is unchanged, so TLS SNI, certificate verification and the `Host`
header stay on the hostname. The pre-flight check, now inside the
`RequestTimeout` context, and the `CheckRedirect` re-check, on
`req.Context()`, remain as fast refusals ahead of the dial; the redirect cap
stays at 5. Resolving in the guard rather than checking in a
`net.Dialer.Control` hook keeps the refuse-on-any-answer rule (a `Control`
hook sees only the address being connected) and gives the DNS branch a seam
that tests can fake without the network.

**Accepted costs of pinning.**

- Each fetch resolves twice, once before the request and once at the dial;
  Go keeps no DNS cache.
- Checked addresses are dialled one after another in resolver order, so the
  IPv4/IPv6 racing of Happy Eyeballs is lost. A black-holed first address
  costs the dialer's timeout (bounded by `RequestTimeout`) before the next is
  tried.
- The transport never uses a proxy: `HTTP_PROXY`/`HTTPS_PROXY` are ignored
  and a caller transport's `Proxy` is dropped, because a proxy resolves names
  with its own resolver, beyond the guard's reach. An operator egress proxy
  is therefore unsupported; admitting one needs an explicit opt-in that
  knowingly moves the SSRF boundary to the proxy.
- A caller-supplied `HTTPClient` must have a nil transport or an
  `*http.Transport` without `DialTLS`/`DialTLSContext` (the transport sends
  HTTPS through a custom TLS dialer instead of `DialContext`); `New` returns
  an error otherwise. The caller's client and transport are cloned, never
  mutated, and the caller's `DialContext`, if set, makes each connection to a
  checked address.

**The address classifier is an explicit table.** `isInternal` works on
`netip.Addr`. Beyond loopback and unspecified it refuses 0.0.0.0/8, 10/8,
100.64.0.0/10 (CGNAT, including the 100.100.100.200 metadata address),
169.254/16, 172.16/12, 192.0.0.0/24, 192.168/16, 198.18.0.0/15, 224.0.0.0/4,
240.0.0.0/4 (including 255.255.255.255), 64:ff9b:1::/48 (local-use NAT64),
fc00::/7, fe80::/10, fec0::/10 and ff00::/8. IPv4-mapped addresses are
unmapped, zones are stripped (a zoned address never matches a
`netip.Prefix`), an unparseable address is refused, and the IPv4 address
carried by NAT64 (64:ff9b::/96), 6to4 (2002::/16) and IPv4-compatible
(::/96) addresses is extracted and re-checked without the `AllowLoopback`
exemption. `AllowLoopback` still exempts only loopback and unspecified. The
documentation ranges (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24,
2001:db8::/32) are not refused, since nothing routes them; the tests use
203.0.113.10 as a stand-in public address. An allow-only-global rule was
the alternative; the explicit table is kept because each row can be
reviewed and is enumerated by `TestIsInternal`.

**Refusals and status failures are typed.** Every SSRF refusal wraps the
exported sentinel `ErrRefusedDestination`, and a non-2xx response wraps an
exported `*HTTPStatusError{Status}`, so a caller can tell a refused
destination (a signal about the search result) from an ordinary fetch
failure with `errors.Is`/`errors.As`. `client.Do` returns a `*url.Error`
naming the request URL with its userinfo, so a refusal is re-wrapped from
the cause beneath that layer, whose message holds only a host and an
address. Every other transport failure is still flattened and scrubbed of
userinfo.

**Requests identify Chiron, and the default body bound is 1 MiB.** Requests
carry `User-Agent: chiron/2 (+https://github.com/rxbynerd/chiron;
research-only web_fetch)`, overridable with `Options.UserAgent`, and
`Accept: text/html, application/xhtml+xml, text/plain;q=0.9, */*;q=0.5`. Go's
default agent draws 403s from many CDNs, and a site operator can now see who
is fetching and why. The default `MaxContentBytes` drops from 8 MiB to
1 MiB. It is a memory backstop, not a transcript budget: a caller that feeds
pages to a model passes its own, tighter bound.

## 2026-09-23 — Standard-model and search clients: strict-schema fake, max_completion_tokens, redirect downgrade

Cycle-3 review remediation for `internal/researcher/fleet/model` and
`internal/researcher/fleet/search`. No new dependency.

**`max_completion_tokens` replaces `max_tokens` on the wire.** OpenAI's Chat
Completions API rejects `max_tokens` for GPT-5-family and o-series models and
documents `max_completion_tokens` as its replacement; the replacement also
counts reasoning tokens, which is the bound a per-turn cap needs.
`Request.MaxTokens` keeps its name, so callers are unaffected; only the wire
field changes, amending the "optional `max_tokens`" wording of the 2026-07-01
standard-model entry. An OpenAI-compatible server that predates the field may
ignore it and leave a turn uncapped server-side; when a worker token cap is
configured, the worker's cumulative check still stops the loop.

**The fake model enforces strict structured output.** `Generate` always sends
`strict: true` for a structured request, and the provider rejects a strict
schema unless every object, at any depth, sets `additionalProperties: false`
and lists every property in `required` (an optional field becomes a union with
`null`). The fake accepted any schema, so a non-compliant schema passed CI and
failed every real run on its first turn. `model.ValidateStrictSchema` now
encodes those two rules, walking `properties`, `$defs`/`definitions`, `items`,
`prefixItems`, `anyOf`/`oneOf`/`allOf` and `not`, and `FakeServer` answers a
failing strict request with HTTP 400 and OpenAI's `invalid_request_error`
envelope, recording the call without consuming a scripted reply. The
validator is exported in the non-test build, beside the fake, so a package
that builds a schema can unit-test it directly. It checks only these two
rules, not the whole strict-mode subset (supported keywords, nesting limits,
root type), so a green fake is necessary but not sufficient.

**Redirects: no scheme downgrade.** Both credential-bearing clients refused
cross-host redirects but followed a same-host `https` to `http` redirect, and
`net/http` re-sends `Authorization` to any target on the same hostname
whatever its scheme, so the key would travel in cleartext. The redirect
policy, renamed from `refuseCrossHostRedirects` to `refuseUnsafeRedirects` in
both packages, now also refuses a non-`https` target when the original request
was `https`; same-host `https` redirects and the three-hop cap are unchanged.
The v1 `internal/interactions.refuseCrossHostRedirects` has the same shape and
is not changed by this entry. Alongside this, both clients reject an endpoint
that embeds userinfo, and no endpoint error echoes a value containing `@`.

**MCP transport conformance.** The search client now follows three more rules
of the 2025-06-18 Streamable-HTTP transport. It sends `MCP-Protocol-Version`
on every request after `initialize`, using the `protocolVersion` the server
returned, or the client's own revision when the result names none; a
different revision is still tolerated rather than refused, since the client
relies only on the JSON-RPC envelope. It requires a reply's `id` to match the
request's on both framings, accepting a null id only on an error reply, and
skips SSE frames that carry a `method` (server requests and notifications,
which it does not answer). When `initialize` issued an `Mcp-Session-Id`,
`Search` ends the session with a best-effort `DELETE` before returning,
whatever the outcome; the `DELETE` runs under the search's own deadline and
its result is ignored, since a server may answer 405. A session still lives
for one `Search`: reusing one across searches would save two round trips per
query but needs re-initialisation on a 404 and concurrency control, and is
deferred. The tool name and query argument key stay configurable through
`Options.ToolName` and `Options.QueryArgKey` (defaults `search` and `query`).
