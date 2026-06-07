# Chiron

Chiron is the **researcher** of the Equestrianism suite — the wise centaur
to Stirrup's tack. It drives a Google Gemini Deep Research agent
end-to-end from the command line: start a research task, await the
long-running agent (streaming or polling), retrieve the result, and emit
a cited, structured Markdown report. It is written in Go, ships as a
single static binary, and is **research-only**: it never mutates a
workspace, runs no shell, and applies no edits. Where Stirrup *changes*
code under a harness, Chiron *investigates*.

## Building

```sh
just build        # go build -o bin/chiron ./cmd/chiron
just test         # go test ./...
just vet          # go vet ./...
just lint         # golangci-lint if installed, else go vet
just ci           # everything CI runs
just proto-lint   # lint and compile-check proto/chiron/v1 (needs buf)
```

Requires Go 1.26. Without `just`: `go build -o bin/chiron ./cmd/chiron`.

## Usage

The API key is resolved from a `secret://` reference (never a literal in
configuration); the default reads the `GEMINI_API_KEY` environment
variable:

```sh
export GEMINI_API_KEY=...   # or --api-key-ref secret://file/<path>
```

### The four commands

```sh
# The main path: start, await, format, emit.
chiron research --query "Competitive landscape of 10BASE-T1L PHY vendors" --out report.md

# Emit the resolved ResearchConfig JSON without running.
chiron research-config --agent deep-research-max

# Re-fetch and format an interaction (resume after a crash).
chiron get <interaction-id>

# Ask a follow-up question against a completed interaction.
chiron follow-up <interaction-id> --query "..."
```

### Research flags

| Flag | Default | Notes |
| --- | --- | --- |
| `--query` / positional | — | The research question. |
| `--agent` | `deep-research` | or `deep-research-max` (tier table below). |
| `--plan` | off | Collaborative planning: review and refine the plan before spending. |
| `--accept-plan` | off | With `--plan`, approve the first proposed plan without prompting. |
| `--model` | adapter default | Follow-up Q&A model (`chiron follow-up`). |
| `--visualise` | off | Ask the agent to produce charts. |
| `--stream` / `--quiet` | `--stream` | Stream thought summaries vs poll silently (mutually exclusive). |
| `--tools` | API defaults | Comma-separated, e.g. `google_search,url_context,code_execution`. |
| `--mcp name=url` | — | Repeatable; attach a remote MCP server. |
| `--file-search <store>` | — | Repeatable; search an internal corpus store. |
| `--input <path-or-url>` | — | Repeatable; multimodal document/image grounding. |
| `--template <path>` | built-in | Output-format prompt template (sections/tone/tables). |
| `-o, --output` | `text` | `text` \| `json` \| `none` (stdout surface; see below). |
| `--out <path>` | stdout | Write the Markdown report (and chart assets) to a file. |
| `--config <path>` | — | Base `ResearchConfig`; `-` or piped stdin for composition. |
| `--api-key-ref` | `secret://GEMINI_API_KEY` | Never a literal key. |
| `--budget <gbp>` | unset | Block the run before any spend if the estimate exceeds the cap. |
| `--timeout <dur>` | `30m` | Wall-clock; hard cap 60m (the agent's own limit). |

All four commands accept the same flag surface; configuration resolves
as documented defaults → base config (`--config` file or piped stdin) →
explicitly set flags, so a piped value is never clobbered by a flag
default. See `examples/researchconfig/` for sample configs.

### Pipeline composition

`chiron research-config` emits the resolved config as JSON and reads a
piped base from stdin, exactly like Stirrup's `run-config`:

```sh
chiron research-config --agent deep-research-max \
  | chiron research-config --visualise \
  | chiron research --query "Competitive landscape of 10BASE-T1L PHY vendors" --out report.md
```

### Resume

Deep Research tasks run for minutes (most under 20, hard max 60) and are
stored server-side, so Chiron keeps no local state. The interaction id —
the resume handle — is emitted on stderr (an `interaction_created` NDJSON
event) the moment it is known, before anything else can fail. If the CLI
crashes, times out, or is killed mid-run:

```sh
chiron get <interaction-id>
```

re-attaches to the stored interaction, awaits whatever remains
(respecting `--timeout`), and emits the report through the normal sinks.
Nothing new is created and nothing new is spent.

### Cost levers

A single Deep Research task costs real money, so the cost controls are
front and centre:

| Tier (`--agent`) | Planning estimate | Searches |
| --- | --- | --- |
| `deep-research` | ≈ £1.58 (envelope £0.79–£2.37) | ~80 |
| `deep-research-max` | ≈ £3.95 (envelope £2.37–£5.53) | ~160 |

The estimates are the midpoints of Google's published per-tier USD
envelopes ($1.00–$3.00 and $3.00–$7.00) converted at a **fixed planning
rate** of 0.79 USD→GBP. They are pre-run planning figures for the budget
gate and the report's front matter — not billing. Real pricing, live FX,
and attribution belong to Stint.

- `--budget <gbp>` blocks the run client-side, before any interaction is
  created, when the tier's estimate exceeds the cap (exit code 4). The
  gate sits ahead of the plan phase too, because plan rounds also spend.
- `--plan` requests a research plan first and opens an interactive
  review on stderr — accept, refine, or quit, bounded at five plan
  rounds including the proposal — before any research money is spent. Declining exits 4 with nothing
  started. Reviewing needs a terminal on stdin; in a pipeline, pass
  `--accept-plan` to approve the first plan unattended.
- `chiron follow-up` answers against the stored interaction with a
  plain model rather than a new research task — quick and far cheaper
  than re-researching.

### Exit codes

The research outcome is machine-readable from the exit code alone, so
pipelines need not parse the report to learn whether to trust it:

| Code | Meaning |
| --- | --- |
| 0 | The research completed and the report was emitted. |
| 1 | Usage, configuration, or infrastructure errors (bad flags, unresolvable secrets, network failures, timeout). |
| 2 | The research task ended `failed` or `incomplete`, or required client action (a state deep research cannot legitimately produce). |
| 3 | The research task was cancelled or exceeded the server-side budget — stopped, rather than broken. |
| 4 | The run was blocked client-side before any spend: the estimate exceeded `--budget`, or the plan review was aborted. |

### Output surfaces

stdout belongs to the report; diagnostics and progress go to stderr:

- `-o text` (default) writes the Markdown document to stdout — unless
  `--out` owns it, in which case stdout stays quiet.
- `-o json` writes the machine-readable `RunResult` envelope (status,
  usage, report, estimated cost) to stdout; useful beside `--out`.
- `-o none` discards the report; the events and exit code still carry
  the outcome.
- `--out <path>` writes the report to a file (owner-only, `0600`), with
  any agent-generated charts written beside it and referenced as
  relative links.
- **stderr** carries NDJSON run events (`run_started`,
  `interaction_created`, `status_changed`, `delta`, `run_completed`,
  `cost_summary`) — streamed thought summaries arrive as `delta`
  events. Separate them with `2>events.ndjson`.

### Environment variables

| Variable | Purpose |
| --- | --- |
| `GEMINI_API_KEY` | The API key, read via the default `secret://GEMINI_API_KEY` reference. Any `secret://env/NAME` or `secret://file/<absolute path>` reference works instead. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Standard OpenTelemetry configuration; naming an endpoint enables the OTel tracer (spans + per-run metrics). Absent, tracing is a no-op. |
| `CHIRON_GEMINI_BASE_URL` | Overrides the Gemini API endpoint **for tests only**: the key is sent to whatever this names, so it is validated at startup — absolute `https://` anywhere, `http://` for loopback hosts only. Never set it in production; absence is the safe default. |

## Documents

- `docs/PROPOSAL.md` — the project proposal and architecture (the spec).
- `docs/INTERACTIONS-API.md` — the verified Gemini Interactions API
  reference; normative wherever it and the proposal disagree.
- `docs/DECISIONS.md` — the running decision log.
- `docs/reviews/` — review briefs and remediation reports for both v1
  review cycles.
- `AGENTS.md` / `CLAUDE.md` — orientation for agentic sessions.
- `SECURITY.md` — security posture and vulnerability reporting.
