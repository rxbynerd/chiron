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
