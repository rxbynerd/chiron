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
