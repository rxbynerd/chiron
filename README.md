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
| `--agent` | `deep-research` | `deep-research-max` (tier table below), `worker` for the in-process research loop, or `fleet` for the opt-in multi-worker orchestrator (sections below). |
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
| `--budget <gbp>` | unset | Block the run before any spend if the estimate exceeds the cap (deep-research tiers; `worker` and `fleet` use `--fleet-ceiling`). |
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

### The in-process worker (`--agent worker`)

`--agent worker` runs Chiron's own bounded research loop instead of a
managed Deep Research task: one OpenAI-compatible Chat Completions model
decides, turn by turn, whether to call a web-search MCP tool, fetch a page
through an SSRF-guarded `web_fetch`, or write the final answer. The report
cites only URLs the loop actually fetched. It is the v2 product path; the
Gemini tiers remain the stopgap and eval baseline.

```sh
export MODEL_KEY=...      # the standard-model key
export SEARCH_KEY=...     # the search-MCP key, if the server needs one
chiron research --agent worker \
  --fleet-model-endpoint https://api.openai.com/v1 \
  --fleet-model-name gpt-5.5 \
  --fleet-model-key-ref secret://MODEL_KEY \
  --fleet-search-endpoint https://search.example.com/mcp \
  --fleet-search-key-ref secret://SEARCH_KEY \
  --query "Competitive landscape of 10BASE-T1L PHY vendors" --out report.md
```

| Flag | Default | Notes |
| --- | --- | --- |
| `--fleet-model-endpoint` | — | Standard-model base URL; absolute `https://`, `http://` for loopback only. Required. |
| `--fleet-model-name` | — | Model identifier sent on every call. Required. |
| `--fleet-model-key-ref` | — | `secret://` reference to the model key. Required. |
| `--fleet-search-endpoint` | — | Web-search MCP (Streamable HTTP) URL; same scheme rule. Required. |
| `--fleet-search-key-ref` | — | `secret://` reference to the search key; omit for a keyless server. |
| `--fleet-max-turns` | `8` | Cap on model turns (search, fetch or final). |
| `--fleet-max-tokens` | `400000` | Cap on prompt+completion tokens across the run; `0` uncapped. |
| `--fleet-worker-timeout` | `5m` | Wall-clock cap for the whole run and each call. |
| `--fleet-ceiling` | `0` | Estimated-cost cap in GBP; needs both price flags. |
| `--fleet-price-input` / `--fleet-price-output` | `0` | Model prices in GBP per million prompt / completion tokens. |
| `--fleet-max-page-bytes` | `65536` | Bound on one fetched page's text after HTML-to-text reduction. |
| `--fleet-knowledge-provider` | unset | Knowledge store to recall from: `billet` or `alexandria`. Unset disables recall. |
| `--fleet-knowledge-endpoint` | — | Knowledge store base URL; same scheme rule. Required with a provider. |
| `--fleet-knowledge-key-ref` | — | `secret://` reference to the knowledge store key; required for `alexandria`, optional for `billet`. |
| `--fleet-knowledge-space` | — | Alexandria space slug to scope recalls to (`alexandria` only). |
| `--fleet-knowledge-limit` | `5` | Hits per recall, 1 to 20. |
| `--fleet-knowledge-remember` | `false` | Save each completed finding back to the store (`billet` only; rejected for `--agent fleet`). |

The worker keeps the v1 money rules: the paid model call is never
auto-retried, the interaction id (a local `wkr_` handle) is emitted before
the first call, and hitting any cap ends the run `incomplete` (exit 2)
with the citations gathered so far. Tool failures (a 404, a refused
destination, a binary page) are fed back to the model so it can choose
another source; three consecutive failures end the run `failed`. The
`wkr_` handle is not a resume token: `chiron get` and `chiron follow-up`
refuse it. The Gemini-only levers (`--budget`, `--plan`, `--model`,
`--visualise`, `--tools`, `--mcp`, `--file-search`, `--input`) are
rejected for the worker rather than silently ignored.
`examples/researchconfig/worker.yaml` is a complete base config.

`--template <path>` is honoured by the worker, but it replaces only the
"Required output format" block of the worker's system prompt: it can ask
for sections, tables or tone, while the actions, source rules and
citation rules stay Chiron's. It uses the same `text/template` grammar as
the deep-research template, and `{{.Query}}` is available but optional,
because the query already has its own section of the prompt. The file is
limited to 8 KiB because the block is re-sent on every turn. It is
checked at startup: a file that is too large, not UTF-8, unparsable,
names an unknown field or renders blank fails before any request. The
same 8 KiB bound is re-checked against the real query when a run starts,
so a template that only exceeds it once the query is substituted still
fails before any request, not partway through the run. Any `<<<` in the
rendered block is spaced out, as in retrieved text, so it cannot mimic the
tool-result fence.
Each turn emits a `delta` event (`type: worker_turn`, with the turn, action,
a scrubbed target and tokens so far) on stderr between `interaction_created`
and `run_completed`, unless `--quiet`.

#### Organisational knowledge (Billet, Alexandria)

With a knowledge store configured the worker gains a read-only `recall`
action: it queries the store for prior findings, decisions and notes,
usually before confirming on the public web, and may cite a recalled item
by its reference. A recalled item is citable but never fetched. Billet
(the suite's memory sidecar, over MCP) can also receive each completed
finding with `--fleet-knowledge-remember`; that write goes only to the
memory store, never to a workspace, and appears on the report's tool
list. Alexandria (over its REST search API) is recall-only. The design is
`docs/KNOWLEDGE.md`.

```sh
chiron research --agent worker \
  --fleet-model-endpoint https://api.openai.com/v1 \
  --fleet-model-name gpt-5.5 \
  --fleet-model-key-ref secret://MODEL_KEY \
  --fleet-search-endpoint https://search.example.com/mcp \
  --fleet-knowledge-provider billet \
  --fleet-knowledge-endpoint http://127.0.0.1:8140/ \
  --fleet-knowledge-remember \
  --query "Which 10BASE-T1L PHY vendor did we shortlist, and has anything changed?"
```

A recalled Billet memory the answer relies on is listed under Sources as
its title and `billet://memory/<id>` locator, not as a link. Recalls count
towards the same three-strike tool-failure bound as search and fetch.

### The in-process fleet (`--agent fleet`)

`--agent fleet` puts a lead in front of several workers. One lead call
splits the question into three to five briefs, each aimed at the external
web; a bounded pool runs one worker per brief, each the same loop as
`--agent worker` under the same per-worker flags and caps; and the lead
writes one synthesised report from the findings, then attributes its
claims to the sources the workers cited. The lead and every worker share
the one model endpoint and key.

The fleet is opt-in and is not the default: a run costs several workers
plus three lead calls, so expect a multiple of a single worker's spend.
The eval gate that would compare it with Gemini Deep Research has not yet
run, so `deep-research` stays the default and `worker` stays the
single-loop path.

```sh
chiron research --agent fleet --fleet-memory inmemory \
  --fleet-max-workers 5 --fleet-concurrency 3 \
  --fleet-model-endpoint https://api.openai.com/v1 \
  --fleet-model-name gpt-5.5 \
  --fleet-model-key-ref secret://MODEL_KEY \
  --fleet-search-endpoint https://search.example.com/mcp \
  --query "How do air-source heat pumps compare with gas boilers for UK homes?" --out report.md
```

| Flag | Default | Notes |
| --- | --- | --- |
| `--fleet-memory` | `noop` | Must be `inmemory` for the fleet: the plan and each finding pass between the lead and its workers by reference through an in-process store, dropped when the run ends. Ignored by `worker`. |
| `--fleet-max-workers` | `5` | Most briefs dispatched. A brief past the cap is dropped and listed as a gap, and the run ends `incomplete`. |
| `--fleet-concurrency` | `3` | Most workers running at once; at most `--fleet-max-workers`. |

The worker's money rules hold across the fleet. No lead or worker model
call is ever auto-retried. Each worker is bounded by `--fleet-max-turns`,
`--fleet-max-tokens`, `--fleet-ceiling` and `--fleet-worker-timeout`, so
the workers' worst case is `--fleet-max-workers` times one worker's caps.
Each lead call is a single attempt whose completion is capped at 16,384
tokens. The report's tokens and estimated cost are the one rollup of every
lead and worker call. The run ends `failed` when no worker produced a
finding (a decomposition the lead cannot use fails before any worker
runs), `incomplete` when a brief yielded no finding or only a partial one,
or when synthesis or citation degraded, and `completed` otherwise.

When `--timeout` ends a fleet run, no report is emitted and the command
exits 1, because the report is written on the run's context, which has
ended. If the lead has recorded its outcome by then, the fleet logs that
outcome's spend to stderr instead, as one warning line. That includes an
outcome stitched from the findings because the deadline cut off synthesis
or citation. The line carries the `flt_` id, status, tokens, search count
and estimated cost, never report or finding text. `--agent worker` also
emits no report when `--timeout` ends it, and it logs no spend. An
interrupt (Ctrl-C) ends either agent at once, with neither a report nor a
spend line.

The interaction id is a local `flt_` handle, emitted before the first
call; like `wkr_`, it is not a resume token, so `chiron get` and
`chiron follow-up` refuse it. Each worker turn emits the `worker_turn`
delta described above, with a `worker_id` field and the text prefixed by
the worker it came from. `--template` shapes the lead's synthesis only,
under the worker template's rules; workers keep the built-in format for
their findings. Workers can recall from a configured knowledge store, but
`--fleet-knowledge-remember` is rejected for the fleet.

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
| `CHIRON_FETCH_ALLOW_LOOPBACK` | Set to exactly `1`, lets the worker's `web_fetch` reach loopback hosts **for tests only**. Any other value is a startup error; it never relaxes the private-network or metadata refusals. Never set it in production. |

## Documents

- `docs/PROPOSAL.md` — the project proposal and architecture (the spec).
- `docs/INTERACTIONS-API.md` — the verified Gemini Interactions API
  reference; normative wherever it and the proposal disagree.
- `docs/DECISIONS.md` — the running decision log.
- `docs/V2-PLAN.md` — the phased v2 implementation plan;
  `docs/V2-RESEARCH-AGENT.md` is the binding design for the in-process
  research agents and `docs/V2-AMENDS.md` the amendment log.
- `docs/reviews/` — review briefs and remediation reports for the v1 and
  v2 review cycles.
- `AGENTS.md` / `CLAUDE.md` — orientation for agentic sessions.
- `SECURITY.md` — security posture and vulnerability reporting.
