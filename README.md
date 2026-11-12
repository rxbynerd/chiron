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
just test         # go test -race ./...
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
| `--otlp-endpoint <url>` | unset | OTLP/HTTP collector base URL for spans; `/v1/traces` is appended. Absolute `https://`, `http://` loopback only. For a collector that needs no request headers: refused while `OTEL_EXPORTER_OTLP_HEADERS` or `OTEL_EXPORTER_OTLP_TRACES_HEADERS` is set. See "Observability" below. |
| `--langfuse-endpoint <url>` | Langfuse Cloud (EU region) once the key refs are set | Langfuse OTLP base URL; same scheme rule. |
| `--langfuse-public-key-ref` | — | `secret://` reference to the Langfuse public key (never a literal); required with `--langfuse-secret-key-ref`. |
| `--langfuse-secret-key-ref` | — | `secret://` reference to the Langfuse secret key (never a literal); required with `--langfuse-public-key-ref`. |

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

The worker's endpoints never travel through a pipeline: a base config
that names one is refused, and so `research-config` refuses the endpoint
flags. Compose everything else, then give the endpoints to the final
`chiron research` stage, by flag or by environment variable:

```sh
chiron research-config --agent worker \
    --fleet-model-name gpt-5.5 --fleet-model-key-ref secret://MODEL_KEY \
  | CHIRON_FLEET_MODEL_ENDPOINT=https://api.openai.com/v1 \
    CHIRON_FLEET_SEARCH_ENDPOINT=https://search.example.com/mcp \
    chiron research --query "Competitive landscape of 10BASE-T1L PHY vendors"
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
cites only URLs the loop saw in a search result or fetched. It is the v2 product path; the
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
| `--fleet-model-endpoint` | — | Standard-model base URL; absolute `https://`, `http://` for loopback only. Required, from this flag or `CHIRON_FLEET_MODEL_ENDPOINT`, never a base config. |
| `--fleet-model-name` | — | Model identifier sent on every call. Required. |
| `--fleet-model-key-ref` | — | `secret://` reference to the model key. Required. |
| `--fleet-search-endpoint` | — | Web-search MCP (Streamable HTTP) URL; same scheme rule. Required with the `mcp` search provider (the default), from this flag or `CHIRON_FLEET_SEARCH_ENDPOINT`, never a base config. |
| `--fleet-search-key-ref` | — | `secret://` reference to the search key; omit for a keyless server. |
| `--fleet-search-tool` | `search` | MCP tool the search client invokes; change it for a search server that names its tool differently. |
| `--fleet-search-query-arg` | `query` | Argument key the search query is passed under; change it to match the configured tool. |
| `--fleet-search-provider` | `mcp` | Who runs web searches: `mcp` (the server at `--fleet-search-endpoint`) or `exa`. |
| `--fleet-fetch-provider` | `direct` | Who fetches pages: `direct` (Chiron's own SSRF-guarded client) or `exa`. |
| `--fleet-exa-endpoint` | — | Exa API base URL, normally `https://api.exa.ai`; same scheme rule. Required when either provider is `exa`, from this flag or `CHIRON_FLEET_EXA_ENDPOINT`, never a base config. |
| `--fleet-exa-key-ref` | — | `secret://` reference to the Exa key, which serves both roles. Required when either provider is `exa`. |
| `--fleet-search-send-objective` | `false` | Send the research question to Exa with each search (`exa` search only). |
| `--fleet-max-turns` | `8` | Cap on model turns (search, fetch or final). |
| `--fleet-max-tokens` | `400000` | Cap on prompt+completion tokens across the run; `0` uncapped. |
| `--fleet-worker-timeout` | `5m` | Wall-clock cap for the whole run and each call. |
| `--fleet-ceiling` | `0` | Model-token cost cap in GBP; needs both price flags. Web-provider spend is not counted. |
| `--fleet-price-input` / `--fleet-price-output` | `0` | Model prices in GBP per million prompt / completion tokens. |
| `--fleet-max-page-bytes` | `65536` | Bound on one fetched page's text after HTML-to-text reduction; with Exa fetch, also the page's character bound (at most 1,000,000). |
| `--fleet-knowledge-provider` | unset | Knowledge store to recall from: `billet` or `alexandria`. Unset disables recall. |
| `--fleet-knowledge-endpoint` | — | Knowledge store base URL; same scheme rule. Required with a provider, from this flag or `CHIRON_FLEET_KNOWLEDGE_ENDPOINT`, never a base config. |
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

The endpoints come only from their flags or from
`CHIRON_FLEET_MODEL_ENDPOINT`, `CHIRON_FLEET_SEARCH_ENDPOINT` and
`CHIRON_FLEET_KNOWLEDGE_ENDPOINT`, and a set flag wins over its
variable. A `--config` file or piped config that names an endpoint is
refused before any key is resolved, because a shared config must not
choose both a key reference and the host that key is sent to. Before the
first model call the run names its destinations on stderr, by scheme and
host only, in one `delta` event:
`{"endpoints":{"model":"https://api.openai.com","search":"https://search.example.com"}}`,
with a `knowledge` entry when a knowledge store is configured.

#### Searching and fetching through Exa

Exa can serve either web role, or both, instead of an MCP search server and
Chiron's own page fetches. Exa crawls the pages from its own infrastructure,
so target sites see Exa's agent, not Chiron's.

```sh
export MODEL_KEY=... EXA_KEY=...
export CHIRON_FLEET_EXA_ENDPOINT=https://api.exa.ai
chiron research --agent worker \
  --fleet-model-endpoint https://api.openai.com/v1 \
  --fleet-model-name gpt-5.5 \
  --fleet-model-key-ref secret://MODEL_KEY \
  --fleet-search-provider exa --fleet-fetch-provider exa \
  --fleet-exa-key-ref secret://EXA_KEY \
  --query "Competitive landscape of 10BASE-T1L PHY vendors" --out report.md
```

Every search query, and every fetched URL with its query string, goes to
Exa; the research question goes too only with
`--fleet-search-send-objective`, which only the final stage's flag can set.
Exa has no built-in endpoint: name it with `--fleet-exa-endpoint` or
`CHIRON_FLEET_EXA_ENDPOINT`, as for the model. While the variable is
exported, any base config can select Exa, so export it only for the runs
that should use it.
URLs carrying credentials or naming internal or local hosts are refused
before anything is sent. The report's front matter records the provider
(`tools: ['web_search:exa', 'web_fetch:exa']`) and Exa's reported cost as
`web_cost_usd`, separate from the GBP model estimate; when a call's cost
was not reported, `web_cost_unknown_calls` counts it and `web_cost_usd` is
a lower bound. A rejected key or an exhausted Exa balance ends the run
`failed` at once. The endpoints event names Exa's origin for `search` and
adds a `fetch` entry.

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

### Observability

Spans and per-run metrics can be forwarded to an OTLP collector or to
Langfuse, on top of the standard `OTEL_EXPORTER_OTLP_ENDPOINT` /
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` variables. An explicit flag wins over
the environment's endpoint, and precedence is otherwise most-specific
first: the Langfuse key pair, then `--otlp-endpoint`, then the
`OTEL_EXPORTER_OTLP_*` variables, then the no-op tracer. A run forwards to
one destination: `--otlp-endpoint` and the Langfuse keys conflict.

```sh
chiron research --agent worker \
  --langfuse-public-key-ref secret://LANGFUSE_PUBLIC_KEY \
  --langfuse-secret-key-ref secret://LANGFUSE_SECRET_KEY \
  --query "Competitive landscape of 10BASE-T1L PHY vendors"
```

`--langfuse-public-key-ref` and `--langfuse-secret-key-ref` are required
together; with both set and no `--langfuse-endpoint`, spans go to Langfuse
Cloud's EU region (`https://cloud.langfuse.com/api/public/otel`). A
US-region or self-hosted Langfuse needs `--langfuse-endpoint` set to its
own OTLP base URL, the host followed by `/api/public/otel`. Every span
carries the research query and the run's spend, so the collector or
Langfuse project this points at is deployment configuration, not a
debugging toggle.

`--otlp-endpoint` is for a collector that needs no request headers. An
authenticated collector is configured entirely through the environment,
endpoint and headers together (`OTEL_EXPORTER_OTLP_ENDPOINT` with
`OTEL_EXPORTER_OTLP_HEADERS`), and `--otlp-endpoint` does not override
that pairing: a run with it refuses to start while
`OTEL_EXPORTER_OTLP_HEADERS` or `OTEL_EXPORTER_OTLP_TRACES_HEADERS` is set,
so those credentials never reach another host. The Langfuse keys replace
both the environment's endpoint and its headers. A malformed header or
endpoint variable is refused on every path, naming the variable but not
its value.
With `--otlp-endpoint` or the Langfuse keys, the exporter refuses
redirects, and the `OTEL_EXPORTER_OTLP_*` certificate, client-certificate
and timeout variables do not configure the connection (an `http://`
loopback endpoint is refused while a certificate variable is set).

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
  `cost_summary`) — streamed thought summaries, and a worker run's
  endpoint hosts, arrive as `delta` events. Text log lines share the stream,
  such as the fleet's spend warning after a timeout, so `2>events.ndjson`
  captures them too and the file is not valid NDJSON. To keep only the
  events, capture stderr and drop the lines that are not JSON:

  ```sh
  chiron research --query "..." --out report.md 2>stderr.log
  jq -cR 'fromjson? | select(. != null)' stderr.log >events.ndjson
  ```

### Environment variables

| Variable | Purpose |
| --- | --- |
| `GEMINI_API_KEY` | The API key, read via the default `secret://GEMINI_API_KEY` reference. Any `secret://env/NAME` or `secret://file/<absolute path>` reference works instead. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Standard OpenTelemetry configuration; naming an endpoint enables the OTel tracer (spans + per-run metrics). Absent, tracing is a no-op. `--otlp-endpoint` and the `--langfuse-*` flags take precedence over these — see "Observability" above. |
| `CHIRON_GEMINI_BASE_URL` | Overrides the Gemini API endpoint **for tests only**: the key is sent to whatever this names, so it is validated at startup — absolute `https://` anywhere, `http://` for loopback hosts only. Never set it in production; absence is the safe default. |
| `CHIRON_FETCH_ALLOW_LOOPBACK` | Set to exactly `1`, lets the worker's direct `web_fetch` reach loopback hosts **for tests only**. Any other value is a startup error; it never relaxes the private-network or metadata refusals. Read only with `--fleet-fetch-provider direct`. Never set it in production. |
| `CHIRON_FLEET_MODEL_ENDPOINT` / `CHIRON_FLEET_SEARCH_ENDPOINT` / `CHIRON_FLEET_KNOWLEDGE_ENDPOINT` / `CHIRON_FLEET_EXA_ENDPOINT` | The worker's model, search, knowledge store and Exa endpoints when the matching `--fleet-*-endpoint` flag is unset. The matching key is sent to whatever these name, so they are validated at startup with the `CHIRON_GEMINI_BASE_URL` rule. Each is read only when the run uses its endpoint: the search variable with MCP search, the knowledge variable with a knowledge provider, the Exa variable when a web role uses Exa. Unlike the two entries above, these are a production deployment mechanism: set them as operator-supplied configuration. |

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
