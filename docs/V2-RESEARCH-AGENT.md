---
project: Chiron
suite: Equestrianism
status: design (v2 research-agent core — companion to docs/V2-PLAN.md; for hand-off to Claude Code)
author: @rubynerd
date: 2026-06-22
locale: en-GB
language: Go
supersedes: corrects the researcher-core direction of docs/V2-PLAN.md (2026-06-21) per author guidance
summary: >
  The design for Chiron v2's own multi-agent research agent: a Chiron-side orchestrator
  that drives Stirrup research-mode jobs against standard frontier models (GPT-5.4/5.5,
  Claude, Gemini-Pro via Stirrup's provider adapters) equipped with a web-search tool,
  and synthesises a cited report. Managed deep-research products (Gemini Deep Research,
  OpenAI deep-research) are demoted from "the product" to a stopgap worker and the eval
  baseline. This is the hybrid "design the core first" artefact; docs/V2-PLAN.md's
  researcher waves (D2/D3/D5, Waves 3–4, the spikes) are re-seated on it, the service
  scaffolding (control plane, ConnectRPC, observability, Paddock, GKE/WIF) unchanged.
---

# Chiron v2 — the research-agent core

## 0. Why this document exists

`docs/V2-PLAN.md` (2026-06-21) drifted on its researcher core: it made *managed
deep-research products* (Gemini Deep Research, then OpenAI `o3`/`o4-mini-deep-research`)
the `Researcher`, and deferred Stirrup entirely. That inverts the suite's stated v2 goal.
The working principle is:

> Chiron coordinates agents that run **standard frontier models** (GPT-5.4/5.5, Claude,
> …), building **our own research agents** (targeting any frontier model) on **Stirrup**.
> The Gemini/OpenAI managed research agents were always a **stopgap** before that agent
> existed.

So the de-drifted target is: **Chiron's own multi-agent research agent, built on Stirrup,
external-web-only for v2, producing reports as good as the managed agents currently do —
with the managed agents kept only as a stopgap worker and the quality baseline.** This
document designs that agent, grounded in Stirrup's actual contract (read from
`github.com/rxbynerd/stirrup` on 2026-06-22, `main`). `docs/V2-PLAN.md` then re-seats its
researcher waves on it (§11); the service scaffolding it already describes — ConnectRPC
control plane, transport, Langfuse observability, Paddock, GKE/WIF, spend-safety — is
sound and unchanged.

Precedence is unchanged (`INTERACTIONS-API.md` > `DECISIONS.md` > `PROPOSAL.md`); this
design sits beside `V2-PLAN.md` below them. Any Stirrup contract detail it cites is
**confirmed by spike before code** — Stirrup is a separate, evolving project and this
document is not normative for it.

## 1. The drift, precisely

| Decision | V2-PLAN.md (2026-06-21) said | This design says |
| --- | --- | --- |
| What runs the research | Managed deep-research models (`o3`/`o4-mini-deep-research`, Gemini DR) as the `Researcher` | A **Stirrup research-mode job on a standard frontier model + a web-search tool** is the worker; Chiron orchestrates |
| Amend 1 ("OpenAI Responses API + WIF") | Read as OpenAI's *deep-research* models via Responses | Read as the **standard-model** Responses path (GPT-5.4/5.5) — exactly Stirrup's `openai-responses` provider with credential federation |
| Stirrup | Deferred entirely (conflated with internal sources) | **The harness the agent is built on** — un-deferred. Only the internal *source tools* (`file_search`/MCP-over-repos) defer |
| Amend 6 ("same content as Gemini produces") | Read as "keep calling Gemini" | Read as a **quality bar** met by our own agent, with Gemini DR as the measured baseline |
| Managed deep-research | The production backend | A **stopgap worker + the eval baseline** (author's choice) |

Note this is more than reverting the 2026-06-21 revision: `PROPOSAL §6` and the
2026-06-14 draft *also* routed **external** research to managed Gemini DR (Stirrup only
for internal). The corrected target makes the homegrown Stirrup web-research agent the
**primary** external worker — which neither prior draft planned, and which is the genuine
hard problem of v2.

## 2. What Stirrup already gives us (grounded inventory)

Read from Stirrup `main`, 2026-06-22. This is the leverage; Chiron builds the thin layer
above it.

- **A read-only research mode, structurally enforced.** `RunConfig.mode = "research"` is
  "read-only; investigates questions without side effects" — one of four read-only modes
  (`planning` (the CLI default), `review`, `research`, `toil`); only `execution` is
  read/write. Stirrup's own `ValidateRunConfig` requires read-only modes to use a
  `permission_policy` of `deny-side-effects` or `ask-upstream`, and a `ToolsConfig.built_in`
  list that **excludes write tools**. This *is* Chiron's "research-only, structurally"
  non-negotiable — enforced upstream, not just asserted
  (`proto/harness/v1/harness.proto`, `RunConfig.mode` / `PermissionPolicyConfig` /
  `ToolsConfig`).
- **Five hand-rolled provider adapters, no SDKs** (`docs/providers.md`): `anthropic`
  (Messages), `bedrock` (ConverseStream), `openai` (Chat Completions, configurable
  baseURL), **`openai-responses`** (`POST /v1/responses`), `gemini` (Vertex AI). Selected
  via `provider.type`; a `providers` map plus `ModelRouterConfig` (`model_router`) lets one
  job mix cheap/expensive models and lets the lead run different workers on different
  models. This is "target any frontier model" — already built, already no-vendor-SDK.
- **A two-tier credential-federation layer** (`docs/credential-federation.md`):
  `TokenSource` (e.g. `gke-metadata`, `aws-irsa`, `azure-imds`, `github-actions-oidc`) →
  `credential.Source` (`static`, `gcp-workload-identity[-federation]`, **`anthropic-wif`**,
  **`azure-workload-identity`**, `web-identity` for Bedrock). Keyless auth to every
  provider from GKE is a solved Stirrup capability — directly answering amend 1's WIF
  requirement (see §7).
- **MCP tool connections** (`ToolsConfig.mcp_servers`, `MCPServerConfig`): Streamable-HTTP
  MCP servers, tools namespaced `mcp_{server}_{tool}`, bearer auth via `secret://`. This
  is how the web-search tool attaches (§4).
- **`offload-to-file` context strategy** (`ContextStrategyConfig.type`): old messages
  written to a file and replaced with a pointer — the native hook for
  findings-by-reference into Paddock (§6.3).
- **Per-job structural budgets**: `max_turns` (1–100), `max_token_budget` (≤50M),
  `max_cost_budget` USD (≤100), `timeout` (1–3600s). Per-worker spend ceilings come for
  free even though Chiron defers its own budgeting (§9).
- **`spawn_agent`** built-in: a research job can fan out its own sub-agents (a fan-out
  option, §5).
- **An eval framework** (`docs/eval.md`, `stirrup-eval`): HCLv2 suites, judges, a trace
  lakehouse, regression/drift diffing. The home for the "match Gemini-DR" baseline (§8).
- **The harness wire contract** (`proto/harness/v1/harness.proto`,
  `service HarnessService { rpc RunTask(stream HarnessEvent) returns (stream ControlEvent) }`):
  the harness dials a control plane outbound, receives a `task_assignment` (a `RunConfig`),
  streams `text_delta`/`tool_call`/`tool_result`/`done(trace)` back. Chiron drives Stirrup
  over this (§5), Buf-generated, never `go mod` imported (amend 4).

## 3. What Stirrup does NOT give us — the hard centre

**Stirrup has no web-search tool, and its providers' built-in search is intentionally off.**

- `ToolsConfig.built_in` is `read_file, write_file, search_replace, apply_diff, edit_file,
  list_directory, grep_files, find_files, run_command, web_fetch, spawn_agent`. There is a
  `web_fetch` (HTTP GET of a known URL) but **no `web_search`** (query → ranked results).
- Stirrup's `openai-responses` adapter explicitly **excludes** the provider built-in
  `web_search`/`file_search` tools, and the `gemini` adapter excludes `google_search`
  (tracked as Stirrup issue #93): "the harness manages its own conversation history and
  does not delegate to server-side state" (`docs/providers.md`).
- Stirrup's research-mode system prompt (`harness/internal/prompt/systemprompts/
  research.md`) is **codebase-oriented** — "read-only access to the workspace… explore the
  codebase… cite file paths and line numbers… fetch URLs". That is internal research; it
  is not a web-research agent.

So the thing the managed deep-research products give you out of the box — **iterative web
search** (issue a query, read ranked results, follow leads, repeat) — is exactly what
Chiron must supply to Stirrup. This is the centre of v2, and it is why the managed agents
are a sensible stopgap while it is built. It is a tool-and-prompt problem, not a new agent
loop (Stirrup is the loop).

**Resolution (SP-A, settled 2026-06-22): a pluggable search backend — an MCP server
(default, portable) and OpenAI's provider built-in `web_search` (an OpenAI-path option behind
a Stirrup enablement); native scraping discounted.** Native first, to discount it: a
Stirrup-process `web_search` is ruled out — workers run on cloud infrastructure, so direct
fetches hit CAPTCHAs at datacentre IPs at an unacceptable rate. The two supported backends,
selected per worker by the `RunConfig` builder:

- **MCP search server — the portable default, works against Stirrup's contract today.** A
  Streamable-HTTP MCP server fronting a search API (Tavily / Exa / Brave / self-hosted
  SearxNG), attached via `ToolsConfig.mcp_servers` (fully specified today: `name` / `uri` /
  `api_key_ref` Bearer), paired with `web_fetch` for page reads and a Chiron research prompt
  (`system_prompt_override` / `composed`) driving the search→read→synthesise loop. One search
  tool for **every** frontier model (uniform across `openai-responses` / `anthropic` /
  `gemini` — this is what makes "target any frontier model" real), and every search is an
  `mcp_search_*` tool_call the harness sees (full tracing, offload-to-file, per-turn budget).
  Chiron-only, no upstream change.
- **OpenAI provider built-in `web_search` — an OpenAI-path production option, gated on a
  Stirrup enablement.** OpenAI's Responses `web_search` tool runs on standard models
  (`gpt-5.5`), `tool_choice: auto|required`, returning server-side `web_search_call` items +
  `url_citation` annotations, with domain / recency / context-size controls. Operationally
  simplest *once available* — OpenAI runs the search loop, nothing to operate, robust to
  CAPTCHAs. But it is **not expressible in Stirrup's current contract**: `ToolsConfig` has no
  provider-built-in surface, and the `openai-responses` adapter *intentionally excludes*
  `web_search` / `file_search` ("the harness manages its own conversation history and does
  not delegate to server-side state", `docs/providers.md`). Enabling it is a bounded
  **Stirrup work-item** — add a `ToolsConfig` provider-built-in surface (opt-in), un-exclude
  `web_search` in the `openai-responses` adapter, and map `web_search_call` items →
  `HarnessEvent`s. Feasible because Stirrup is our repo; tracked as Stirrup work, it does not
  block the MCP path.

**Trade-off:** provider built-in is server-side, so the harness gives up per-search tool_call
visibility and the offload hook over raw results (the exact behaviour Stirrup excludes by
design), and it is OpenAI-specific (Claude / Gemini workers still need MCP). MCP keeps harness
visibility and cross-provider uniformity at the cost of running and paying for a search API.
**Sequencing: MCP-first** — it unblocks the worker with zero Stirrup change and serves every
model; the OpenAI-built-in track lands in parallel as a Stirrup enablement and becomes a
per-worker option when ready. This was the single highest-leverage decision in v2.

## 4. Topology — three contracts, one runner in the middle

```mermaid
flowchart TB
  CP[Chiron control plane<br/>chiron.v1 ConnectRPC ingress] -->|chiron.v1: ResearchRequest| RUN

  subgraph RUN[Chiron runner = run core + fleet Researcher]
    LEAD[Lead orchestrator<br/>decompose / synthesise / cite]
  end

  RUN -->|stirrup.harness.v1: task_assignment RunConfig| W1[Stirrup research job<br/>frontier model + web_search MCP]
  RUN -->|stirrup.harness.v1: task_assignment RunConfig| W2[Stirrup research job]
  W1 -->|HarnessEvent: tool_call / text_delta / done+trace| RUN
  W2 -->|HarnessEvent| RUN

  W1 -->|web_search MCP| SRCH[(Search API<br/>Tavily / Exa / SearxNG)]
  W2 -->|web_fetch| WEB[(Open web)]

  LEAD -->|plan + findings by reference| CS[(ContextStore<br/>in-memory -> Paddock)]
  W1 -->|offload-to-file -> findings ref| CS
  RUN -. baseline / stopgap .-> MDR[Managed deep research<br/>v1 Gemini DR adapter]
  RUN -. spans + cost .-> LF[OTLP -> Langfuse]
```

Three proto contracts are in play, and the **runner sits in the middle of two of them**:

1. `chiron.v1` (existing) — Chiron control plane ↔ Chiron runner. The runner is the
   **client** (outbound dial). Unchanged from `V2-PLAN.md` Wave 1.
2. `stirrup.harness.v1` (**new to Chiron**) — Chiron runner ↔ Stirrup research workers.
   The Stirrup harness dials outbound, so the **runner is the `HarnessService` server**:
   it accepts a worker's `ready`, sends a `task_assignment` (the research `RunConfig`),
   and consumes `HarnessEvent`s. Generated into Chiron with **Buf from Stirrup's proto**,
   never `go mod` imported (amend 4).
3. Paddock (D4) — structural/embedded `ContextStore`. Unchanged.

The runner being a `chiron.v1` client *and* a `stirrup.harness.v1` server is the clean
shape: Chiron schedules onto runners; runners schedule onto Stirrup jobs.

## 5. The research agent

### 5.1 Lead orchestrator (Chiron)

The lead is Chiron's pure-function control flow with model judgement at the decision
points — the v1 ethos ("use the LLM only when judgement is needed") applied to research:

1. **Decompose** the question into 3–5 worker briefs, each with the four Anthropic fields
   (*objective, output format, source guidance, boundaries*). Persist the plan to the
   `ContextStore` by reference.
2. **Dispatch** one Stirrup research job per brief (§5.2), bounded by the fan-out ceiling.
3. **Collect** each worker's findings *by reference* (§6.3) as they complete.
4. **Synthesise** the references into one report, then a **citation pass** producing the
   deduplicated `types.Citation` list the formatter already expects.

The lead's three judgement calls (decompose, synthesise, cite) are themselves
frontier-model calls. **SP-C (§5.5, resolved 2026-06-22) settles them as thin hand-rolled
`net/http` calls to one standard model — not Stirrup jobs — and settles the cross-worker
fan-out as Chiron-level, not `spawn_agent`.** The lead is Chiron's own control flow and
prompts; Stirrup owns only the per-worker agentic loop. The tentative "lead-as-Stirrup-jobs"
lean was investigated against Stirrup's actual contract and rejected; §5.5 records why.

### 5.2 Worker = a Stirrup research job

Each worker is one `RunConfig` sent as a `task_assignment`:

```
RunConfig{
  mode:             "research",                      // read-only, structurally
  prompt:           <the worker brief>,
  provider:         { type: "openai-responses" | "anthropic" | "gemini", ... },  // any frontier model
  tools: {
    built_in:       ["web_fetch"],                    // external-only: no workspace; NO write_file/run_command/edit_*
    mcp_servers:    [{ name: "search", uri: <web-search MCP>, api_key_ref: "secret://SEARCH_API_KEY" }],
    // search backend = MCP (default, this shape); OpenAI provider built-in web_search is the alt once the Stirrup enablement lands (§3)
  },
  permission_policy:{ type: "deny-side-effects" },    // required for read-only mode
  context_strategy: { type: "offload-to-file", max_tokens: ... },  // -> findings by reference (§6.3)
  max_turns, max_token_budget, max_cost_budget, timeout,            // per-worker structural caps
  system_prompt_override | prompt_builder:"composed", // Chiron's web-research prompt
}
```

The worker loops search→read→reason→synthesise inside Stirrup, emitting `tool_call`
(`mcp_search_*`, `web_fetch`) and `text_delta`, and finishes with `done` carrying a
`RunTrace`. Chiron reads the worker's synthesised finding plus its cited URLs.

### 5.3 Research-only proof

The invariant holds by construction and is asserted in tests: `mode:"research"` +
`permission_policy:"deny-side-effects"` + a `built_in` list with no write/exec tools +
no `executor:"local"`/`"container"` (use `"api"` read-only or none) + the Rule of Two
(`RuleOfTwoConfig`). A `permission_request` event from a worker is then a **defect** — a
worker tried to reach a side-effecting tool — and a test fails the run on it. Chiron never
sends `permission_response{allowed:true}` to a research worker.

### 5.4 Dispatch and provisioning — Chiron as Stirrup's control plane (SP-B, resolved)

Grounded in Stirrup `docs/deployment.md` + `docs/architecture.md` (read 2026-06-22): Stirrup
is "designed to be deployed as a short-lived Kubernetes job that talks to a long-running
control plane over gRPC", and "the harness *connects outbound* … there is no inbound port to
expose, no service mesh hop to configure, and no shared filesystem". So the **Chiron runner is,
precisely, Stirrup's `control plane`** — a long-running Deployment behind a ClusterIP Service
with two responsibilities:

1. **Wire (the `HarnessService` server).** A worker Pod runs the `stirrup job` entrypoint (no
   flags; all from env + the `RunConfig` delivered as the first `ControlEvent`), dials
   `CONTROL_PLANE_ADDR` (the runner's Service, e.g. `chiron-runner.chiron.svc:9090`), sends
   `ready`, blocks ≤5 min for `task_assignment`, runs, emits `done`+`RunTrace`, exits 0.
2. **Provisioning (K8s Job orchestration).** The runner creates one Job per worker via the K8s
   API, setting on the Pod: `CONTROL_PLANE_ADDR` (its own Service); a
   **`CONTROL_PLANE_SESSION_ID`** = the subtask/brief id (the worker echoes it in `ready`, so
   the runner matches each incoming stream to the brief it dispatched — the fan-out correlation
   key); `STIRRUP_FOLLOWUP_GRACE=0` (research workers are one-shot); and the provider/search
   `secret://` refs the `RunConfig` resolves via credential federation.

**Provisioning model: per-run Job is Stirrup's native shape** — the runner needs a K8s client
and RBAC to create/delete Jobs in the worker namespace. A pre-warmed pool is a later latency
optimisation, bounded by one-task-per-process and the 5-min pre-assignment timeout; default to
per-run Jobs. The published image `ghcr.io/rxbynerd/stirrup:<tag>` (distroless, nonroot uid
65532) is **pinned, not built by Chiron** — to a tag whose `stirrup.harness.v1` matches what
Chiron Buf-generates against. (Cloud Run Jobs is a documented GCP alternative with the same
outbound-dial semantics, if a non-GKE target ever matters.)

**Security (Chiron owns it).** Stirrup does **not** prescribe auth on `CONTROL_PLANE_ADDR` —
the runner's `HarnessService` endpoint must be secured by Chiron: an in-cluster `NetworkPolicy`
limiting who may dial, plus mTLS (mesh / cert-manager) or a bootstrap token, with the
`CONTROL_PLANE_SESSION_ID` correlation. Granting the runner RBAC to create Jobs is a real
privilege for a research-only component — scope it to a single worker namespace and a fixed Job
template, and flag it in the Wave 7 security review.

**Testing (the faked harness).** The fake is an in-process `HarnessService` *client* over a
bufconn / `httptest` pipe: it dials, sends `ready` (with a `CONTROL_PLANE_SESSION_ID`), waits
for `task_assignment`, replays scripted `HarnessEvent`s (`tool_call`/`text_delta`/`done`), and
never touches K8s — so `--agent research`/`fleet` run in CI with no cluster.

### 5.5 Lead substrate and fan-out — Chiron owns the orchestration (SP-C, resolved)

Grounded in Stirrup `main` (read 2026-06-22): `types/result.go:55-59`,
`harness/internal/core/subagent.go:46-179`, `harness/internal/tool/builtins/subagent.go`,
`harness/internal/prompt/systemprompts/planning.md`, `docs/deployment.md`,
`docs/architecture.md`. Both halves of SP-C resolve the same way — **Chiron owns the
orchestration shape; Stirrup owns only the per-worker agentic loop** — for one reason: every
Stirrup-internal alternative *hides* the per-unit visibility and control that v2's
spend-safety (§9) and observability (D7) are built on.

**Lead judgement calls — a thin hand-rolled `net/http` adapter to one standard model, not a
Stirrup job per call.** The lead's three calls (decompose, synthesise, cite) are *tool-less,
single-shot* model turns: no agentic loop, no search tool, no executor. Three contract facts
make the Stirrup-job substrate the wrong tool for them:

- **A Stirrup job is one process — one K8s Pod** (`docs/deployment.md`: `stirrup job` "runs
  the agentic loop to completion, and exits"; one Job → one Pod → one loop). Dispatching a job
  for a single tool-less turn pays a full pod-schedule + harness-loop start for zero agentic
  value, and serialises pod cold-start onto the lead's critical path three times per run, on
  top of the N worker pods.
- **Stirrup returns free text only.** `RunResult.FinalAssistantText` is the whole result, and
  its own comment says "callers that framed their prompt for JSON output parse this field"
  (`types/result.go:55-59`); there is no `response_format` / `output_schema` in `RunConfig`.
  So the Stirrup-job path gives the lead *no* structured-output safety, whereas a hand-rolled
  adapter can use the provider's **native** structured output (OpenAI Responses `json_schema`,
  Anthropic structured tool-use, Gemini `responseSchema` + `responseMimeType:
  application/json`) for decompose and cite — exactly the two calls that must be parsed
  deterministically into briefs and a `types.Citation` list. The hand-rolled path is strictly
  better on the axis that matters.
- **Stirrup's `planning` prompt is codebase-oriented** ("a planning agent with read-only
  access to the workspace… analyse the codebase… a numbered list… referencing the specific
  files and functions", `harness/internal/prompt/systemprompts/planning.md`), so it must be
  replaced with a `system_prompt_override` regardless; "leverage planning mode" buys little.

The cost traded away is real and bounded: one new small `net/http` client and its own keyless
auth (Stirrup's credential federation is not reachable from a Chiron-side HTTP client). Bound
it — the lead runs on **one** model (it need not be the frontier OpenAI model; Claude or
Gemini-Pro suffice), it reuses the v1 `internal/researcher/gemini` adapter's HTTP hardening
(cross-host-redirect refusal, `secret://` + `Scrub`, create-never-retried — §9 / `V2-PLAN.md`
§4), and its GKE auth rides the cheapest keyless path bound in Wave 7 (Gemini Vertex
`gcp-workload-identity`, or the Gemini-DR stopgap's Secret-Manager key). It is SDK-free, so the
"no vendor AI SDKs" non-negotiable holds. (Note the v1 Gemini adapter is *Deep-Research*-shaped
— planner / stream / follow-up — so this is a **new plain-generate client that borrows its
hardening**, not a reuse of the DR client itself.)

**Cross-worker fan-out — Chiron-level, not `spawn_agent`.** This half is not close. Stirrup's
`spawn_agent` (`harness/internal/core/subagent.go`) is built for a coding harness's
"explore in a clean context" use and breaks four things v2 depends on:

- **Sub-agents are invisible to the control plane.** A spawned sub-agent runs on a capture
  transport wrapping `NullTransport` — "no streaming to the control plane"
  (`subagent.go:46-104`). The Chiron runner *is* the control plane (SP-B); with `spawn_agent`
  it would see only the lead job's single `HarnessEvent` stream, every worker buried inside one
  process. That kills D7 / Langfuse per-worker spend rollup (`SpanDelegate`) and §9's "each
  worker emits its Stirrup `run_id` early as the per-worker resume handle".
- **Sub-agents share the parent's token/cost budget.** Per-worker structural caps
  (`max_turns` / `max_token_budget` / `max_cost_budget` / `timeout` per job, §9) collapse into
  one shared cap; the "bounded fan-out × per-job caps = hard ceiling" floor is lost.
- **Sub-agents inherit the parent's provider** — only prompt/mode/max_turns are overridable,
  and they reuse `parent.Provider` (`subagent.go:114-118,140-145`). Per-worker model choice
  (§6.1) shrinks to whatever the parent's `model_router` allows. Coarse.
- **It contradicts the resolved SP-B topology** (one K8s Job per worker, dialing the runner,
  correlated by `CONTROL_PLANE_SESSION_ID`). Chiron-level fan-out *is* that topology.

So the lead fans out at the Chiron level: one Stirrup `research` Job per worker (§5.2, §5.4),
each independently streamed, budgeted, resumable, and traced. `spawn_agent` is not used in v2;
the worker `built_in: ["web_fetch"]` list (§5.2) already excludes it structurally, keeping
per-worker accounting exact. (It is in Stirrup's default read-only tool set and passes
`deny-side-effects`, so the exclusion is a deliberate `ToolsConfig` choice, not a default —
worth an assertion in the research-only proof of §5.3.)

## 6. Standard-model targeting, auth, and findings-by-reference

### 6.1 Any frontier model

`provider.type` per job selects the model; the lead may run different workers on different
models (`providers` map + `model_router`). Default standard models: GPT-5.4/5.5 via
`openai-responses`, Claude via `anthropic`, Gemini-Pro via `gemini`. No managed
deep-research model appears here — those are §8 only.

### 6.2 Keyless auth (amend 1, grounded)

Stirrup's credential federation supplies keyless auth from GKE. Amend 1's "OpenAI Responses
via WIF" is resolved (author's choice) to **Azure OpenAI / Foundry Responses +
`azure-workload-identity`** — the wire-compatible `/openai/v1/responses` surface with an
Entra-ID bearer, *available in Stirrup today with no upstream change*. OpenAI-direct
Responses + a new `openai-wif` credential source (per OpenAI's GKE
Workload-Identity-Federation guide — a `gke-metadata` OIDC token exchanged for a
short-lived OpenAI token) is kept only as an **optional future alternative**, since it
needs a small addition to Stirrup's credential layer. Anthropic uses `anthropic-wif`;
Gemini uses `gcp-workload-identity` on Vertex — both keyless today. No static key in the
cluster, matching `V2-PLAN.md` Wave 7. SP-D (§10) is therefore a configuration confirmation
of the Azure path, not an open vendor choice.

### 6.3 Findings by reference

Map Stirrup's `offload-to-file` context strategy onto Paddock's blob plane: a worker's
large outputs are written once, content-addressed, and the worker returns a lightweight
`Reference` (the Anthropic "subagent-output-to-filesystem" pattern). The lead reads
references, never payloads, avoiding the multi-stage "game of telephone". In-memory
`ContextStore` first (D4), Paddock embedded later. SP-E (§10) confirms the offload-target
interop.

## 7. Managed deep research — stopgap worker + eval baseline

Per the author's choice, the managed agents are kept, demoted:

- **Stopgap — a top-level alternative researcher.** The existing v1
  `internal/researcher/gemini` Deep Research adapter stays selectable as its own
  `--agent gemini-deep-research` (exactly as in v1): a whole-run fallback while the
  homegrown agent matures. It is deliberately **not** a per-subtask worker the lead routes
  to — keeping it out of the fleet avoids a router branch and keeps the eval comparison
  (§8) clean. A run is either the homegrown Stirrup fleet or the managed stopgap, never a
  mix.
- **Eval baseline.** It is the yardstick. Amend 6's "the same sort of content as Gemini's
  research agents currently produce" becomes a literal, testable target: the homegrown
  agent's report is judged against the Gemini-DR report on the same question.

The managed adapter is reused as-is; v2 adds **no** new managed-deep-research provider
(the OpenAI `o*-deep-research` path from the drifted plan is dropped).

## 8. Quality bar and evaluation

The homegrown agent's risk is quality, not plumbing. Make it measurable with `stirrup-eval`
(the proposal already delegates evaluation to Stirrup):

- An eval suite of representative external-research questions; for each, run both the
  Chiron homegrown agent (`--agent fleet`, which contains no managed-DR worker — §7 — so
  the comparison is clean) and the Gemini-DR baseline (`--agent gemini-deep-research`).
- Judge on coverage, citation count/validity, and faithfulness — needing an **LLM-judge**
  suite judge (SP-F confirms availability in `stirrup-eval`; `docs/eval.md` lists
  `test-command`/`file-exists`/`file-contains`/`composite`, so an llm-judge may need adding
  upstream). **Fallback if it cannot land in `stirrup-eval` in time:** a Chiron-side judge
  (a hand-rolled `net/http` call scoring the two reports) so the v2 quality gate is never
  blocked on a Stirrup PR.
- **Gate:** the homegrown agent is not the default until it meets or beats the baseline on
  the suite. Until then the cheap single-call path (one Stirrup research job, or the
  managed stopgap) stays default; the multi-worker fleet is opt-in. This is the honest
  reading of "produce the same content as Gemini does".

## 9. Spend safety (budgeting still deferred — amend 2)

Budget *enforcement/attribution* stays deferred (`V2-PLAN.md` D6). But the homegrown design
*improves* the spend-safety floor for free:

- **Per-worker structural caps come from Stirrup**: each research job carries
  `max_turns`/`max_token_budget`/`max_cost_budget`/`timeout`; a runaway worker self-
  terminates (`stop_reason: budget_exceeded`/`timeout`). The lead's bounded fan-out × these
  per-job caps is a hard, if coarse, ceiling — without Chiron building any budgeting.
- **No create auto-retry / early id emission / poll-resilient await** carry over per worker
  (`V2-PLAN.md` §4); a Stirrup job's `run_id` is the per-worker resume handle.
- **Visibility over enforcement (amend 7)**: every worker's tokens/cost flow to Langfuse,
  per-worker and rolled up — the interim control, now richer because Stirrup emits its own
  OTel trace per job.

## 10. Spikes (these gate the re-seated waves)

These revive and refocus the prior `S2`/`S3` (Stirrup research-mode contract; routing),
which the drifted plan wrongly deferred.

| Spike | Question | Output |
| --- | --- | --- |
| **SP-A** (the big one) | **Web-search tool — RESOLVED (§3, 2026-06-22):** pluggable backend — an **MCP search server** (default, portable, works against Stirrup's contract today) + **OpenAI provider built-in `web_search`** (OpenAI-path option behind a Stirrup enablement); **native discounted** (datacentre CAPTCHAs). Remaining: prove the MCP search→read→synthesise loop reaches Gemini-DR grade, pick the search API (Tavily/Exa/Brave/SearxNG), and scope the Stirrup `web_search` enablement. | The worker `ToolsConfig` (MCP default) + research prompt; the Stirrup enablement work-item; recorded in `DECISIONS.md` when Wave 3 lands. |
| **SP-B** | **Stirrup dispatch — RESOLVED (§5.4, 2026-06-22):** the Chiron runner IS Stirrup's "control plane" (`deployment.md`) — a long-running Deployment + ClusterIP Service that serves `HarnessService` (workers dial in) and creates one K8s Job per worker (`stirrup job` entrypoint; `CONTROL_PLANE_ADDR` = runner Service; `CONTROL_PLANE_SESSION_ID` = brief id for fan-out correlation; image `ghcr.io/rxbynerd/stirrup:<tag>` pinned, not built). Per-run Job is the native model (runner needs a K8s client + RBAC); warm pool is a later optimisation. | The runner↔worker integration shape (§5.4); a faked-harness bufconn client for tests; the K8s Job orchestration + RBAC + endpoint auth land in Wave 7. |
| **SP-C** | **Lead substrate — RESOLVED (§5.5, 2026-06-22):** the lead's decompose/synthesise/cite are **thin hand-rolled `net/http` calls to one standard model**, not a Stirrup job per call (a job is one K8s Pod — all overhead, no agentic value for a tool-less single-shot turn; Stirrup returns free text only (`result.go:55-59`), so a hand-rolled adapter's native structured output is strictly better for decompose/cite; the `planning` prompt is codebase-oriented and needs overriding regardless). Cross-worker fan-out is **Chiron-level, not `spawn_agent`** — `spawn_agent` sub-agents are invisible to the control plane (NullTransport), share the parent budget, and inherit the parent provider, breaking per-worker visibility/caps/model-choice and the SP-B topology. | The lead implementation decision (§5.5); a small plain-generate `net/http` client reusing the v1 gemini adapter's HTTP hardening; recorded in `DECISIONS.md` when Wave 4 lands. |
| **SP-D** | **Azure OpenAI Responses + `azure-workload-identity` (chosen, §6.2):** confirm the project can register the Azure OpenAI/Foundry resource and Entra-ID workload-identity mapping, and that Stirrup's `azure-workload-identity` source binds it keylessly. OpenAI-direct + `openai-wif` is a recorded future alternative only. | The (configuration) auth binding for the OpenAI standard-model path; closes amend 1 with no Stirrup change. |
| **SP-E** | **Findings-by-reference interop.** Stirrup `offload-to-file` target → Paddock blob plane (and the in-memory store first). | The `ContextStore`↔Stirrup offload binding. |
| **SP-F** | **Eval judge.** Does `stirrup-eval` offer an LLM-judge for report-quality-vs-baseline, or must one be added? | The baseline eval suite + judge. |

SP-A, SP-B, SP-C have no spend and run first; they define the worker, the dispatch, and the
lead — all three are now resolved (§3, §5.4, §5.5).
**Cross-repo rule:** where a spike's resolution would require a change in Stirrup (a
provider-built-in/native `web_search` for SP-A, an `openai-wif` source for SP-D, an llm-judge
for SP-F), prefer the Chiron-only option (a search MCP, the Azure path, a Chiron-side judge
respectively) for the critical path, so v2 is never blocked on an upstream Stirrup release —
the Stirrup change rides alongside as a parallel track.

## 11. Impact on docs/V2-PLAN.md (what re-seats)

The hybrid plan: keep `V2-PLAN.md`'s scaffolding, re-seat its researcher core on this
design. Concretely, when `V2-PLAN.md` is revised:

- **D2** → researcher = Chiron lead orchestrating **Stirrup research jobs on standard
  frontier models + a web-search tool**; managed deep research demoted to a top-level
  stopgap researcher + the eval baseline (§7). Drop the `o3`/`o4-mini-deep-research`
  bindings — and with them Chiron's hand-rolled `internal/researcher/openai` adapter and
  `docs/RESPONSES-API.md`, which **are not built**: Stirrup owns the `openai-responses`
  provider adapter, so there is no Chiron OpenAI adapter to write or document. The OpenAI
  standard-model path is Azure OpenAI + `azure-workload-identity` via Stirrup (§6.2).
- **D3** (await) stays poll-first for the managed stopgap and the `chiron.v1` stream;
  worker await is the `stirrup.harness.v1` event stream (`done`/`error`), not provider
  polling — so the runner holds a live harness stream per worker, and the §9 / `V2-PLAN.md`
  §4 "a broken event stream never aborts a paid run" rule extends to that worker stream.
- **D5** → un-defer Stirrup (it is the harness); defer only internal *source tools*
  (`file_search`/repo-MCP). External web research via Stirrup is *in* v2. This **re-affirms
  `PROPOSAL §6`'s `Researcher = stirrup-fleet`** (which the drifted plan wrongly marked
  superseded); the transport is still ConnectRPC (D8).
- **D8** → gains a **second Buf target**, `stirrup.harness.v1`, generated alongside
  `chiron.v1`. The module wiring belongs in Wave 1 (the proto/codegen wave); the server
  implementation lands in Wave 3.
- **Spikes** → replace S1's OpenAI-deep-research framing with SP-A…SP-F here; S2 (Paddock)
  stays.
- **Wave 1** → adds the `stirrup.harness.v1` Buf module and generated server stubs to the
  proto/codegen deliverables (implementation deferred to Wave 3).
- **Wave 3** → "standard-model + Stirrup-harness integration": implement the
  runner-as-`HarnessService`-server, dispatch one research `RunConfig`, wire the web-search
  MCP (SP-A), and bind the credential-federation auth (Azure OpenAI + `azure-workload-
  identity`, `anthropic-wif`, Gemini; SP-D). Add a **faked Stirrup harness** as a
  first-class test artefact (the no-real-network fake, parallel to the faked Gemini/OpenAI
  clients). The v1 Gemini DR adapter is retained unchanged as the top-level stopgap +
  baseline; **no Chiron OpenAI adapter and no `RESPONSES-API.md`** are created.
- **Wave 4** → the fleet's workers are Stirrup research jobs; the router is external-web-only
  and never routes to the managed stopgap (which is top-level, §7); acceptance gains the
  eval-vs-baseline gate (§8) and the research-only proof (§5.3).
- **Waves 2, 5, 6** (Langfuse observability, control plane, Paddock) — unchanged in shape;
  Wave 5's advertised `researchers` capability list is
  `["research","fleet","gemini-deep-research"]` (single Stirrup worker, the fleet, and the
  managed stopgap; each maps to an `--agent`).
- **Wave 7** → GKE/auth/durability unchanged in shape, with two additions: the OpenAI path
  is Azure OpenAI + `azure-workload-identity` via Stirrup (not a hand-rolled token
  exchange), and v2 now ships and versions a **Stirrup worker image** the runner dispatches
  jobs to (provisioning model — pre-warmed pool vs per-run Job — is SP-B). Webhook
  completion applies to the managed stopgap only; workers complete over the harness stream.

## 12. References

- `github.com/rxbynerd/stirrup` (read 2026-06-22, `main`): `proto/harness/v1/harness.proto`
  (`RunConfig`, `ToolsConfig`, `MCPServerConfig`, `ContextStrategyConfig`,
  `PermissionPolicyConfig`, `HarnessService`); `docs/providers.md`;
  `docs/credential-federation.md`; `docs/eval.md`;
  `harness/internal/prompt/systemprompts/research.md`; `harness/harnessapi/harnessapi.go`.
- `docs/V2-PLAN.md` — the v2 implementation plan whose researcher core this re-seats.
- `docs/PROPOSAL.md` §6 — the Stirrup-fleet vision (external-via-managed-DR there is the
  stopgap this design replaces).
- `docs/PADDOCK.md` — the `ContextStore`/blob plane for findings-by-reference.
- Anthropic, *How we built our multi-agent research system* (2025-06-13),
  https://www.anthropic.com/engineering/multi-agent-research-system — the web-only
  orchestrator-worker pattern this agent follows, on standard models.
