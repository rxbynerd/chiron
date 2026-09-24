# Cycle 3 — specification compliance review (SPEC)

Branch `feat/v2-research-agent` at `d4ac9f2`, base `origin/main` (`926d7bf`).
Reviewer: spec-compliance. Every file named in the brief was read in full.

Specs checked:

- `docs/V2-PLAN.md`: §0 pivot banner, §1 D2/D6/D9, §2 invariants, §4 spend safety, §6 Wave 3 and Wave 4.
- `docs/V2-RESEARCH-AGENT.md` (V2-RA): §1, §3, §4, §5, §6, §8, §9, §10.
- The five dated 2026-07-01 entries added to `docs/DECISIONS.md` on this branch.
- `AGENTS.md` and `CLAUDE.md`.

**Important note on the brief.** `docs/V2-RESEARCH-AGENT.md` has no §0a, §5.3, §5.4 or §5.5. It was rewritten on 2026-07-01 in commit `d484da3`, and the model substrate now lives in §5 "Deliverables". This review treats §5 as the "§5.5 model substrate" the brief refers to. See C3-SPEC-12.

## Verification performed

```
go build ./...                       ok
go vet ./...                         ok
go test -count=1 ./...               all packages ok
go test -race ./internal/researcher/... ./internal/cli/...   ok
golangci-lint run ./...              0 issues
git diff origin/main...HEAD -- internal/run internal/researcher/gemini internal/interactions go.mod go.sum
                                     empty (untouched)
```

I built a binary into the scratchpad and ran the following commands.

```
$ chiron research --agent fleet --query q -o none
chiron: agent "fleet": not yet wired (v2 Wave 4 in progress)      exit=1
$ chiron research --agent worker --query q --plan --budget 0.01 \
    --fleet-model-endpoint https://api.openai.com/v1 --fleet-model-key-ref secret://NOPE -o none
chiron: research --agent worker: fleet.model_name is required     exit=1
```

The second command shows that `--plan` and `--budget` are accepted with `--agent worker`. The run was stopped only by the missing model name. See C3-SPEC-2.

---

## (a) Traceability — Wave 3 and Wave 4

### Wave 3 deliverables (V2-PLAN §6 Wave 3; V2-RA §5)

| Deliverable | Status | Evidence |
| --- | --- | --- |
| Thin `net/http` model adapter with native structured output, bounded, redirect-safe, never retried | Partial | `internal/researcher/fleet/model/{model,generate}.go`. Hardening is present. The structured-output schema the worker sends is probably rejected by OpenAI strict mode (C3-SPEC-4). There is no streaming API; each turn is one `Generate` call, which is acceptable. |
| In-process worker loop (search, read, reason, synthesise) with turn/token/cost/time caps | Partial | `worker.go`. Turn, token and time caps work. The cost cap can never fire (C3-SPEC-3). Citations are only gathered on `final` (C3-SPEC-6). |
| Research prompt | Implemented | `worker_prompt.go` |
| Streamable-HTTP MCP search client, bearer via `secret://` | Implemented | `search/{search,mcp,rpc}.go`. The search API choice required by SP-A is not made (C3-SPEC-8). |
| `web_fetch` | Implemented | `fetch/{fetch,do}.go`. Uses an SSRF guard; DNS pinning is deferred per DECISIONS. |
| `--agent worker` selectable | Implemented | `internal/cli/research.go:87-89`, `:132-228`, `TestWorkerAgentIsWired` |
| Fake search MCP and fake model transport | Implemented | `model/fake.go`, `search/fake.go`. Both ship in the non-test build (C3-SPEC-18). |
| Static-key `secret://` config for local dev | Implemented | `internal/config/config.go` `FleetConfig`. Keys are resolved at `research.go:171-183`. |
| AGENTS.md package map gains `fleet/{model,worker}`; security section gains model/search overrides | Partial | The model row is added but cites field names that do not exist. The search, fetch and worker rows are missing, and the fleet row is stale. Config fields replace env vars, a deviation recorded in DECISIONS (C3-SPEC-11). |
| DECISIONS entries: worker design + adapter; SP-A loop; research-only proof; single-provider auth; faked-MCP approach; "no Stirrup/OpenAI adapter/RESPONSES-API" | Partial | Four of the six are recorded. SP-A search-API choice, single-provider auth for dev versus GKE, the "no Stirrup harness" statement, and D6's effect on the worker are missing (C3-SPEC-19). |

### Wave 3 key tasks and acceptance criteria

| Criterion | Status | Evidence |
| --- | --- | --- |
| `chiron research --agent worker --query` with fakes produces a cited report through the unchanged run core | Implemented | CLI test `TestWorkerAgentIsWired` runs a final-only script. `TestWorkerGoldenReportThroughRunCore` runs search, fetch and final through `run.Run` against `testdata/worker-report.md`. |
| Research-only by construction, with a test | Implemented | `parseAction` closed switch (`worker_action.go:121-141`); `TestRunWorkerRefusesSideEffectingActions` |
| A model create/turn failure is attempted exactly once | Implemented | `TestNoRetryOn5xx` (model) and `TestRunWorkerNoRetryOnPaidTurn` (worker, `CallCount()==1`) |
| Gemini stopgap unchanged | Implemented | gemini, interactions and run show no diff; gemini tests pass |
| Worker tokens/cost forward to Langfuse | Missing | Wave 2 is absent. The worker's metrics are also dropped under OTel (C3-SPEC-5). |
| Wave gate: Wave 2 acceptance holds before Wave 3 | Not met | Waves 1 and 2 are absent from main and from this branch (C3-SPEC-13). |
| Per-worker spans and metrics use the Wave 2 vocabulary | Partial | `SpanWorker` was added (`internal/trace/names.go`). The reserved `SpanDelegate` and related names were not. |
| Refuse cross-host redirects on every credential-bearing client | Implemented | `model.refuseCrossHostRedirects`, `search.refuseCrossHostRedirects`, plus tests |
| Credentials scrubbed from logs, traces, stderr and errors (V2-RA §5 acceptance, §8) | Partial | The Interaction `StatusDetail` is tested (`TestWorkerScrubsModelCredential` and the search variant). No trace or span scrub test exists for the worker path. |

### Wave 4 (V2-PLAN §6 Wave 4; V2-RA §6)

| Deliverable or criterion | Status |
| --- | --- |
| `fleet/lead.go`, `router.go`, `synthesise.go`, `cite.go` | Missing (deferred) |
| `internal/memory/inmemory.go` | Missing (deferred). The `fleet.memory: inmemory` config value is already accepted (C3-SPEC-17). |
| Lead on the shared model adapter; decompose/cite via structured output | Missing (deferred). The adapter supports it (`Request.JSONSchema`). |
| Bounded worker pool; findings by reference | Missing (deferred). `RunWorker`/`Brief`/`Finding` are factored for reuse, which counts as preparation. |
| Eval-vs-baseline harness and judge (SP-F) | Missing (deferred) |
| `--agent fleet` wiring | Deferred, guarded by `checkResearcherWired` (`research.go:343-350`) |
| Fleet spans: decompose, delegate, synthesise, cite | Missing (deferred) |
| AGENTS.md map for `fleet/*`, `inmemory.go`, eval | Missing (deferred) |
| All Wave 4 acceptance criteria | Missing (deferred) |

---

## (b) Spend-safety invariants (V2-PLAN §4 and AGENTS.md "Money safety")

| Invariant | Worker path status | Evidence |
| --- | --- | --- |
| AGENTS: nothing creates before the `--budget` gate; the gate precedes plan | **Not honoured.** `--budget` and `--plan` are silently ignored for the worker. | `research.go:129-131` says "--plan and --budget ... do not apply here". `runWorkerResearch` never calls `gateBudget` or `reviewPlan`. V2-PLAN D6 leaves the gate "as-is — not extended", which justifies not *applying* it. It does not justify silently accepting the flags (C3-SPEC-2). |
| §4 / AGENTS: no create is auto-retried | Honoured | The model makes exactly one POST (`generate.go:154-155`). `tools/call` is single-attempt (`mcp.go:67-71`). The worker adds no retry (`worker.go:223-228`). All three are pinned by tests. |
| AGENTS: id emitted the moment `Start` returns | Mechanically honoured, but not a recovery handle | `Worker.Start` returns a `wkr_` id immediately (`worker_researcher.go:86-112`). `run.Run` emits `interaction_created`. The id cannot be recovered after a crash, which V2-RA §3 accepts. The CLI help still promises `chiron get <id>` recovers the run (C3-SPEC-7). The id does make a run *attributable* in the event stream and trace (`root.SetAttr("interaction_id")`). |
| §4: per-loop caps: turns, token/cost ceiling, wall-clock | Partial | Turns work: default 8 (`worker.go:194`). Time works: `WorkerTimeout` default 5m (`worker.go:125-129`). Tokens work but default to 0, meaning uncapped (`worker.go:197`). **Cost is inert** because `EstimatedCostGBP` is never set (C3-SPEC-3). Large fetched pages inflate every later turn (C3-SPEC-10). |
| §4: findings written by reference as it goes | Deferred to Wave 4 | There is no `ContextStore` binding. A crashed worker loses everything. |
| §4 / AGENTS: a broken stream never aborts a paid run | Honoured | Transport emission in the run core is unchanged and best effort. Worker tracing ignores a nil tracer and never errors. A failed tool call ends the worker as `failed`, which is by design and not a stream fault. |
| §4: bounded fan-out | Not applicable (Wave 4) | `MaxWorkers`/`Concurrency` are validated for `fleet` only (`config.go:302-312`) and are unused. |
| §4 / D7: spend visibility per call | Not honoured | Worker metrics are set on an ended span and dropped by OTel. There is no Langfuse forwarding, because Wave 2 is absent (C3-SPEC-5, C3-SPEC-13). |
| AGENTS: a non-terminal stdin can never approve `--plan` spend | Vacuous for the worker | No plan phase exists, so `--plan` is silently dropped (C3-SPEC-2). |

---

## (c) Code and documentation disagreements

These are indexed here and detailed in the findings below.

- **CLAUDE.md contradicts the pivot.** It says the runner is the `stirrup.harness.v1` HarnessService server and that Stirrup owns the adapters (C3-SPEC-1).
- **AGENTS.md is out of date.** The `internal/researcher/fleet` row still says "a stub that returns not-implemented". The package now hosts the real `Worker` (C3-SPEC-11).
- **AGENTS.md cites non-existent fields.** The model row names `Options.ModelEndpoint` and `ModelKeyRef`. The real fields are `Options.Endpoint` and `Options.APIKey` (C3-SPEC-11).
- **DECISIONS versus `worker_action.go`, part one.** The field lists match. The claim that "the provider `enum` bars a fourth kind on the wire" depends on a `strict: true` schema that OpenAI would probably reject (C3-SPEC-4).
- **DECISIONS versus `worker_action.go`, part two.** DECISIONS says partial citations are preserved on every outcome. The code only gathers citations on `final` (C3-SPEC-6).
- **V2-RA §3 and §5 versus `worker.go`.** The loop steps 1–6 match. Step 5, "stop when turn/token/cost/time caps stop the loop", is weakened in two ways: the cost cap is inert, and a token-cap truncation reports `failed` rather than `incomplete` (C3-SPEC-3, C3-SPEC-14).
- **V2-RA §3 versus the CLI help.** V2-RA §3 says not to claim `chiron get` recovers in-process runs. The `research` and `get` help, and `transport/events.go`, claim it does (C3-SPEC-7).
- **V2-RA §6 versus the trace code.** V2-RA §6 asks for per-worker spans carrying a worker id and a brief id. `SpanWorker` carries objective, status, turns and detail only (C3-SPEC-5).
- **Worker `Start` comment versus its behaviour.** The comment says cancellation is carried by Await's context. The run is detached and ignores Await cancellation (C3-SPEC-9).
- **Broken cross-references.** V2-PLAN, V2-AMENDS and STIRRUP-ENGINE-PROPOSAL cite V2-RA §0a, §5.3, §5.4, §5.5, §8 as the eval section, §10 as the spikes section, and §2–§11 as the Stirrup reference design. None of these exist. V2-PLAN Wave 5 still says workers complete over `stirrup.harness.v1` (C3-SPEC-12).
- **Stale code comments and help text.** `fleet.go` package doc, `FleetConfig.ModelName`, `--fleet-model-name` help, "a later wave requires them", and the "typed" error claim in DECISIONS (C3-SPEC-16).

## (d) Is the `--agent fleet` guard documented consistently?

No. The runtime guard itself is correct: it runs before any secret is resolved or request made, exits 1 and is pinned by `TestFleetAgentNotYetWired`. The documentation is inconsistent, as detailed in C3-SPEC-17:

- **Flag help.** `--agent` lists `fleet` as an ordinary choice with no caveat (`flags.go:19`).
- **Research help.** The long help for `research` mentions neither `worker` nor `fleet` (`commands.go:14-43`).
- **README.** It lists only the two deep-research tiers (`README.md:56`).
- **Config validation.** `Validate` accepts `fleet` and enforces fleet-only caps. `research-config --agent fleet` therefore emits a config that `research` will then refuse.
- **Two different "not implemented" surfaces.** `fleet.Fleet` returns "stirrup-fleet researcher is a v2 seam, not implemented in v1" (`fleet.go:22`). The CLI never constructs it and instead returns "not yet wired (v2 Wave 4 in progress)".
- **AGENTS.md** describes the stub as the fleet package's role. Its security section says "the `--agent worker`/`fleet` paths add a `fleet` config block" with no note that `fleet` does not run.
- **DECISIONS** (config entry) calls the guard a "typed" error. It is a plain `fmt.Errorf` with no sentinel.

---

## Findings

### C3-SPEC-1 — CLAUDE.md (added in this PR) tells agents to build the Stirrup-remote design the pivot dropped
**Severity:** High
**File:** CLAUDE.md:10-19

```
In v2 the `Researcher` is Chiron's own multi-agent fleet — a lead orchestrator driving **Stirrup**
(`github.com/rxbynerd/stirrup`) research-mode jobs on **standard frontier models** ...
Stirrup owns the provider adapters; its wire types are Buf-generated, never `go get`. The Chiron runner is the
`stirrup.harness.v1` `HarnessService` server (Stirrup workers dial in).
```

This contradicts the documents it calls binding:

- **V2-PLAN §0 and D9.** Stirrup is deferred to an *embedded engine*. `HarnessService` and K8s Jobs are "not built".
- **V2-PLAN D2.** Chiron hand-rolls the one-model adapter.
- **V2-RA §0.** "Do not build `stirrup.harness.v1`, a runner-as-harness server, or K8s worker Jobs for v2."

CLAUDE.md is loaded into every agentic session. It also names the wrong branch: it says `v2`, but the work is on `feat/v2-research-agent`. Wave 4 sessions will be told to build the wrong architecture.

**Fix:** Rewrite the paragraph to match the pivot. The fleet is an in-process lead over goroutine workers on one standard model, using Chiron's hand-rolled `internal/researcher/fleet/model` adapter plus a search MCP and `web_fetch`. Stirrup is deferred to an embedded-engine scale-out and is not built for v2. Remove the HarnessService sentence and correct the branch reference.
**Acceptance:** `grep -n "HarnessService\|Stirrup owns" CLAUDE.md` returns nothing. The paragraph agrees with V2-PLAN §0 and V2-RA §0.

### C3-SPEC-2 — `--plan`, `--accept-plan` and `--budget` are silently ignored on `--agent worker`; paid calls proceed ungated
**Severity:** High
**File:** internal/cli/research.go:129-146

```go
// ... --plan and --budget
// are Gemini-tier levers (a planning round and a per-tier cost estimate);
// the worker has neither, so they do not apply here.
func runWorkerResearch(cmd *cobra.Command, cfg config.ResearchConfig) error {
```

`runWorkerResearch` never reads `cfg.Plan`, `cfg.AcceptPlan` or `cfg.BudgetGBP`, and nothing rejects them. The binary check above shows `--plan --budget 0.01` accepted with `--agent worker`.

A user who passes `--budget 0.01` or `--plan` expects the documented contract:

- the `research` help says "blocked up front ... before any interaction is created";
- the same help says "before any research money is spent";
- AGENTS.md "Money safety" bullets 1 and 4 say the same.

Instead, paid model turns start immediately. V2-PLAN D6 defers budgeting and "does not extend" the gate. That supports not *implementing* a worker gate. It does not support silently accepting a spend-control flag that no longer does anything.

**Fix:** In `ResearchConfig.Validate` (or at the top of `runResearch`), reject `Plan`, `AcceptPlan` or `BudgetGBP > 0` when `Agent` is `worker` or `fleet`. Use a usage error that names the in-process caps (`--fleet-max-turns`, `--fleet-max-tokens`, `--fleet-worker-timeout`) as the available bounds. Update the `research` long help and AGENTS.md "Money safety" to state that the in-process agents have no budget gate and no plan phase under D6. Record the D6 application in DECISIONS.
**Acceptance:** Add a CLI test in which `research --agent worker --budget 1` and `--plan` each exit 1 with no request reaching a fake model server (`CallCount()==0`).

### C3-SPEC-3 — The GBP cost ceiling can never fire; `--fleet-ceiling` is a spend cap that caps nothing
**Severity:** High
**File:** internal/researcher/fleet/worker.go:200-202, 369-375; internal/config/flags.go:47

```go
if w.deps.Caps.CeilingGBP > 0 && w.usage.EstimatedCostGBP >= w.deps.Caps.CeilingGBP {
...
func (w *workerRun) accumulateModelUsage(u model.Usage) {
	w.usage.InputTokens += u.InputTokens
	w.usage.OutputTokens += u.OutputTokens
	// EstimatedCostGBP stays best-effort: ... it
	// remains 0 unless a future rate is wired in.
```

Nothing ever writes `EstimatedCostGBP`, so the ceiling check is dead code. The flag help ("per-worker estimated-cost ceiling in GBP; 0 means uncapped") and DECISIONS ("both are honoured: exceeding either ends the loop `incomplete`") both present it as a working cap. V2-PLAN §4 requires "a token/cost ceiling". An operator who sets `--fleet-ceiling 0.50` believes spend is capped. In fact it is bounded only by eight turns of uncapped tokens.

**Fix:** Choose one of the following.
- (a) Add a price input, for example `fleet.input_gbp_per_mtok` and `fleet.output_gbp_per_mtok` validated as non-negative. Compute `EstimatedCostGBP` in `accumulateModelUsage` and test that the ceiling fires.
- (b) Until a rate exists, reject a non-zero `ceiling_gbp` in `FleetConfig.validate` with a message pointing at `--fleet-max-tokens`, and correct the help and DECISIONS text.

**Acceptance:** Either a worker test in which a priced run stops `incomplete` with the "cost ceiling" detail, or a config test in which `ceiling_gbp: 0.5` is rejected.

### C3-SPEC-4 — The action schema and `max_tokens` probably fail against OpenAI's documented structured-output rules
**Severity:** High (not verified against the live API: this rests on OpenAI's published Structured Outputs constraints, and the fakes do not validate schemas)
**File:** internal/researcher/fleet/worker_action.go:60-96; internal/researcher/fleet/model/generate.go:81-86, 204-212

The request sends `response_format.json_schema.strict: true`. OpenAI strict mode requires every key in `properties` to appear in `required`, with optional fields expressed as nullable types. The schema only requires `["action"]` at the top level and `["url"]` on citation items:

```
"required": ["action"]            // query, url, answer, citations omitted
... "properties": {"url":..., "title":...}, "required": ["url"]
```

OpenAI documents a 400 "invalid schema" for this shape. The comment at `worker_action.go:53-56`, "per-action required fields are enforced in parseAction, not the schema", reflects a design that strict mode does not allow.

Separately, the wire type sends `max_tokens`. OpenAI's reasoning-model families, which include the GPT-5.x models CLAUDE.md names as targets, require `max_completion_tokens` and reject `max_tokens`. That field is sent whenever `fleet.max_tokens > 0`.

Result: the Wave 3 "provider-native structured output" deliverable probably fails on the first real turn against the reference provider. All CI uses `model.FakeServer`, which accepts any schema.

**Fix:**
- Make every property required. Type the per-action fields as `["string","null"]` and `citations` as `["array","null"]`, with `title` also required and nullable. Keep `parseAction` enforcing per-action presence and treat JSON `null` as absent.
- Send `max_completion_tokens`, or make the field name an adapter option recorded in DECISIONS.
- Add a unit test that walks `actionSchema` and asserts that every object's `required` equals its property keys and that `additionalProperties` is false.
- Record the provider compatibility target in DECISIONS.

**Acceptance:** The schema-walk test passes. Optionally, a manual smoke run against the chosen provider is recorded in the PR description.

### C3-SPEC-5 — Worker spend metrics are dropped under OTel, duplicate run-core names, and carry no worker id
**Severity:** Medium
**File:** internal/researcher/fleet/worker.go:140-152, 416-424

```go
span.End(nil)
emitWorkerMetrics(ctx, deps.Tracer, finding.Usage)
```

`trace.OTel.Metric` sets an attribute on `SpanFromContext(ctx)` (`internal/trace/otel.go:72-76`), and `ctx` here holds the worker span that has just ended. The OTel SDK's `recordingSpan.SetAttributes` returns early when `!s.isRecording()` (`go.opentelemetry.io/otel/sdk/trace/span.go:238-247`). Every worker token, search and cost metric is therefore discarded on the only binding that reaches Langfuse.

Under JSONL the metrics are written, but with the same names (`search_count`, `input_tokens`, ...) that `run.recordMetrics` writes again for the run. A name-keyed rollup therefore double-counts.

V2-RA §6 asks for per-worker spans with worker id, brief id, status, turns, tokens, searches and cost. The span has no id attribute. No test injects a recording tracer, so none of this is covered. This is the only per-worker spend signal while budgeting is deferred (D7).

**Fix:**
- Emit the metrics before `span.End`, or set them directly as span attributes.
- Add `worker_id` and `brief_id` attributes.
- Either namespace the worker metrics (for example `worker.input_tokens`) or document that the run-level metrics are the rollup and drop the duplicates.
- Add a test with a recording `trace.Tracer` fake that asserts the metrics land on the worker span before it ends.

**Acceptance:** The new test passes. A JSONL trace of a faked worker run shows the worker metrics attached to the `worker` span id, with no duplicate-name double count.

### C3-SPEC-6 — "Partial citations are preserved on all outcomes" is vacuous: citations exist only after `final`
**Severity:** Medium
**File:** internal/researcher/fleet/worker.go:311-321, 326-348; docs/DECISIONS.md (worker entry, "Partial citations gathered before any stop are preserved on all outcomes")

`addCitation` is called only from `finalise`. `incomplete()` and `failed()` return `w.citations`, which is always empty on those paths. The test file admits this at `worker_test.go:226-229`: "The worker gathers citations from final actions".

A turn-capped or failed worker therefore discards every URL it searched and fetched. V2-RA §8 requires that "partial worker failure is represented in the final interaction without losing completed findings". The Wave 4 lead will depend on this.

**Fix:** Record each successfully fetched page URL (the sanitised `page.URL`) as a provisional source during `doFetch`. On `final`, merge the model's citations with those sources, or keep them as a separate `Sources` list. Correct the Finding doc and DECISIONS to say exactly what is preserved.
**Acceptance:** A worker test in which search, fetch and then the turn cap fires yields an `incomplete` Finding whose citations include the fetched URL.

### C3-SPEC-7 — User-facing text promises `chiron get <id>` recovery for runs whose id is a non-durable local handle
**Severity:** Medium
**File:** internal/cli/commands.go:21-22, 82-87; internal/transport/events.go:12-14

```
The interaction id is emitted on stderr ... it is the resume handle for a crashed or timed-out run.
```

V2-RA §3: "Do not claim `chiron get <id>` can recover an in-process run across process death". For `--agent worker`, the emitted `wkr_…` id cannot be resumed. `chiron get wkr_…` would send that id to the Gemini API with the Gemini key and fail with a confusing remote error.

**Fix:** Qualify the `research` and `get` help, and the `KindInteractionCreated` comment: only Gemini deep-research ids are server-side resumable, and `wkr_` ids identify a run for attribution only. In `runGet` and `runFollowUp`, reject a `wkr_` prefix locally with a clear message before resolving any key.
**Acceptance:** A CLI test in which `get wkr_abc` exits 1 with the local message, no request reaches the Gemini fake, and the help text is updated.

### C3-SPEC-8 — SP-A's "pick the search API" is not done; the production search tool name is not configurable
**Severity:** Medium
**File:** internal/researcher/fleet/search/search.go:41-42, 78-82; internal/cli/research.go:196-200; docs/DECISIONS.md (search entry)

V2-PLAN §5 SP-A: "Remaining before Wave 3: ... pick the search API", with the output "recorded in DECISIONS.md when Wave 3 lands". The DECISIONS entry records only an *assumed* result shape `{"results":[{title,url,snippet}]}`.

The client defaults to tool `search` with argument `query`. `buildWorker` does not set `ToolName` or `QueryArgKey`, and `FleetConfig` has no field for them. Hosted search MCPs expose differently named tools, so a real deployment cannot reach its search tool without a code change. I have not verified the vendor tool names here.

**Fix:** Choose the SP-A backend and record it in DECISIONS, including the vendor's tool name, argument key and result shape. Add `fleet.search_tool` and `fleet.search_query_arg`, or hard-wire the chosen vendor's values. If the chosen vendor's result shape differs, map it in `parseResults`.
**Acceptance:** A DECISIONS entry names the backend. A config round-trip test covers the new fields, and `buildWorker` passes them through.

### C3-SPEC-9 — The detached worker run ignores cancellation and the `Start` comment says the opposite
**Severity:** Medium
**File:** internal/researcher/fleet/worker_researcher.go:100-110

```go
// ... Cancellation and the wall-clock bound are carried by Await's context and the per-worker
// Timeout cap inside RunWorker ...
go func() {
	st.finding = RunWorker(context.WithoutCancel(ctx), w.deps, brief)
```

Cancelling Await's context stops only the *wait*. The goroutine keeps making paid model turns until `WorkerTimeout` (default 5m) or `MaxTurns` stops it. In the one-shot CLI, process exit hides this. The Wave 5 runner requires "Cancel works" (V2-PLAN Wave 5), and there a cancelled run would keep spending.

The `runs` map is also never pruned, so a long-lived process grows it without bound.

**Fix:** Store a `context.CancelFunc` in `workerState`. Cancel it when Await's ctx is done, or expose an explicit cancel. Delete finished entries after `Result`. Correct the comment.
**Acceptance:** A test cancels Await's ctx and asserts the fake model sees no further turn after cancellation, and that the Finding is `incomplete` with a context detail.

### C3-SPEC-10 — Fetched pages up to 8 MiB are appended to the transcript and re-sent every turn; tokens are uncapped by default
**Severity:** Medium
**File:** internal/cli/research.go:201-204; internal/researcher/fleet/fetch/fetch.go:38; internal/researcher/fleet/worker_prompt.go:153-166; internal/config/config.go (`defaultFleet`, `MaxTokens` 0)

`buildWorker` leaves `fetch.Options.MaxContentBytes` at its default of 8 MiB of raw bytes, HTML included. `fetchedPageMessage` writes the whole body into a user turn, and every later turn re-sends the full transcript. `fleet.max_tokens` defaults to 0, meaning uncapped.

V2-RA §5 step 4 says "feed *bounded* page content back into the model". With the defaults, one large page can make each remaining turn close to maximum context. The run is then bounded only by eight turns and five minutes. That weakens the V2-PLAN §4 "hard, if coarse, ceiling" while budgeting is deferred.

**Fix:** Set a worker-appropriate `MaxContentBytes` in `buildWorker` (tens to low hundreds of KiB), or expose it as `fleet.max_page_bytes`. Consider a non-zero default `max_tokens`. Record the chosen bound in DECISIONS.
**Acceptance:** `buildWorker` passes an explicit page bound. A worker test with an oversize page shows the transcript message is capped and marked truncated.

### C3-SPEC-11 — AGENTS.md package map and "Money safety" are out of date
**Severity:** Medium
**File:** AGENTS.md:40-41, 66-82

- **Line 40.** Still says `internal/researcher/fleet` is "a stub that returns not-implemented". It now hosts the worker loop, action schema, prompt and the `Worker` Researcher.
- **Line 41.** Cites `Options.ModelEndpoint`/`ModelKeyRef`. `model.Options` has `Endpoint` and `APIKey`; the config fields are `fleet.model_endpoint` and `fleet.model_key_ref`.
- **Missing rows.** `internal/researcher/fleet/search` and `internal/researcher/fleet/fetch` have no rows, yet fetch carries the first SSRF-sensitive surface. The Wave 3 deliverable requires a worker row.
- **Money safety.** The section is unchanged. It says nothing about the in-process agents having no budget gate, no plan phase, a non-durable id, or the per-loop caps as the only bound.
- **Test conventions.** No carve-out is recorded for the exported `FakeServer`s (see C3-SPEC-18).

**Fix:** Update the fleet row. Add rows for `fleet/search` and `fleet/fetch`, noting the SSRF guard and that `AllowLoopback` is test-only. Correct the model row's field names. Add an in-process bullet set to "Money safety" covering one-attempt turns, the caps, no budget gate or plan (D6), and non-resumable ids.
**Acceptance:** Every package under `internal/researcher/fleet` has a row, every identifier AGENTS.md names exists (grep), and "Money safety" covers the worker path.

### C3-SPEC-12 — Broken cross-references into the rewritten V2-RESEARCH-AGENT.md, and Stirrup-stream residue in V2-PLAN
**Severity:** Medium
**File:** docs/V2-PLAN.md:4,14,49,107,110,114,126,139,252,261-265,555,833; docs/V2-AMENDS.md:10; docs/STIRRUP-ENGINE-PROPOSAL.md:20; docs/V2-PLAN.md:643-644, 664-666

V2-RA now has sections §0 to §11 with no subsections. Every one of the following cited targets is absent:

- §0a, the pivot rationale;
- §5.3, research-only;
- §5.4, SP-B;
- §5.5, the model substrate;
- §8 as the "eval" section (eval is §7);
- §10 as the "spikes" section (§10 is the checklist);
- §2–§11 as the "preserved Stirrup-remote design".

V2-PLAN Wave 5 also still says "Workers complete over the `stirrup.harness.v1` stream, not webhooks" (lines 643-644 and 664-666). That contradicts D3 and D9, under which workers are in-process.

**Fix:** Repoint each reference to the current section (§0, §1, §5, §7, §9), or restore the pivot rationale as a short §0a in V2-RA. Rewrite the two Wave 5 sentences to say that in-process workers complete in-process.
**Acceptance:** Every `V2-RESEARCH-AGENT.md §N` reference in `docs/` resolves to an existing heading, and `grep -n "complete over the \`stirrup" docs/V2-PLAN.md` is empty.

### C3-SPEC-13 — Wave 3 landed ahead of the Wave 1 and Wave 2 gates, with no recorded exception
**Severity:** Medium
**File:** docs/V2-PLAN.md:92-93, 449-451; internal/cli/research.go:549-555 (`newTracer`)

V2-PLAN §0 says "Waves are gated: do not start a wave until the previous wave's acceptance criteria are met". Wave 3 adds: "Do not begin this wave ... until Wave 2's acceptance holds ... Spend must be visible before it scales."

Neither the ConnectRPC transport (Wave 1) nor the Langfuse flags and reserved fleet vocabulary (Wave 2) exist. `newTracer` still binds OTel only from `OTEL_EXPORTER_OTLP_*`. V2-RA §10 step 1 allows proceeding "with fakes only". The tests comply with that. However, the production CLI can now spend on a real endpoint with no Langfuse path, and the metric pipeline it would use is broken (C3-SPEC-5). No DECISIONS entry records the out-of-order sequencing.

**Fix:** Add a DECISIONS entry recording that Wave 3 code landed before Waves 1 and 2 under the V2-RA §10 "fakes only" allowance. State that real-endpoint worker runs are not sanctioned until Wave 2 acceptance holds. Optionally gate `--agent worker` on a configured OTLP exporter until then.
**Acceptance:** The entry exists, and the PR description states the gating status.

### C3-SPEC-14 — A token-cap truncation is reported as `failed: invalid model action`, not `incomplete`
**Severity:** Low
**File:** internal/researcher/fleet/worker.go:217-238, 383-392

`remainingTokenBudget` shrinks `max_tokens`, down to a floor of 1, as the cap approaches. A truncated structured reply (`finish_reason: "length"`) is invalid JSON. `parseAction` then fails and the run ends `failed`. `resp.FinishReason` is never inspected.

V2-RA §5 step 5 expects a cap stop to read as a bounded stop, and `Caps` documents "Exceeding any of them ends the loop with StatusIncomplete".

**Fix:** When `resp.FinishReason == "length"`, return `w.incomplete("reached the N-token cap ...")` before parsing.
**Acceptance:** A worker test with `MaxTokens` set and a fake reply carrying `FinishReason: "length"` and truncated JSON yields `incomplete`.

### C3-SPEC-15 — Final-answer citations are not checked against the URLs the worker actually saw
**Severity:** Low
**File:** internal/researcher/fleet/worker.go:311-314

`fetch` is restricted to `seenURLs` as defence in depth, but `final` citations are accepted verbatim. A model can cite a URL it never searched or fetched, and the formatter will render it as a numbered source. V2-RA §7 judges "citation validity", and the prompt says "do not invent sources".

**Fix:** Keep citations whose URL is in `seenURLs` (or was fetched). Drop or flag the rest, for example by recording an unverified count in `Detail`.
**Acceptance:** A test in which a final citing an unseen URL produces a Finding without that citation.

### C3-SPEC-16 — Stale or contradictory code comments and help text
**Severity:** Low
**File:** see the list below

- **`internal/researcher/fleet/fleet.go:1-8`.** The package doc describes "parallel Stirrup jobs (internal sources) and Gemini Deep Research workers (external sources)" and says "every method returns ErrNotImplemented". Both contradict D2, D5 and D9 and the `Worker` now in the package.
- **`internal/config/config.go:137-138` and `flags.go:41`.** These say "empty selects the adapter's documented default", but `buildWorker` and `model.New` require a model name.
- **`config.go:129-130, 189, 275, 322`.** These say "a later wave resolves and requires them", but this PR requires them at the root (`research.go:156-170`).
- **`worker.go:146-150`.** The comment says a context error "is recorded as the span's error", but the code always calls `span.End(nil)`.
- **`worker_action.go:131-133`.** The comment says an empty final "maps ... to an interaction with a placeholder body". `mapInteraction` adds no placeholder; it omits `Outputs`, and any placeholder comes from the formatter.
- **DECISIONS config entry.** It calls the guard a "typed" error. It is a plain `fmt.Errorf` (`research.go:348`).

**Fix:** Bring each comment and help string into line with present behaviour. Rewrite the `fleet.go` package doc for the in-process worker and the pending fleet.
**Acceptance:** Each cited line reads correctly against the code.

### C3-SPEC-17 — The `--agent fleet` guard and the in-process agents are inconsistently documented to users
**Severity:** Low
**File:** internal/config/flags.go:19; internal/cli/commands.go:14-43; README.md:56; internal/researcher/fleet/fleet.go:22; internal/config/config.go:314-318

See section (d). In addition, `fleet.memory: inmemory` passes validation for both agents even though no in-memory `ContextStore` exists and the worker never reads `Memory`.

**Fix:**
- Mark `fleet` as "(not yet available)" in the `--agent` help and the README.
- Add `worker` to the README agent row and the `research` long help, with the fleet flags and the non-resumable id.
- Reject `memory: inmemory` until Wave 4, or document it as ignored.
- Either delete `fleet.Fleet` and its test, or make the CLI guard return `fleet.ErrNotImplemented`, so one message and one sentinel exist.

**Acceptance:** `chiron research --help` and the README describe `worker` and `fleet` accurately. `errors.Is(err, fleet.ErrNotImplemented)` holds for `--agent fleet`, or the stub is removed.

### C3-SPEC-18 — The test fakes ship in the production binary, and helper-owned servers go against AGENTS.md
**Severity:** Low
**File:** internal/researcher/fleet/model/fake.go; internal/researcher/fleet/search/fake.go; internal/researcher/fleet/worker_testservers_test.go:14-62

`go list -deps ./cmd/chiron | grep httptest` returns a match, so `net/http/httptest` is linked into `chiron`. DECISIONS accepts this. AGENTS.md "Test infrastructure conventions" still says "do not hide server lifecycle inside helper functions — the call site owns and varies the handler". The following all build handlers inside helpers:

- `FakeServer`;
- `newFetchPage`;
- `newBlockingModelServer`;
- `newLeakySearchServer`.

The call sites do own `Close`.

**Fix:** Either move the fakes into an internal `…/faketest` package imported only by tests, keeping httptest out of the binary, or add an explicit AGENTS.md carve-out that points at the DECISIONS entry.
**Acceptance:** `go list -deps ./cmd/chiron | grep -c httptest` returns 0, or AGENTS.md documents the exception.

### C3-SPEC-19 — Required Wave 3 DECISIONS entries are incomplete
**Severity:** Low
**File:** docs/DECISIONS.md (2026-07-01 entries)

V2-PLAN Wave 3 "Dependencies + DECISIONS.md" lists six required entries. Four are missing or partial:

- **SP-A.** The chosen web-search backend is not recorded (C3-SPEC-8).
- **Single-provider auth.** "Static key for dev, Wave 7 for GKE" is not stated.
- **Stirrup.** "No Stirrup harness" for v2 is not stated. Only "no full Responses adapter" appears.
- **D6.** Its effect on the worker is not recorded: no budget gate, no plan phase, and an inert GBP estimate (C3-SPEC-2, C3-SPEC-3).

**Fix:** Add a short dated entry covering the four points.
**Acceptance:** Each of the six items in V2-PLAN Wave 3 "Dependencies + DECISIONS.md" maps to a DECISIONS paragraph.

---

## Deferred / not in this PR

Each item below is from the plan and is not delivered by this PR.

- **Wave 1: ConnectRPC transport.** Generated `chiron.v1` code, `internal/transport/connect.go`, `CONTROL_PLANE_ADDR` validation and the submission/history RPCs (V2-PLAN §6 Wave 1).
- **Wave 2: Langfuse forwarding.** The `--otlp-endpoint`, `--langfuse-*` flags and ingestion headers (V2-PLAN §6 Wave 2 deliverables 1 and 2, key tasks 1 and 2).
- **Wave 2: reserved fleet vocabulary.** `SpanDecompose`, `SpanDelegate`, `SpanSynthesise`, `SpanCite` and the control-plane span, plus the synthetic single-worker test (V2-PLAN §6 Wave 2 deliverable 3 and acceptance 3).
- **Wave 2: span scrub test.** No credential or Langfuse key transits a span (V2-PLAN §6 Wave 2 key task 3; V2-RA §8).
- **Wave 3: Langfuse forwarding of worker spend.** Worker tokens and cost reaching Langfuse (V2-PLAN §6 Wave 3 acceptance 4; §4 "Spend visibility").
- **Wave 3: SP-A search backend.** The choice and its DECISIONS record (V2-PLAN §5 SP-A; §6 Wave 3 DECISIONS).
- **Wave 3: GBP cost estimation.** A per-model rate so `EstimatedCostGBP` and `ceiling_gbp` work (V2-PLAN §4 per-loop caps).
- **Wave 3: worker trace scrub test.** Model and search credentials must not leak into spans (V2-RA §5 acceptance, §8).
- **`web_fetch` DNS-rebinding hardening.** Connection-time IP pinning via `DialContext` (DECISIONS 2026-07-01 web_fetch entry, recorded follow-up).
- **Shared `internal/httpx` helper.** Redirect policy, bounded reads and the loopback scheme rule (DECISIONS 2026-07-01 model entry, recorded follow-up).
- **Wave 4: lead orchestrator.** `lead.go` with decompose into 3–5 four-field briefs and plan persisted by reference (V2-PLAN §6 Wave 4 key task 1; V2-RA §6 lead flow 1–3).
- **Wave 4: external-web-only router.** `router.go`, with the Gemini stopgap not a routing target (V2-PLAN Wave 4 key task 2; V2-RA §6).
- **Wave 4: worker pool.** Bounded concurrency under `max_workers`/`concurrency`, running goroutine workers in parallel (V2-PLAN Wave 4 key task 3; §4 bounded fan-out).
- **Wave 4: synthesis and citation pass.** `synthesise.go`/`cite.go` with a deduplicated `Citation` list across workers (V2-PLAN Wave 4 key task 4; V2-RA §6 lead flow 6–8).
- **Wave 4: in-memory ContextStore.** `internal/memory/inmemory.go` with `Put`/`Get`, `OpenSession`, TTL/GC and a `Remember`/`Recall` decision (V2-PLAN Wave 4 deliverable 2; D4; SP-E).
- **Wave 4: findings by reference.** Each worker's findings written as it goes, as the crash-recovery floor (V2-PLAN §4 bullet 3; Wave 4 key task 5).
- **Wave 4: fleet observability.** Decompose, delegate, synthesise and cite spans plus per-worker metrics rolled up per run (V2-PLAN Wave 4 key task 6; V2-RA §6 "Spend and observability").
- **Wave 4: eval gate.** The eval-vs-baseline harness and judge, and the single-call-default gate (V2-PLAN Wave 4 deliverable 4 and key task 8; SP-F; V2-RA §7).
- **Wave 4: `--agent fleet` wiring.** Replacing the `checkResearcherWired` guard with real construction (V2-RA §6 deliverables; V2-PLAN Wave 4 acceptance 1).
- **Wave 4: fleet golden report and in-memory store tests** (V2-RA §8 required fakes).
- **Wave 4: documentation.** The AGENTS.md map for `fleet/*`, `inmemory.go` and the eval suite, plus the five Wave 4 DECISIONS entries (V2-PLAN Wave 4 deliverable 5 and DECISIONS list).
- **Durable recovery of in-process runs across process death** (V2-RA §3; V2-PLAN Wave 7 key task 4).
- **Waves 5, 6 and 7, and the scale-out track** (V2-PLAN §6). These are out of scope and are listed for completeness.

## Verified OK

- **Run core untouched.** `internal/run`, `internal/researcher/gemini`, `internal/interactions`, `go.mod` and `go.sum` show no diff. The worker satisfies `researcher.Researcher` (`var _ researcher.Researcher = (*Worker)(nil)`).
- **No new dependency and no vendor AI SDK.** Everything uses stdlib `net/http`, `encoding/json` and `bufio`.
- **Research-only by construction.** The closed three-verb action enum, strict decoding (`DisallowUnknownFields`), and a dispatch switch with no execution default. `TestRunWorkerRefusesSideEffectingActions` covers shell, write, exec, unknown, missing discriminator and extra-field smuggling, asserting zero tool calls.
- **No paid-call retry.** The model POST, the search `tools/call` and the worker loop are each single-attempt, and all three are pinned by tests.
- **Endpoint validation.** `https://`, with `http://` for loopback only, is checked in config (`validEndpoint`) and again in `model.New` and `search.New`. Key refs must be `secret://` and are never echoed (`TestValidateErrorsNeverEchoSecrets`). Keys are resolved only at the composition root (`research.go:171-183`).
- **Cross-host redirect refusal** on the model and search clients. `web_fetch` re-checks its SSRF guard on every redirect, and `AllowLoopback` is false in production (`research.go:201-204`).
- **Bounded reads.** The model and search clients error on oversize. Fetch truncates and marks the result.
- **Worker `Start` returns immediately** (`TestWorkerStartReturnsImmediately`), and follow-up is rejected before any work starts.
- **`--agent fleet` runtime guard** fires before any secret or request and exits 1 (`TestFleetAgentNotYetWired`, plus the binary check).
- **Caps.** Turn and wall-clock caps stop the loop deterministically (`TestRunWorkerCapsStopDeterministically`, `TestRunWorkerTimeoutIsIncomplete`). A fetch of a URL the worker never saw is refused without a request (`TestRunWorkerRefusesUnseenFetchURL`).
- **Golden report through the unchanged run core** (`testdata/worker-report.md`).
- **Test conventions.** Table-loop variables are all named `tt` in the new tests. The build, vet, test, race and golangci-lint runs are green. No emojis were found in the added docs. The only US spellings are protocol identifiers such as `initialize` and `Authorization`.
