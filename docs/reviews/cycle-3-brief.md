# Remediation Brief — Chiron Cycle 3 (v2 research agent, PR #1)

**Scope:** v2 Wave 3 in-process research agent (`--agent worker`), the `fleet/{model,search,fetch}` clients, fleet config and CLI wiring, and the v2 planning docs; HEAD `d4ac9f2` on `feat/v2-research-agent` against `origin/main` (`926d7bf`)  
**Reviewers:** code · security · spec · tests · consistency · functionality · docs · verify  
**Date:** 2026-09-23  
**Raw findings:** 82 (21 code · 11 security · 19 spec · 14 tests · 3 consistency · 6 functionality · 6 docs · 2 verify)  
**After deduplication:** 35 (17 clusters merged from two or more reports; 18 single-report findings)  
**Distribution:** 1 Critical · 8 High · 15 Medium · 11 Low

**Re-ratings (highest reviewer rating used; disagreements noted):**

- C3-01 (orientation docs): docs rated Critical, spec rated High. Critical is used.
- C3-02 (unbounded spend): five reports rate their part High and five rate theirs Medium. High is used.
- C3-03 (detached worker, mid-turn stops): code and tests rate High, spec Medium, security and spec Low for sub-parts. High is used.
- C3-04 (SSRF): security and tests rate High; code and security rate sub-parts Medium or Low. High is used.
- C3-05 (worker metrics and trace): tests rate High, spec Medium, code Low. High is used, qualified: the High rests on the missing V2-RA §8 span-scrub test, not on data loss.
- C3-07 (broken cross-references): docs rate High, spec Medium. High is used.
- C3-10, C3-12, C3-14, C3-15, C3-16: one reviewer rated Medium and the others Low. Medium is used.

**Synthesizer observations:**

- **The functionality report's claim that the GBP ceiling stops the run is wrong.** Its Verified OK section says `MaxTurns`, `MaxTokens` and `CeilingGBP` were "independently verified to stop the run". Code, security, spec and tests all report that `EstimatedCostGBP` is never assigned. A grep confirms the field is only read, at `worker.go:200` and `worker.go:423`. Two knock-on effects follow. The functionality report's "risk is mitigated" remark about `--budget` does not hold. The verify report's option (b), mapping `--budget` onto `fleet.ceiling_gbp`, is not viable until a rate table exists. C3-02 adopts rejection instead.
- **Three design decisions need team-lead confirmation.** The brief takes a position on each so the implementer can proceed:
  1. Fetch and search tool errors become recoverable feedback (C3-08). The spec report calls the current hard failure "by design", and C3-TEST-10 proposes a test pinning it.
  2. A `finish_reason: "length"` truncation ends `incomplete`, not `failed` (C3-03). C3-TEST-9 proposes a test asserting `failed`; C3-CODE-5 and C3-SPEC-14 require `incomplete`.
  3. DNS-rebinding pinning lands in this PR (C3-04). DECISIONS and the spec report's deferred list treat it as a recorded follow-up. The security report rates it High because the production composition root ships the unpinned client.
- **No reviewer verified C3-06 against a live provider.** The strict-schema and `max_tokens` findings rest on OpenAI's published Structured Outputs rules. The functionality and verify reviewers had no network access. A manual smoke run against the chosen provider before merge would settle it.
- **The worker trace scrub test moves from deferred to in-PR.** The spec report lists "Wave 3: worker trace scrub test" as deferred. C3-TEST-2 rates the gap High. It is cheap once the metric ordering in C3-05 is fixed, so C3-05 carries it.
- **The team-lead brief cited V2-RA §5.5.** V2-RESEARCH-AGENT.md has no subsections since its 2026-07-01 rewrite. The model substrate is §5 (see C3-07).
- **Pattern:** every reviewer agrees on the structural safety properties. These are the closed action vocabulary, no paid retries, key scrubbing, endpoint validation and an untouched run core. The defects concentrate in three areas: stop and failure paths, spend bounding, and orientation docs. Most code fixes land in `worker.go`, `worker_researcher.go`, `fetch/fetch.go` and `config.Validate`.
- **Order of work:** C3-02, C3-03 and C3-08 all rewrite the loop's error and cap handling in `worker.go`, so do them in one pass. C3-06's per-turn clamp depends on C3-02's default token cap. C3-10 and C3-11 share a new fetched-URL set. C3-01's "Money safety" text depends on the C3-02 lever decision.

---

## Critical — fix first

---

### C3-01 — Orientation documents describe the abandoned Stirrup-remote design and omit the shipped worker
**(4-reviewer consensus: docs, spec, functionality, code)** — merges C3-DOC-1, C3-SPEC-1, C3-DOC-2, C3-SPEC-11, C3-FUNC-1, C3-CODE-18 (AGENTS.md item)  
**Files:** `CLAUDE.md:10-19` · `AGENTS.md:40-41, 66-82` · `README.md` · `examples/researchconfig/`

- **CLAUDE.md.** This PR added a v2 paragraph saying Stirrup owns the provider adapters and "the Chiron runner is the `stirrup.harness.v1` `HarnessService` server (Stirrup workers dial in)". That contradicts V2-PLAN §0, D2 and D9, and V2-RA §0 ("Do not build `stirrup.harness.v1`, a runner-as-harness server, or K8s worker Jobs for v2"). It also names the branch as `v2`; the work is on `feat/v2-research-agent`. CLAUDE.md loads into every agentic session, so Wave 4 sessions would be told to build the wrong architecture.
- **AGENTS.md package map.** Line 40 still calls `internal/researcher/fleet` "a stub that returns not-implemented", though it now hosts the wired `Worker`. Line 41 cites `Options.ModelEndpoint`/`ModelKeyRef`; the real fields are `Options.Endpoint` and `Options.APIKey`. There are no rows for `fleet/search` or `fleet/fetch`, although V2-PLAN:479-481 promised a worker row.
- **AGENTS.md "Money safety".** It is unchanged. It says nothing about the in-process agents having no budget gate, no plan phase, a non-durable id, and per-loop caps as the only bound.
- **README.** It has no mention of `worker`, `fleet`, any `--fleet-*` flag or the `fleet` config block. `examples/researchconfig/` has no fleet sample. An operator reading only the README cannot discover `--agent worker`.

**Fix:**
1. Rewrite the CLAUDE.md v2 paragraph to the prove-first shape. The fleet is an in-process lead over goroutine workers on one standard model, using the hand-rolled `internal/researcher/fleet/model` adapter plus a search MCP and `web_fetch`. Stirrup is deferred to an optional embedded-engine swap (V2-PLAN D9, V2-RA §9). Gemini Deep Research is stopgap and eval baseline only. Remove the HarnessService sentence and correct the branch name.
2. In AGENTS.md, split the fleet row so it no longer calls the package a stub. Add rows for `fleet/search` and `fleet/fetch`; the fetch row notes the SSRF guard and that `AllowLoopback` is test-only. Correct the model row's field names. Add in-process bullets to "Money safety": one-attempt turns, the caps, no budget gate or plan phase under D6 (per the C3-02 outcome), and non-resumable `wkr_` ids.
3. Add a README section and an `examples/researchconfig/fleet.yaml` covering the `--agent worker` flags. Include a worked example pointing `--fleet-model-endpoint` and `--fleet-search-endpoint` at real services. Mark `fleet` as not yet available.

**Acceptance criteria:**
- `grep -n "HarnessService\|Stirrup owns" CLAUDE.md` returns nothing, and the paragraph agrees with V2-PLAN §0 and V2-RA §0.
- Every package under `internal/researcher/fleet/` has an AGENTS.md row, and every identifier AGENTS.md names exists (grep).
- "Money safety" covers the worker path.
- A new operator can build a working `--agent worker` invocation from README.md alone.

---

## High — fix in this remediation wave

---

### C3-02 — Worker spend is effectively unbounded: 8 MiB pages re-sent every turn, no default token cap, an inert GBP ceiling, and `--budget`/`--plan` silently ignored
**(6-reviewer consensus: code, security, spec, tests, functionality, verify)** — merges C3-CODE-4, C3-CODE-8, C3-CODE-9, C3-SEC-2, C3-SPEC-2, C3-SPEC-3, C3-SPEC-10, C3-TEST-3, C3-TEST-6 (budget-flag item), C3-TEST-14 (ceiling item), C3-FUNC-3, C3-VERIFY-2  
**Files:** `internal/researcher/fleet/worker_prompt.go:153-167` · `fleet/fetch/fetch.go:38` · `internal/cli/research.go:125-146, 201-211` · `internal/config/config.go:190-197, 233-243` · `fleet/worker.go:197-202, 369-392` · `internal/config/flags.go:47`

This is the money path. V2-PLAN §4 requires a per-worker "token/cost ceiling" so that "a runaway worker self-terminates". As shipped, only the turn cap and the wall clock bound a run.

- **Transcript growth.** `buildWorker` leaves `fetch.Options.MaxContentBytes` at the 8 MiB default. `fetchedPageMessage` writes raw bytes, HTML included, into a user turn. There is no content-type filter, so binary bodies arrive with each invalid byte turned into U+FFFD. The whole transcript is re-sent every turn, and nothing stops a re-fetch of the same URL. Search results are bounded only by the 8 MiB MCP cap. The security report's PoC: a 3.5 MiB page fetched on turn 2 costs about 6M input tokens over an eight-turn run.
- **No default token cap.** `defaultFleet()` sets `MaxTokens` to 0, meaning uncapped (`worker.go:197`). `remainingTokenBudget` caps only the completion, so one turn's input can overshoot any cap. The token-cap stop has no test: `remainingTokenBudget` is at 33.3% and `totalTokens` at 0%. A probe by the tests reviewer shows the logic works today, sending `max_tokens` of `[120 70 20]` against a 120-token cap.
- **Inert GBP ceiling.** Nothing assigns `EstimatedCostGBP` (`worker.go:369-375`), so the check at `worker.go:200` never fires. DECISIONS says "both are honoured", and the `--fleet-ceiling` help presents it as a live cap.
- **Ignored levers.** `runWorkerResearch` never calls `gateBudget` or `reviewPlan`, and `Validate` does not reject the flags. The spec and verify reviewers both ran the binary: `--agent worker --budget 0.0001` completes and exits 0. `--accept-plan`, `--input`, `--mcp`, `--file-search`, `--tools`, `--template`, `--visualise` and `--model` are dropped too, and grounding documents passed with `--input` are discarded. The functionality report adds `--stream` and `--quiet`. D6 defers budget *enforcement*; it does not justify accepting a spend-control flag that does nothing. The research help and AGENTS.md "Money safety" promise the opposite.

**Fix:**
1. **Bound page text.** Set a worker page cap of 32–64 KiB in `buildWorker`, or truncate in `fetchedPageMessage`, and mark the result truncated. Accept only `text/*`, `application/json` and `application/xhtml+xml`; refuse other types as recoverable feedback (see C3-08). Strip HTML tags with the standard library. Better extraction is a follow-up. Cap snippet length and result count in `searchResultsMessage`. Deduplicate fetches of the same URL.
2. **Default token cap.** Ship a non-zero default `fleet.max_tokens`, for example 200k per worker. Before each turn, estimate the next request's input tokens as bytes divided by 4. Stop `incomplete` if the estimate plus accumulated usage would exceed the cap. The security report suggests also rejecting 0 without an explicit opt-out. Treat that as optional.
3. **GBP ceiling.** Reject a non-zero `ceiling_gbp` in `FleetConfig.validate` with a message pointing at `--fleet-max-tokens`. The per-model rate table is a follow-up. Correct the flag help and the DECISIONS "both are honoured" sentence.
4. **Levers.** In `ResearchConfig.Validate`, reject the following when `Agent` is `worker` or `fleet`: `Plan`, `AcceptPlan`, `BudgetGBP > 0`, `Inputs`, `MCP`, `FileSearch`, `Tools`, `Template`, `Visualise` and `Model`. Return a usage error that names the flag and the available in-process caps (`--fleet-max-turns`, `--fleet-max-tokens`, `--fleet-worker-timeout`). For `--stream` and `--quiet`, either reject them or scope their help text to the Gemini tiers. Update the `research` long help and record the D6 application in DECISIONS (see C3-16).

**Acceptance criteria:**
- A worker test with a 1 MiB fetched page shows the next model request's page message is at most the cap and marked truncated. A binary content-type page yields feedback, not bytes.
- `config.Default().Fleet.MaxTokens > 0`.
- `TestRunWorkerTokenCapStops` scripts ten search replies at 40 input and 10 output tokens against a 120 cap. It asserts Incomplete, a detail containing "120-token cap", three model calls and `MaxTokens` of `[120, 70, 20]`. A pre-turn estimate test stops before a turn whose estimated input would exceed the cap. `TestRemainingTokenBudgetFloorsAtOne` also passes.
- A config test rejects `ceiling_gbp: 0.5`.
- A config table test covers each rejected lever. A CLI test shows `research --agent worker --plan` and `--budget 1` each exit 1 with fake-model `CallCount() == 0`.

---

### C3-03 — The worker goroutine ignores the run deadline and cancellation, and a stop that lands mid-turn ends `failed` instead of `incomplete`
**(4-reviewer consensus: code, security, spec, tests)** — merges C3-CODE-2, C3-CODE-5, C3-SEC-9, C3-SPEC-9, C3-SPEC-14, C3-TEST-1, C3-CODE-17 (runs-map item)  
**Files:** `internal/researcher/fleet/worker_researcher.go:96-128` · `fleet/worker.go:185-192, 217-238, 258-297, 383-392`

- **The detached run drops the deadline and cancellation.** `Start` runs `RunWorker(context.WithoutCancel(ctx), …)`, which drops both. When Await's context ends, Await returns, but nothing cancels the goroutine. The comment's stated reason is wrong: the run core's Start context is not cancelled when Start returns (`run.go:132-133`).
- **Consequences of detaching.** Suppose `--timeout 2m` is set with the 5m `worker_timeout` default. Await fails, `run.Run` exits 1 with no report, and the finding and its usage are lost. Paid turns keep running until `MaxTurns` or `WorkerTimeout`. The `runs` map is never pruned. The Wave 4 lead and the Wave 5 "Cancel works" criterion both depend on this path.
- **Mid-call stops map to `failed`.** The context check that yields `incomplete` runs only at the top of the loop. A deadline that fires inside `Generate`, `Search` or `Fetch` maps to `w.failed(...)`. A probe by the tests reviewer with `Caps.Timeout` at 150ms returned `status=failed detail="model turn failed: … context deadline exceeded"`. `TestRunWorkerTimeoutIsIncomplete` pre-cancels the context, so it only reaches the top-of-loop check.
- **Truncation maps to `failed`.** `resp.FinishReason` is never inspected. A `length` truncation under the per-turn `max_tokens` produces partial JSON. `parseAction` rejects it and the run ends `failed: invalid model action`, contradicting the `Caps` contract ("ends the loop with StatusIncomplete"). Reasoning models make this likely.

**Fix:**
1. Derive the worker context so it keeps the parent's deadline and has its own cancel. Store the `CancelFunc` on `workerState` and call it from Await's `ctx.Done()` branch. Delete the `runs` entry after `Result` maps a finished run. Correct the comment. Clamp `Caps.Timeout` to the remaining run deadline in `buildWorker`, and have `Validate` reject `fleet.worker_timeout >= timeout`.
2. In `turn`, `doSearch` and `doFetch`, when a call errors and `ctx.Err() != nil`, return `w.incomplete("worker context ended: …")`.
3. In `turn`, when `resp.FinishReason == "length"`, return `w.incomplete(<token-cap reason>)` before parsing. This supersedes C3-TEST-9's proposed `TestRunWorkerTruncatedReplyFails`, which would assert `failed`.

**Acceptance criteria:**
- `Start` with a 200ms-deadline context: a blocking model server sees its request context cancelled within about 200ms.
- Cancelling Await's context: the fake model receives no further requests after a short grace period, and the Finding is `incomplete` with a context detail.
- A config test rejects `worker_timeout >= timeout`.
- `TestRunWorkerTimeoutDuringModelTurnIsIncomplete` and `TestRunWorkerTimeoutDuringSearchIsIncomplete` pass. The search variant also asserts turn-1 usage is preserved. Handlers drain the body first; see C3-29. `TestRunWorkerTimeoutIsIncomplete` stays green.
- A reply with `FinishReason: "length"` and truncated JSON under a `MaxTokens` cap yields `incomplete` with the cap in `Detail`.
- The `runs` map is empty after `Result`.

---

### C3-04 — `web_fetch` SSRF guard: DNS rebinding and proxy bypass, missing special-purpose ranges, context-blind lookups, and an untested hostname branch
**(3-reviewer consensus: security, code, tests)** — merges C3-SEC-1, C3-SEC-3, C3-SEC-8, C3-CODE-11, C3-CODE-12, C3-TEST-4  
**Files:** `internal/researcher/fleet/fetch/fetch.go:157-199` · `fetch/do.go:31-53` · `internal/cli/research.go:189-211` · `fetch/fetch_test.go:73-121, 215`

- **Resolve-then-check.** `guardHost` checks the answers from `net.LookupIP`, then `http.DefaultTransport` resolves the name again unchecked. There is no `Dialer.Control`. The production root ships this unpinned client (`research.go:201`), and every redirect hop has the same gap. `DefaultTransport` also honours `HTTP(S)_PROXY`, and the proxy re-resolves with its own resolver. The security report's PoC is a TTL-0 rebinding answer that alternates a public address with `169.254.169.254`. The worker then reads IMDS credentials into the transcript.
- **Denylist gaps.** `isInternal` admits CGNAT 100.64/10, which includes Alibaba metadata at `100.100.100.200`. It also admits NAT64 `64:ff9b::/96`, 6to4 `2002::/16` and IPv4-compatible `::/96` embeddings of internal addresses. Also admitted: 0/8 beyond `0.0.0.0`, 192.0.0/24, 198.18/15, 240/4, broadcast, `fec0::/10` and multicast. The security reviewer checked these empirically on Go 1.27.1.
- **Lookups ignore the context.** `net.LookupIP` runs before `context.WithTimeout` (`do.go:31` then `:35`) and inside `CheckRedirect`, so a slow resolver is unbounded.
- **Fetch timeout inherits the whole budget.** `buildWorker` sets the fetch `RequestTimeout` to `WorkerTimeout` (5m), replacing the 30s default. One slow page can consume the whole worker budget.
- **Untested hostname branch.** `guardHost` is at 42.9% and its DNS branch at 0%, yet production takes that branch for every search-result hostname. There is no resolver seam. The literal-IP tests assert only `err != nil`. If the guard regressed, they would really dial `10.0.0.1` or `169.254.169.254`.

**Fix:**
1. Give the fetch client its own `http.Transport` with `Proxy: nil` and a `net.Dialer` whose `Control` hook validates the actual peer IP with the shared classifier. Apply it even when a caller supplies an `HTTPClient`, or document that doing so forfeits the guard. Keep `guardHost` as the early, friendlier refusal.
2. Replace the denylist with deny-by-default on `netip.Addr`. Call `Unmap()`, and extract embedded IPv4 from NAT64 (`64:ff9b::/96`, `64:ff9b:1::/48`), 6to4 and IPv4-compatible forms. Admit only `IsGlobalUnicast()` addresses outside the IANA special-purpose ranges listed in C3-SEC-3. `AllowLoopback` relaxes loopback only.
3. Add a resolver seam, `lookupIP func(ctx, host)`, defaulting to `net.DefaultResolver`. Use the request context, and `req.Context()` in `CheckRedirect`. Move `WithTimeout` above the guard.
4. Keep fetch on its own short default, or add `fleet.fetch_timeout` clamped to at most `WorkerTimeout`.
5. Rewrite the DECISIONS 2026-07-01 `web_fetch` "follow-up" paragraph in the present tense.

**Acceptance criteria:**
- A rebinding test uses an injected resolver or dial hook that returns a public IP first, then `127.0.0.2` or `10.0.0.1`. `Fetch` fails with the dial-time refusal.
- With `HTTPS_PROXY` set, a fetch is not routed through the proxy.
- A table test refuses each of the following, with and without `AllowLoopback`, and asserts the error contains "internal":
  - `100.100.100.200`, `100.64.0.1`, `0.0.0.1`, `198.18.0.1`, `255.255.255.255`, `224.0.0.1`
  - `64:ff9b::a9fe:a9fe`, `64:ff9b::a00:5`, `::a9fe:a9fe`, `2002:a9fe:a9fe::1`, `fec0::1`
  - `[::ffff:10.0.0.1]`, `[::ffff:127.0.0.1]`, `[::ffff:169.254.169.254]`, `[fd00::1]`
- These tests pass:
  - `TestFetchHostnameResolvingInternalRefused`
  - `TestFetchHostnameMixedAnswersRefused`
  - `TestFetchRedirectToHostnameResolvingInternalRefused`
  - `TestFetchResolveErrorSurfaced`
  - `TestFetchEmptyHostRefused`
- A slow injected resolver makes `Fetch` return within `RequestTimeout`.
- SSRF tests use an `HTTPClient` whose `DialContext` fails the test, so no test can open a non-loopback socket. `guardHost` reaches 100%.
- A `buildWorker` test asserts the fetch timeout is shorter than the worker timeout.

---

### C3-05 — Worker metrics are emitted after the span ends, double-count run-level metrics, and no test covers the worker trace path
**(3-reviewer consensus: tests, spec, code)** — merges C3-TEST-2, C3-SPEC-5, C3-CODE-13  
**Files:** `internal/researcher/fleet/worker.go:140-152, 416-424` · `internal/trace/otel.go:72-76` · `internal/run/run.go:267-269`

- **Metrics after `End`.** `emitWorkerMetrics` runs after `span.End(nil)`. `trace.OTel.Metric` sets attributes on `SpanFromContext(ctx)`, which is the ended worker span, and the OTel SDK drops them (`sdk/trace/span.go:238-247`). OTel is the only binding that reaches Langfuse, and while D7 defers budgeting this is the only per-worker spend signal.
- **Double counting.** Under JSONL the same metric names are written again by `run.recordMetrics`, so a name-keyed rollup double-counts.
- **Wrong parent.** `SpanWorker` is a child of the already-ended `start` span, not `await`.
- **Stale comment.** The comment at 146-149 says a context error "is recorded as the span's error". The code always passes `nil`.
- **Missing attributes.** The span has no `worker_id` or `brief_id`, which V2-RA §6 asks for.
- **No tests.** Every fleet test passes a nil `Tracer`, and the CLI passes `trace.Noop{}`. There is no span-scrub test, which V2-RA §8 and §5 acceptance require. `RunWorker` is at 61.9% per package.

**Fix:** Emit worker metrics before `span.End`, or set them as span attributes such as `worker.search_count`, so the run core stays the sole emitter of the shared names. Add `worker_id` and `brief_id` attributes. Either pass the context error to `End` or correct the comment. Re-parent the span under `await` once C3-03 changes where the worker context comes from.

**Acceptance criteria:**
- `TestRunWorkerTraceScrubsCredentials` passes. It uses a `trace.NewJSONL(&buf)` tracer, a model 401 echoing a high-entropy key, and the leaky search variant. `buf` never contains the key and does contain `status: failed` with a scrubbed detail.
- `TestRunWorkerMetricsLandOnOpenSpan` passes. It uses the OTel tracer with `tracetest.SpanRecorder`, and the ended worker span carries `metric.search_count` and `metric.input_tokens`.
- A JSONL trace of a worker run through `run.Run` shows each metric name exactly once.
- `RunWorker` reaches 100% per package.

---

### C3-06 — The action schema is invalid under OpenAI strict mode, and `max_tokens` is the wrong wire field for reasoning models
**(2-reviewer consensus: code, spec)** — merges C3-CODE-1, C3-CODE-10, C3-SPEC-4  
**Files:** `internal/researcher/fleet/worker_action.go:53-96` · `fleet/model/generate.go:81-86, 204-213` · `fleet/worker.go:383-392`  
**Confidence:** Not verified against a live API. Both reviewers rely on OpenAI's published Structured Outputs constraints, and the fakes accept any schema.

- **Strict schema.** The client always sends `strict: true`. The schema requires only `["action"]` at the top level and `["url"]` on citation items. Strict mode requires every property key in `required`, with optional fields expressed as nullable types, and OpenAI documents a 400 for this shape. Every real `--agent worker` run against OpenAI would end `failed` on turn 1. The GPT-5.x models are a named target. The comment at `worker_action.go:53-56` describes a design strict mode forbids.
- **`max_tokens` field.** The wire type sends `max_tokens`. GPT-5-family and o-series models reject it in favour of `max_completion_tokens`.
- **Per-turn value.** `remainingTokenBudget` returns the whole remaining cumulative budget, so `fleet.max_tokens: 200000` sends `max_tokens: 200000` on turn 1. That exceeds every model's completion limit. This path is hit only when a cap is set, which C3-02 makes the default.

**Fix:**
1. Make every property required. Type the per-action fields as `["string","null"]` and `citations` as `["array","null"]`, and make citation `title` required and nullable. `parseAction` already treats JSON `null` as absent. Correct the comment.
2. Send `max_completion_tokens`, or make the field name an adapter option. Clamp the per-turn value to `min(remaining, per-turn maximum)`, for example via `fleet.max_output_tokens`.
3. Record the provider compatibility target in DECISIONS.

**Acceptance criteria:**
- A unit test walks `actionSchema` and asserts, recursively, that every object has `additionalProperties: false` and that `required` equals its property keys.
- A `parseAction` case `{"action":"search","query":"x","url":null,"answer":null,"citations":null}` decodes to a valid search.
- A model test asserts the wire body carries `max_completion_tokens`. A worker test with `MaxTokens` 200000 asserts the per-turn value is at most the clamp.
- Optionally, record a manual smoke run against the chosen provider in the PR description.

---

### C3-07 — Planning docs cite V2-RESEARCH-AGENT.md sections that no longer exist, and V2-PLAN Wave 5 still describes Stirrup-stream completion
**(2-reviewer consensus: docs, spec)** — merges C3-DOC-3, C3-SPEC-12  
**Files:** `docs/V2-PLAN.md:4,14,35,49,107,110,114,126,139,207,252,261-265,555,643-644,664-666,818,833,846` · `docs/V2-AMENDS.md:10` · `docs/STIRRUP-ENGINE-PROPOSAL.md:20,189`

V2-RA was rewritten on 2026-07-01 (`d484da3`) into sections §0–§11 with no subsections. V2-PLAN and STIRRUP-ENGINE-PROPOSAL still cite the old structure:

- §0a, the pivot rationale;
- §5.3, §5.4 and §5.5;
- §8 as the eval section, which is now §7;
- §10 as the spikes section, which is now the checklist;
- §2–§11 as the "preserved" Stirrup-remote design. Current §9 lists that design as explicitly deferred, the opposite of what the citation claims.

V2-PLAN Wave 5 also still says "Workers complete over the `stirrup.harness.v1` stream", which contradicts D3 and D9.

**Fix:** Repoint each citation to the current section (§0, §1, §5, §7 or §9), or restore a short §0a in V2-RA. Rewrite the two Wave 5 sentences to say that in-process workers complete in-process. Bump V2-PLAN's `date:`/`supersedes:` frontmatter to acknowledge the companion rewrite.

**Acceptance criteria:**
- Every `V2-RESEARCH-AGENT.md §N` reference under `docs/` resolves to an existing `##` heading.
- `grep -n "complete over the \`stirrup" docs/V2-PLAN.md` returns nothing.

---

### C3-08 — Any single fetch or search tool error fails the whole paid run
**(1-reviewer: code; corroborated by verify checklist items 6 and 7; the path is untested per C3-TEST-10)** — merges C3-CODE-3, C3-TEST-10 (fetch-failure item)  
**Files:** `internal/researcher/fleet/worker.go:258-262, 294-297` · `fleet/fetch/do.go:51-53`

`Fetch` returns an error in all of these cases, and each ends the run `failed` with no answer after several paid turns:
- any non-2xx response, including the common 403 bot-block, 404 and 401 paywall;
- DNS failure;
- a TLS error;
- an SSRF refusal;
- a non-http scheme in a search result.

Go's default `User-Agent` draws extra 403s. A search `isError` also fails the run: the verify reviewer saw "rate limited, try again later" end the run `failed`. The unseen-URL path at `worker.go:281-289` already shows the right pattern, feeding the error back to the model.

**Disagreement:** The spec report calls a tool failure ending the worker "by design". C3-TEST-10 proposes `TestRunWorkerFetchFailureIsFailed`, pinning today's behaviour. The code report argues this is the most common real-world outcome of fetching arbitrary search hits. The brief follows the code report. If the team lead keeps the failure fatal, record that in DECISIONS and write C3-TEST-10's test as specified.

**Fix:** Treat per-URL fetch errors as recoverable. Append `errorFeedbackMessage("fetching <url> failed: <scrubbed reason>")` and continue; the turn cap still bounds the loop. Apply the same rule to search `isError` and JSON-RPC errors. Transport failures against the configured search endpoint stay fatal, and context errors become `incomplete` per C3-03. A nil client stays fatal. Optionally set a descriptive `User-Agent` on fetch.

**Acceptance criteria:**
- A worker test scripts search, then a fetch of a result URL that returns 404, then final. The finding is `completed`, the turn-3 transcript contains the fetch-failure feedback, and the model call count is 3.
- A search `isError` followed by a final action ends `completed`.
- `TestRunWorkerNilFetchClientFails`, from C3-TEST-10, calls `RunWorker` with a nil `Fetch`. It asserts `failed` and zero fetch attempts.

---

### C3-09 — `--agent worker` cannot complete a fetch through the shipped CLI against any local double
**(1-reviewer: verify)** — C3-VERIFY-1  
**File:** `internal/cli/research.go:208-214`

`buildWorker` hardcodes `AllowLoopback: false`, with no flag, config field or environment override. Every `httptest.Server` binds to `127.0.0.1`. A scripted search-then-fetch through the real command tree therefore always ends `failed: fetch: refusing to fetch internal address 127.0.0.1`. The happy path is verified only by `TestWorkerGoldenReportThroughRunCore`, which builds the fetch client with `AllowLoopback: true`, a configuration the CLI never produces. Offline CI and eval environments cannot run `--agent worker` end to end.

**Fix:** The preferred option for this PR is documentation. State in AGENTS.md and V2-RESEARCH-AGENT.md that the fetch step cannot run against a local double through the compiled CLI, and name the golden test as the substitute. If an override is wanted instead, it must meet all of these:
- it is env-gated only, never a flag or config field, mirroring `CHIRON_GEMINI_BASE_URL` and the provenance rule in C3-17;
- it relaxes loopback only, never private or metadata ranges;
- it ships with a CLI e2e test.

**Synthesizer note:** An override widens the SSRF surface that C3-04 is tightening. The security reviewer did not assess one.

**Acceptance criteria:** Either the documentation statement is present, or the env-gated override lands with two tests. One CLI e2e test completes a fetch through a loopback page. The other shows the override does not admit `10.0.0.1` or `169.254.169.254`.

---

## Medium — fix in this wave

---

### C3-10 — Untrusted tool output is unframed, final citations are not checked against seen URLs, and the report body is verbatim model Markdown
**(3-reviewer consensus: security, code, spec)** — merges C3-SEC-5, C3-CODE-7, C3-CODE-16, C3-SPEC-15  
**Files:** `internal/researcher/fleet/worker.go:311-314` · `fleet/worker_prompt.go:120-167` · `fleet/worker_researcher.go:182` · `internal/formatter/markdown.go:127, 193` · `internal/cli/research_test.go` (`TestWorkerAgentIsWired`)

- **Unframed input.** Page bytes and snippets go into user-role messages with no delimiter, and the page-controlled `Content-Type` is echoed. The system prompt never says tool output is data. A page can imitate Chiron's own framing text.
- **Unchecked citations.** `finalise` adds every `act.Citations[].URL`. `TestWorkerAgentIsWired` shows it: `https://example.org/sky` is never searched or fetched yet appears under Sources. V2-RA §7 judges citation validity.
- **Verbatim report body.** The answer is not scrubbed, and remote images and raw HTML pass through. Opening the report in a Markdown preview fires an attacker beacon carrying the query. This is the exfiltration channel for anything C3-04 lets the worker read.

**Fix:** Wrap each tool result in a nonce-delimited fence with a fresh random nonce per run, stripping the nonce from content first. This is the security report's form; the code report's plain `<untrusted_page>` marker is weaker. Add a system-prompt rule that fenced text is data, never instructions. In `finalise`, keep only citations whose URL was fetched, or at least appeared in `seenURLs`. Record the dropped count in `Detail` or a span attribute. Before mapping, rewrite `![alt](url)` to a plain link or drop it, strip raw HTML tags, and run `secret.Scrub`. Adjust the CLI smoke test to search first, or to expect the unseen URL to be dropped.

**Acceptance criteria:**
- A final action citing one seen and one unseen URL yields a Finding with only the seen URL.
- A fake page carrying the PoC injection text yields a report with no `![` remote image.
- A prompt test shows page content enclosed in the nonce fence, and that an embedded closing fence cannot terminate it early.

---

### C3-11 — "Partial citations are preserved on all outcomes" is vacuous: citations exist only after `final`
**(3-reviewer consensus: code, spec, tests)** — merges C3-CODE-6, C3-SPEC-6, C3-TEST-13 (first item)  
**Files:** `internal/researcher/fleet/worker.go:311-363` · `docs/DECISIONS.md` (worker entry) · `fleet/worker_test.go:220-229`

`addCitation` is called only from `finalise`, so `incomplete` and `failed` Findings always carry `Citations: nil`. Three documents claim otherwise: the `Finding`/`RunWorker` docs, the DECISIONS worker entry ("a bounded or failed run never discards the sources it found") and V2-RA §8. The Wave 4 lead depends on this. The test comment in `TestRunWorkerCapsStopDeterministically` admits the gap but claims an assertion the test does not make.

**Fix:** Record each successfully fetched page, using the sanitised `page.URL` and a title if known, as a provisional source in `doFetch`. Surface these on `incomplete` and `failed`. On `final`, merge them with the validated model citations from C3-10, or keep a separate Sources list. Correct the Finding doc, DECISIONS and the test comment to say exactly what is preserved.

**Acceptance criteria:**
- Search, fetch, then a model 5xx: the `failed` Finding carries the fetched URL.
- Search, fetch, then the turn cap: the `incomplete` Finding carries the fetched URL.

---

### C3-12 — CLI help, flag help and validation misdescribe the worker and fleet agents
**(3-reviewer consensus: functionality, spec, code)** — merges C3-FUNC-2, C3-FUNC-5, C3-SPEC-17, C3-CODE-18 (ModelName item), C3-SPEC-16 (ModelName item)  
**Files:** `internal/config/flags.go:19, 41` · `internal/config/config.go:137-138, 314-318` · `internal/cli/commands.go:14-43` · `README.md:56` · `internal/researcher/fleet/fleet.go:22` · `internal/cli/research.go:343-350`

- **Model name.** `--fleet-model-name` help and the `FleetConfig.ModelName` comment say "adapter default if unset". `buildWorker` requires the field, and the adapter has no default; the functionality reviewer confirmed this live.
- **Agent help.** `--agent` lists `fleet` as an ordinary choice. The `research` long help mentions neither `worker` nor `fleet`, and the README agent row lists only the two Gemini tiers.
- **Fleet config.** `research-config --agent fleet` emits a config that `research` refuses. `fleet.memory: inmemory` passes validation, although no in-memory store exists and the worker never reads `Memory`.
- **Two "not implemented" surfaces.** `fleet.Fleet` returns `ErrNotImplemented`, while the CLI guard returns a plain `fmt.Errorf`. DECISIONS calls the guard "typed".

**Fix:**
- Change the model-name help to "required for `--agent worker`/`fleet`".
- Mark `fleet` "(not yet available)" in the `--agent` help and the README.
- Add `worker` to the README agent row and the `research` long help, with the fleet flags and the non-resumable id.
- Reject `memory: inmemory` until the in-memory store exists.
- Unify the not-implemented surface. Either delete the `fleet.Fleet` stub and its test, or have the CLI guard return `fleet.ErrNotImplemented` with wording per C3-15.

**Acceptance criteria:**
- `chiron research --help` and the README describe `worker` and `fleet` accurately.
- `errors.Is(err, fleet.ErrNotImplemented)` holds for `--agent fleet`, or the stub is removed.
- A config test rejects `memory: inmemory`.

---

### C3-13 — `chiron get` and `follow-up` send `wkr_` ids to Gemini, and the help promises a recovery that cannot happen
**(2-reviewer consensus: spec, functionality)** — merges C3-SPEC-7, C3-FUNC-4  
**Files:** `internal/cli/commands.go:21-22, 82-87` · `internal/transport/events.go:12-14` · `internal/cli/research.go:233-260` (`runGet`, `runFollowUp`)

The help and the `KindInteractionCreated` comment call the emitted id "the resume handle for a crashed or timed-out run". V2-RA §3 says not to claim that for in-process runs. `runGet` and `runFollowUp` build the Gemini researcher unconditionally. With `GEMINI_API_KEY` set, `chiron get wkr_…` makes a real Gemini call with a nonexistent id. Without the key, it fails with an unrelated secret error.

**Fix:** In `runGet` and `runFollowUp`, reject a `wkr_` prefix locally, before resolving any key, with a message naming the in-process resume limitation. Qualify the `research` and `get` help and the `KindInteractionCreated` comment: only Gemini deep-research ids are server-side resumable.

**Acceptance criteria:**
- `chiron get wkr_abc` and `chiron follow-up wkr_abc` each exit 1 with the local message, whether or not `GEMINI_API_KEY` is set.
- No request reaches the Gemini fake.
- The help text is updated.

---

### C3-14 — `parseAction` accepts trailing data, discards refusals, and its malformed-output branches are untested
**(2-reviewer consensus: tests, code)** — merges C3-TEST-9, C3-CODE-14  
**Files:** `internal/researcher/fleet/worker_action.go:109-141` · `fleet/model/generate.go:104-110`

`dec.Decode` reads only the first value, so `{"action":"final","answer":"x"} {"action":"shell"}` decodes as a final. The tests reviewer confirmed this, which contradicts the "decoded strictly" comment. A structured-output refusal (`message.refusal` set, `content` null) surfaces as "model returned an empty action", losing the provider's reason. Three branches are uncovered: an empty or whitespace reply, `search` with no query, and `fetch` with no URL. `parseAction` is at 81.2%.

**Fix:** After `Decode`, require a second `Decode` to return `io.EOF`, or `dec.More() == false`. Parse `choices[0].message.refusal` and surface it as a scrubbed failure detail. Add `TestParseAction` as a `tt` table covering:
- `""` and whitespace;
- `{"action":"search"}` and a blank query;
- `{"action":"fetch"}`;
- prose, a Markdown-fenced object and a truncated object;
- trailing data after a valid object;
- positive cases for all three kinds, including a final with empty citations.

The truncated-reply worker test belongs to C3-03 and asserts `incomplete`.

**Acceptance criteria:**
- `parseAction` reaches 100%.
- Trailing data is rejected.
- A model test surfaces the refusal text.

---

### C3-15 — "Wave N" planning labels and forward-looking narrative in code comments and a user-facing error
**(2-reviewer consensus: consistency, code)** — merges C3-CONS-1, C3-CODE-19  
**Files:**
- `internal/trace/names.go:18`
- `internal/config/config.go:44-45, 122, 186`
- `internal/cli/research.go:82, 340-342, 348`
- `internal/researcher/fleet/worker.go:20-25, 29, 48, 115, 373, 406`
- `fleet/worker_researcher.go:20-21, 29`
- `fleet/worker_prompt.go:88`
- `fleet/model/fake.go:12-13` and `fleet/search/fake.go:12, 39`
- `research_test.go` (`TestFleetAgentNotYetWired`)

The code-comment policy bars speculation about future change and session vocabulary. About thirteen comments narrate what "Wave 4's lead" will do, and others use "chunks", "now wired" or "a future rate". The v1 milestone tags (`M2`, `M4`, `M5`) are different: they label behaviour that has already shipped. `research.go:348` also puts the label in a user-visible error: `not yet wired (v2 Wave 4 in progress)`. Several doc comments run 6–12 lines of rationale that DECISIONS already holds:
- `worker_action.go:9-16, 49-59, 98-108`
- `fetch.go:142-156`
- `model.go:108-113`
- `search.go:111-116`

**Fix:** Rewrite each comment in the present tense, stating what the code does and its invariant. Move sequencing and rationale to V2-PLAN or DECISIONS, and keep doc comments to one to four lines. Reword the guard error in the style of `fleet.go:22`, for example `agent %q: the fleet orchestrator is not wired yet`, and coordinate the sentinel with C3-12.

**Acceptance criteria:**
- `grep -rnE "Wave [0-9]|chunk|will be|now wired|future"` over the changed Go files, test files included, finds no comment matches.
- The guard error carries no wave or version label.

---

### C3-16 — Wave 3 landed ahead of the Wave 1 and Wave 2 gates, and required Wave 3 DECISIONS entries are missing
**(2-reviewer consensus: spec, docs)** — merges C3-SPEC-13, C3-DOC-5, C3-SPEC-19  
**Files:** `docs/V2-PLAN.md:92-93, 449-451` · `docs/DECISIONS.md` (2026-07-01 entries) · `internal/cli/research.go:549-555` (`newTracer`)

V2-PLAN gates Wave 3 on Wave 2: "Do not begin this wave … Spend must be visible before it scales". None of the following exists: the ConnectRPC transport, the Langfuse flags or the reserved fleet vocabulary. `newTracer` binds OTel only from `OTEL_EXPORTER_OTLP_*`. V2-RA §10 step 1 allows proceeding with fakes only, and the tests comply. The production CLI, however, can now spend on a real endpoint with no Langfuse path, and no record explains the order.

Four of the six DECISIONS entries V2-PLAN Wave 3 requires are also missing or partial:
- the SP-A search backend;
- single-provider auth: a static key for dev, Wave 7 for GKE;
- "no Stirrup harness" for v2;
- D6's effect on the worker: no budget gate, no plan phase and no GBP estimate.

**Fix:** Add one dated DECISIONS entry. It records that Wave 3 landed before Waves 1 and 2 under the V2-RA §10 fakes-only allowance. It states that real-endpoint worker runs are not sanctioned until Wave 2 acceptance holds. It also covers the four missing items, recording SP-A as open and pointing at the follow-up issue. Optionally, gate `--agent worker` on a configured OTLP exporter. State the gating status in the PR description.

**Acceptance criteria:**
- Each of the six V2-PLAN Wave 3 DECISIONS items maps to a paragraph.
- V2-PLAN's gate language agrees with what DECISIONS records.

---

### C3-17 — A config file can choose both the credential and its destination
**(1-reviewer: security; medium confidence, depending on how far config files are trusted)** — C3-SEC-4  
**Files:** `internal/config/config.go:363` · `internal/cli/commands.go:167-176` · `internal/secret/resolve.go` · `fleet/model/generate.go:150`

In v1 a config file could choose which `secret://` reference to resolve, but the destination was env-only (`CHIRON_GEMINI_BASE_URL`). In v2, `fleet.model_endpoint` and `fleet.model_key_ref` both come from `--config` or piped stdin. `secret://NAME` resolves any environment variable, and `secret://file/<abs>` any readable file. The value is sent as a bearer token to any https host. For example, a shared `research.yaml` naming `secret://AWS_SECRET_ACCESS_KEY` and an attacker gateway exfiltrates on the first turn. `fleet.search_endpoint` and `search_key_ref` form a second channel.

**Fix:** Pick one option and record it in DECISIONS:
- **(a) Provenance.** Fleet endpoints come only from a flag or `CHIRON_FLEET_*_ENDPOINT` env var. A base config naming an endpoint is rejected.
- **(b) Binding.** When an endpoint comes from a base config, its key ref must come from a flag, or the host must be on an allowlist.
- **(c) Namespacing.** Env refs must start with `CHIRON_`, and file refs must sit under a Chiron config directory.

Option (a) mirrors the v1 rule and is the smallest change. In every case, log the endpoint host on stderr before the first paid call. Update the AGENTS.md security section to match (see C3-01).

**Acceptance criteria:**
- A config test shows a base config with a disallowed `model_endpoint` plus `model_key_ref` fails validation before any `secret.Resolve` call.
- A CLI test with a counting fake resolver proves the resolver is never invoked.

---

### C3-18 — The SP-A search backend is not chosen, and the search tool name and argument key are not configurable
**(1-reviewer: spec)** — C3-SPEC-8  
**Files:** `internal/researcher/fleet/search/search.go:41-42, 78-82` · `internal/cli/research.go:196-200` · `docs/DECISIONS.md` (search entry)

V2-PLAN §5 SP-A requires the search API choice to be recorded when Wave 3 lands. DECISIONS records only an assumed result shape. The client defaults to tool `search` with argument `query`, and neither `buildWorker` nor `FleetConfig` can override them. Hosted search MCPs use different names. The spec reviewer did not verify vendor tool names.

**Fix:** Choose the backend and record its tool name, argument key and result shape in DECISIONS. Add `fleet.search_tool` and `fleet.search_query_arg`, or hard-wire the chosen vendor's values, and map the result shape in `parseResults` if it differs. **Disposition:** follow-up, because the backend choice is a product decision. The two config fields are cheap enough to land in this PR if wanted.

**Acceptance criteria:**
- A DECISIONS entry names the backend.
- A config round-trip test covers the new fields, and `buildWorker` passes them through.

---

### C3-19 — Worker tests never assert what the model is sent
**(1-reviewer: tests)** — C3-TEST-5  
**File:** `internal/researcher/fleet/worker_test.go:85, 333`

`model.FakeServer.Requests()` records every transcript, but no fleet test reads it. Any of these regressions would pass every worker test and the golden:
- search results not fed back;
- fetched page text dropped;
- the unseen-URL guidance missing;
- the schema, schema name or strict flag not sent;
- the Brief not rendered into the system prompt.

**Fix:** Add three tests:
- `TestRunWorkerTranscriptCarriesToolOutput` reuses the search-fetch-final script and asserts:
  - `Requests()[1]` contains the search result URL;
  - `Requests()[2]` contains "Rayleigh scattering makes the sky appear blue.";
  - every request has `ResponseFormatType == "json_schema"`, `SchemaName == "worker_action"`, `Strict == true` and a schema equal to `actionSchema`;
  - `Requests()[0].Messages[0]` is the system role and contains the objective.
- `TestRunWorkerUnseenFetchFeedsGuidance` asserts the next request contains "did not appear in any search result".
- `TestRunWorkerTruncatedPageMarked` sets `MaxContentBytes` to 16 and asserts "[truncated" appears.

These tests are also the natural home for the C3-02 page-cap and C3-10 fence assertions.

**Acceptance criteria:** Deleting the `searchResultsMessage` or `fetchedPageMessage` append in `worker.go` makes a test fail.

---

### C3-20 — CLI worker failure paths and exit codes are untested
**(1-reviewer: tests)** — C3-TEST-6 (its budget-flag item is folded into C3-02)  
**File:** `internal/cli/research.go:157` (`buildWorker`, 64.5%), `:534` (`exitForStatus`)

Every required-field error, secret-resolution failure and the search-key path in `buildWorker` is uncovered. So are the Failed and Incomplete exit-code mappings and scrubbing of the model key from stderr, which V2-RA §5 acceptance lists explicitly.

**Fix:** Add four tests:
- `TestWorkerAgentMissingFleetConfig`, a `tt` table: missing model endpoint, model name, key ref or search endpoint, plus an unresolvable `secret://`. Each case asserts the error names the field, is not an `*ExitError`, and the model fake has zero calls.
- `TestWorkerAgentFailedExitCode`: a model 401 echoing `sk-proj-…`. Assert `ExitResearchFailed` and that neither stdout nor stderr contains the key.
- `TestWorkerAgentIncompleteExitCode`: `--fleet-max-turns 1`. Assert `ExitResearchFailed` and front matter `status: incomplete`. Adjust if C3-32 changes the exit code.
- `TestWorkerAgentSendsSearchKey`.

**Acceptance criteria:** `buildWorker` reaches at least 90%, and every worker terminal status has a CLI exit-code test.

---

### C3-21 — MCP client SSE framing and JSON-RPC error frames lack tests in their own package
**(1-reviewer: tests)** — C3-TEST-7  
**Files:** `internal/researcher/fleet/search/rpc.go:146-229` · `search/mcp.go:107-108` · `search/search_test.go:213`

`readEventStream` is at 65.3%. The following are uncovered:
- skipping a notification frame before the reply;
- multi-line `data:` fields;
- the per-event bound and scanner errors;
- a trailing frame with no blank line;
- "carried no JSON-RPC response";
- SSE decode errors.

No test proves the reader returns on the first response frame without waiting for EOF. The `initialize` rejection is uncovered everywhere. `TestSearchToolCallNotRetried` claims to test a server error but scripts a decode error.

**Fix:** Add these tests in `search_test.go`. Handlers live at the call site and write through a flushing `sseWrite` helper:
- `TestSearchSSESkipsNotificationBeforeReply`
- `TestSearchSSEMultiLineData`
- `TestSearchSSETrailingFrameWithoutBlankLine`
- `TestSearchSSENoResponseIsError`
- `TestSearchSSEMalformedDataIsError`
- `TestSearchSSEReturnsWithoutEOF`
- `TestSearchInitializeRejected`
- `TestSearchToolCallRPCErrorScrubbed`
- `TestSearchToolCall5xxNotRetried`
- `TestSearchNonTextBlocksIgnored`

Coordinate with the C3-27 fix for server requests.

**Acceptance criteria:** `readEventStream` reaches at least 90%, and `initialize` and `callSearch` reach 100%.

---

### C3-22 — Redirect-hop caps and model decode failures are untested in the credential-bearing clients
**(1-reviewer: tests)** — C3-TEST-8  
**Files:** `internal/researcher/fleet/model/model.go:181-189` · `fleet/search/search.go:204-212` · `fleet/model/generate.go:173-177, 222-229`

Both `refuseCrossHostRedirects` implementations are at 40%. Only cross-host refusal is tested, not the same-host follow or the three-hop cap, and a 307 or 308 on a POST replays the paid body. Three `Generate` failure paths are also uncovered: a malformed 200 body, `choices: []`, and an empty non-2xx body.

**Fix:** Add `TestSameHostRedirectFollowed` and `TestRedirectHopCap` in both packages, plus `TestGenerateMalformedBody`, `TestGenerateNoChoices` and `TestGenerateEmptyErrorBody`. If C3-26 adopts "refuse all redirects on the paid POST", invert the same-host test to assert refusal.

**Acceptance criteria:** Both redirect policies and `Generate` reach 100%.

---

### C3-23 — The Worker Researcher binding's defensive branches are untested
**(1-reviewer: tests)** — C3-TEST-10 (remaining items; the fetch-failure test is specified in C3-08)  
**Files:** `internal/researcher/fleet/worker_researcher.go:122, 139, 147-154` · `fleet/worker.go:290-291`

The unknown-id paths of `Await` and `Result` and the in-progress `Result` branch are uncovered.

**Fix:** Add `TestWorkerAwaitUnknownID`, `TestWorkerResultUnknownID` and `TestWorkerResultBeforeCompletion`. The last uses the blocking model and asserts `StatusInProgress` with the query and tools set, without blocking.

**Acceptance criteria:** `doFetch`, `Await`, `Result` and `state` reach 100%.

---

### C3-24 — V2-AMENDS.md carries amendments the shipped design reversed, with nothing marking them superseded
**(1-reviewer: docs)** — C3-DOC-4  
**File:** `docs/V2-AMENDS.md:3,5`

Amend 1 (OpenAI Responses API via Workload Identity Federation) and amend 3 (reverting to long requests via Responses or Anthropic Messages) are reversed by amend 8, V2-PLAN D2 and V2-RA:69-71. The shipped client is Chat Completions. V2-PLAN says it "applies V2-AMENDS.md in full" without flagging them. The file also has no frontmatter and is the only new doc using curly quotes.

**Fix:** Add a status header marking amends 1 and 3 as superseded by amend 8 and V2-PLAN D2/D9. Alternatively, fold the live content into the V2-PLAN §1 table and delete the file. Add frontmatter and use ASCII quotes.

**Acceptance criteria:** The header is present, or the file is removed and V2-PLAN §1 carries the same provenance.

---

## Low — non-blocking; address in this wave where trivial

---

### Test infrastructure

**C3-25 — Test fakes ship in the production binary, three test helpers hide server lifecycle, and the SSE fake does not flush**  
*(4-reviewer consensus: spec, tests, code, consistency — merges C3-SPEC-18, C3-TEST-11, C3-CODE-20 (helper item), C3-CONS-3)*  
**Files:** `fleet/model/fake.go` · `fleet/search/fake.go:186-192` · `fleet/worker_testservers_test.go:14-62` · `fleet/worker_researcher_test.go:19`  
`go list -deps ./cmd/chiron` shows `net/http/httptest` linked into `chiron`. No non-test file on `main` imports it. DECISIONS accepts this, but AGENTS.md has no carve-out. `newFetchPage`, `newBlockingModelServer`, `newLeakySearchServer` and `newWorker` hide server lifecycle, against the AGENTS.md test conventions. The search fake writes its SSE frame with a raw `Fprintf` and no flush. That works today only because the handler returns immediately.  
**Disagreement:** The consistency report's Verified OK says every new `_test.go` file creates servers at the call site. The other three reports name the helpers in `worker_testservers_test.go`. The majority reading is used.  
**Action (this PR):**
- Inline the three helpers at their call sites with `defer Close()`.
- Have `newWorker` take the search fake from its caller.
- Flush after the SSE write in `fake.go`, or add a one-line comment explaining why the single write needs none.
- Add an AGENTS.md carve-out for the exported `FakeServer`s that points at DECISIONS.

**Follow-up:** Move the fakes into test-support packages so `httptest` leaves the binary.

---

### Security hardening (same pass as C3-04 and C3-17)

**C3-26 — A same-host https-to-http redirect re-sends the bearer key in cleartext**  
*(2-reviewer consensus: code, security — merges C3-CODE-21, C3-SEC-6)*  
**Files:** `fleet/model/model.go:181-189` · `fleet/search/search.go:204-212`  
`refuseCrossHostRedirects` compares only `URL.Host`. Go's `shouldCopyHeaderOnRedirect` is hostname-only, so `Authorization` is re-sent over http.  
**Action:** Refuse a redirect whose scheme is not https when the original was. The simpler alternative is to refuse all redirects on the paid POST. Add a test that redirects a TLS test server to `http://` on the same host and asserts refusal with no second request, in both packages. Coordinate with C3-22.

**C3-30 — Endpoints accept userinfo, query and fragment, and echo them in errors**  
*(security — C3-SEC-7)*  
**Files:** `internal/config/config.go:328` · `fleet/model/model.go:143` · `fleet/search/search.go:165`  
`https://api.openai.com@evil.example/v1` passes validation. A low-entropy password in userinfo is echoed unredacted on a scheme typo. A query string also breaks path joining.  
**Action:** Reject `u.User`, `RawQuery` and `Fragment` in all three validators, and echo only `u.Redacted()` or scheme plus host. Add table tests in the config, model and search packages.

**C3-31 — Worker `Detail` can carry up to 1 MiB of provider error text into spans and report front matter**  
*(security — C3-SEC-10)*  
**File:** `fleet/worker.go:144` (`w.failed`)  
Provider error bodies can echo attacker page text into the `detail` span attribute, the stderr event and the report's `status_detail`. The `objective` attribute also duplicates the full query on every worker span.  
**Action:** Truncate `Detail` to about 512 bytes with an ellipsis in `w.failed`. Drop `objective` or truncate it to 256 bytes. Add a test where a 2 MiB error body leaves `len(Finding.Detail)` at or under the bound.

---

### MCP client

**C3-27 — The MCP search client opens a new session per search, sends no protocol-version header, never ends sessions, and misreads server requests as responses**  
*(2-reviewer consensus: code, security — merges C3-CODE-15, C3-SEC-11)*  
**Files:** `fleet/search/mcp.go:72-92` · `fleet/search/rpc.go:73, 232-237`
- Every `Search` runs `initialize`, `initialized` and `tools/call` and never sends `DELETE`.
- `MCP-Protocol-Version` is never sent, and the negotiated `protocolVersion` is ignored.
- `readEventStream` accepts the first response frame without matching its id.
- `rpcResponse.Method()` returns true only when `id`, `result` and `error` are all absent. A server `ping` carrying an id is therefore taken as the reply, and decoding the nil `result` fails.

**Action (this PR, correctness):** Decode `method` and skip frames that carry it, and rename the predicate `isResponse`. Require the response id to equal the request id in both the JSON and SSE paths. Test: a ping frame before the reply still returns results, and a mismatched id is rejected.  
**Follow-up:** Send the protocol-version header and check the negotiated version. Reuse the session with re-initialise on 404, or send a best-effort `DELETE`.

---

### Stale text and hygiene

**C3-28 — Stale or inaccurate comments, help text and DECISIONS statements**  
*(2-reviewer consensus: code, spec — merges C3-CODE-18 and C3-SPEC-16, remaining items; the ModelName items are in C3-12 and the AGENTS.md rows in C3-01)*
- `config.go:72` Agent comment omits `worker` and `fleet`.
- `config.go:129-130, 189, 274-275, 322` say "a later wave resolves/requires" the endpoints. They are required at the root now.
- `fleet.go:1-8` package doc describes parallel Stirrup jobs and Gemini DR workers, and "every method returns ErrNotImplemented".
- `worker.go:146-150` span comment contradicts `span.End(nil)` (see C3-05).
- `worker_action.go:131-133` claims an empty final maps to a placeholder body. `mapInteraction` omits `Outputs`.
- DECISIONS 2026-07-01 model-adapter entry says worker calls "leave [JSONSchema] nil". The worker sets it on every turn.
- DECISIONS config entry calls the guard "typed". The ceiling "both are honoured" sentence is covered by C3-02.
- `worker_test.go:305` names `…OnLaterStop`, and `:224-227` describes behaviour the test does not implement (with C3-11).
- The `TestWorkerAgentIsWired` comment calls the model endpoint "MCP-style".

**Action:** Correct each item. Acceptance: grep for "adapter default", "later wave" and "a stub that returns" finds nothing in the touched files.

**C3-29 — Test hygiene: vacuous assertions, mismatched comments, slow deadline tests, and minor config and golden gaps**  
*(2-reviewer consensus: tests, code — merges C3-TEST-12, C3-TEST-13 (remaining), C3-TEST-14 (remaining), C3-CODE-20 (remaining))*
- **Vacuous guard.** In `TestRunWorkerRefusesSideEffectingActions`, no scripted action references `fetchSrv`, so its guard cannot fire. Point a fetch-shaped case at it, and fix the `searchSrv` "fails the test" comment.
- **Golden test comment.** The comment at `worker_golden_test.go:132` overstates what is checked, and the local `cap` shadows the builtin.
- **Nil deref on failure.** `TestFetchSchemeRejected` (`fetch_test.go:136-141`) calls `err.Error()` after a non-fatal `t.Errorf`. Use `t.Fatalf`.
- **Unsynchronised state.** `TestFetchRedirectDepthCapped` (`fetch_test.go:243`) mutates `hops` unsynchronised. Use `atomic.Int32`.
- **Slow deadline tests.** `TestCallerContextDeadlineWins` (`model_test.go:286`) and `TestSearchCallerContextDeadlineWins` (`search_test.go:348`) take 2s each because the handler never drains the body. Drain `r.Body` first; target under 300ms with `-race`.
- **Config gaps.** Add `http://localhost` and `http://127.0.0.1` acceptance cases. Add a field column to `TestValidateRejectsBadFleet` and assert on it.
- **Golden port normaliser.** `volatilePort` should also match `\[::1\]:\d+`.
- **Detached goroutines.** The code report says the goroutines in `TestWorkerStartReturnsImmediately` and `TestWorkerAwaitRespectsContext` outlive the test. The tests report says `defer close(release)` releases them. Either way, the C3-03 cancel handle makes this moot.
- **Superseded item.** C3-TEST-14's "ceiling inert" test is replaced by the C3-02 rejection test.

**Acceptance:** Comments match assertions. No nil-deref-on-failure patterns and no unsynchronised handler state remain. `go test -race ./internal/researcher/fleet/...` is clean.

**C3-33 — `parseAction` errors lack the `fleet:` package prefix**  
*(consistency — C3-CONS-2)*  
**File:** `fleet/worker_action.go:112, 118, 124, 128, 135, 140`  
**Action:** Prefix all six errors with `fleet: `. Visible `status_detail` strings change as a result, so update any test or golden that matches them. Acceptance: grep shows every `fmt.Errorf` in the file starting with `fleet: `.

---

### Worker adapter and user experience

**C3-32 — The Worker adapter records `CompletedAt` at `Result` time, and the exit code for a cap stop is undecided**  
*(code — C3-CODE-17, remaining items; the runs-map pruning is in C3-03)*  
**Files:** `fleet/worker_researcher.go:179` · `internal/cli/research.go:525-539`  
**Action:** Set `CompletedAt` in the goroutine before `close(st.done)`, and test that it is no later than Await's return. Decide whether an `incomplete` cap stop exits 2 (today, as the verify reviewer confirmed against the help text) or 3 ("stopped"), and document the choice in the exit-code help.

**C3-34 — No progress signal on stderr during a worker run**  
*(functionality — C3-FUNC-6)*  
**Files:** `fleet/worker_researcher.go:105-118` · `internal/run/run.go:127-146`  
Between `interaction_created` and `run_completed` a worker run emits nothing for up to five minutes, so an operator cannot tell progress from a hang. **Disposition:** follow-up.

**C3-35 — The V2-RESEARCH-AGENT.md §2 "existing state" table will read as current after merge**  
*(docs — C3-DOC-6)*  
**File:** `docs/V2-RESEARCH-AGENT.md:93`  
The table says `internal/researcher/fleet` is a stub returning `ErrNotImplemented`, which this PR overtakes.  
**Action:** Date-stamp the table as "state as of 2026-07-01, before Wave 3", or point at the AGENTS.md package map as the current source of truth.

---

## Verified OK (cross-reviewer)

Each of these was confirmed by at least two reviewers. No reviewer contradicted any of them.

- **Research-only by construction.** The action vocabulary is a closed three-verb enum decoded with `DisallowUnknownFields`, and the dispatch switch has no execution default. `TestRunWorkerRefusesSideEffectingActions` covers the refusals. `seenURLs` is populated only by real search results. *(code, security, spec, tests, verify)*
- **No paid retries.** The model POST, search `tools/call` and the worker loop are each single-attempt, pinned by `TestNoRetryOn5xx`, `TestSearchToolCallNotRetried` and `TestRunWorkerNoRetryOnPaidTurn`. *(code, security, spec, tests, functionality)*
- **Run core untouched.** `internal/run`, `internal/researcher/gemini`, `internal/interactions`, `go.mod` and `go.sum` show no diff. No new dependency or vendor SDK was added. *(code, security, spec)*
- **Credential handling.** Keys travel only in `Authorization`. Every error path scrubs by exact key match and then `secret.Scrub`. Keys resolve only at the composition root. *(code, security, consistency, functionality)*
- **Endpoint scheme rules and cross-host redirect refusal.** https is required, with http only for loopback, enforced in config and in both adapters. Only the scheme downgrade in C3-26 remains open. *(code, security, spec, tests)*
- **Bounded reads.** Model 16 MiB, search 8 MiB aggregate and per event, and fetch truncate-and-mark. *(code, security, spec, tests)*
- **Id emitted early, `--agent fleet` guard fires before any secret or request, golden report through the unchanged run core.** *(code, spec, tests, verify)*
- **Tooling.** Build, vet, `-race` tests at count 1, 3 and 5, and golangci-lint are green. Table variables are `tt`, en-GB is held and there are no emojis. *(spec, tests, consistency, docs)*

---

## Disposition guidance

### (a) Fix in this PR

**Required for merge:**

- **C3-01:** Rewrite CLAUDE.md's v2 paragraph, update the AGENTS.md map and "Money safety", and document the worker in the README.
- **C3-02:** Bound page text, default the token cap, reject `ceiling_gbp > 0`, and reject Gemini-only levers on `worker`/`fleet`.
- **C3-03:** Keep the run deadline, cancel on Await, and map mid-call stops and `length` truncation to `incomplete`.
- **C3-04:** Add a dial-time IP check without a proxy, a deny-by-default classifier, context-aware lookups and a short fetch timeout.
- **C3-05:** Emit worker metrics before `span.End`, stop double-counting, and add the trace scrub and metrics tests.
- **C3-06:** Use a strict-mode-valid nullable schema and `max_completion_tokens`, with a per-turn clamp.
- **C3-07:** Repoint dead V2-RA section references and fix V2-PLAN Wave 5.
- **C3-08:** Make fetch and search tool errors recoverable feedback. This needs team-lead confirmation.
- **C3-09:** Document that the fetch step cannot run through the CLI against loopback, or add an env-gated override with an e2e test.
- **C3-10:** Fence untrusted content, keep only seen citations, and sanitise the report body.
- **C3-11:** Preserve fetched sources on `incomplete` and `failed`.
- **C3-12:** Correct the CLI and README agent descriptions, reject `memory: inmemory`, and unify the fleet sentinel.
- **C3-13:** Reject `wkr_` ids locally in `get` and `follow-up`, and qualify the help.
- **C3-14:** Make `parseAction` reject trailing data, surface refusals, and add the malformed-output table.
- **C3-15:** Remove "Wave N" narrative from comments and the user-facing error.
- **C3-16:** Add a DECISIONS entry for wave sequencing and the four missing Wave 3 items.
- **C3-17:** Stop a config file from choosing both credential and destination. This needs team-lead choice of option (a), (b) or (c).
- **C3-19:** Add transcript-content assertions to the worker tests.
- **C3-20:** Add CLI worker failure-path and exit-code tests.
- **C3-21:** Add MCP SSE framing and JSON-RPC error tests.
- **C3-22:** Add redirect-hop and `Generate` decode-failure tests.
- **C3-23:** Add tests for the Worker binding's unknown-id and in-progress branches.
- **C3-24:** Mark V2-AMENDS amends 1 and 3 superseded, or fold them into V2-PLAN.
- **C3-28:** Correct stale comments, help text and DECISIONS statements.
- **C3-35:** Date-stamp the V2-RA §2 "existing state" table.

**Cheap Low items, recommended in the same pass:**

- **C3-25 (in-PR part):** Inline the helper-built servers, flush the SSE fake, and add an AGENTS.md carve-out for `FakeServer`.
- **C3-26:** Refuse an https-to-http redirect on credential-bearing clients.
- **C3-27 (in-PR part):** Skip server-request frames and match response ids.
- **C3-29:** Fix test hygiene, drain request bodies in the deadline tests, and close the config and golden gaps.
- **C3-30:** Reject userinfo, query and fragment in endpoints, and redact them in errors.
- **C3-31:** Bound `Finding.Detail` and the `objective` span attribute.
- **C3-32:** Record `CompletedAt` at loop end and document the cap-stop exit code.
- **C3-33:** Prefix `parseAction` errors with `fleet:`.

### (b) Track as follow-up issue

These are seeded from the spec report's "Deferred / not in this PR" list. Two items from that list move into (a) instead. `web_fetch` DNS-rebinding hardening becomes C3-04 because security rates it High. The Wave 3 worker trace scrub test becomes C3-05. Waves 5, 6 and 7 and the scale-out track stay tracked by V2-PLAN itself and need no issue.

1. **Wave 1: ConnectRPC control-plane transport**  
   Generate the `chiron.v1` code, add `internal/transport/connect.go`, and implement `CONTROL_PLANE_ADDR` validation plus the submission and history RPCs. Plan reference: V2-PLAN §6 Wave 1.

2. **Wave 2: Langfuse and OTLP forwarding flags, including worker spend**  
   Add `--otlp-endpoint` and the `--langfuse-*` flags with ingestion headers so worker token and cost signals reach Langfuse. Plan reference: V2-PLAN §6 Wave 2 deliverables 1–2, Wave 3 acceptance 4, and §4 "Spend visibility".

3. **Wave 2: reserved fleet span vocabulary and Langfuse-key span scrub test**  
   Add `SpanDecompose`, `SpanDelegate`, `SpanSynthesise`, `SpanCite` and the control-plane span, plus the synthetic single-worker test and a test that no Langfuse key transits a span. Plan reference: V2-PLAN §6 Wave 2 deliverable 3, acceptance 3 and key task 3; V2-RA §8.

4. **SP-A: choose the web-search MCP backend and make its tool name configurable**  
   Record the chosen backend's tool name, argument key and result shape in DECISIONS, then add `fleet.search_tool` and `fleet.search_query_arg` or hard-wire the vendor values (C3-18). Plan reference: V2-PLAN §5 SP-A and the §6 Wave 3 DECISIONS list.

5. **Per-model GBP rate table so `fleet.ceiling_gbp` can be enforced**  
   This PR rejects a non-zero `ceiling_gbp` because `EstimatedCostGBP` is never computed (C3-02). Add per-model input and output rates, compute the estimate in `accumulateModelUsage`, and re-enable the ceiling. Plan reference: V2-PLAN §4 per-loop caps.

6. **Web page HTML-to-text extraction beyond a basic tag strip**  
   This PR adds a page byte cap, a content-type allowlist and a stdlib tag strip. The follow-up improves readable-text extraction by removing scripts, navigation and boilerplate, and handling tables. Plan reference: V2-RA §5 step 4, "bounded page content".

7. **Shared `internal/httpx` helper for redirect policy, bounded reads and the loopback scheme rule**  
   Consolidate the duplicated `allowedEndpointScheme`, `refuseCrossHostRedirects` and `readBounded` across `interactions`, `config`, `cli`, `fleet/model` and `fleet/search`. Plan reference: the DECISIONS 2026-07-01 model-adapter entry, which records this follow-up.

8. **MCP search client: protocol-version header, version check and session lifecycle**  
   Send `MCP-Protocol-Version` after `initialize`, reject unsupported negotiated versions, and reuse the session with re-initialise on 404 or a best-effort `DELETE` (C3-27). Plan reference: V2-PLAN §6 Wave 3, the Streamable-HTTP MCP search client deliverable.

9. **Move the model and search `FakeServer`s into test-support packages**  
   Relocate the fakes to `model/modeltest` and `search/searchtest`, or equivalent, so `net/http/httptest` is no longer linked into the `chiron` binary (C3-25). Plan reference: V2-RA §8 required fakes, the DECISIONS 2026-07-01 fake-reuse entry, and the AGENTS.md test conventions.

10. **Per-turn progress events for worker runs**  
    Emit a best-effort transport event per turn between `interaction_created` and `run_completed` so operators can tell progress from a hang (C3-34). Plan reference: V2-RA §6 "Spend and observability", which requires best-effort emission.

11. **Template support for the worker research brief**  
    This PR rejects `--template` for `--agent worker` (C3-02). The follow-up lets a template shape the worker prompt and report layout. Plan reference: V2-RA §5 research-prompt deliverable (V2-PLAN §6 Wave 3).

12. **Wave 4: lead orchestrator and external-web router**  
    Implement `lead.go`, which decomposes a query into 3–5 four-field briefs and persists the plan by reference. Implement `router.go` for external-web routing, with the Gemini stopgap excluded as a target. Plan reference: V2-PLAN §6 Wave 4 key tasks 1–2; V2-RA §6 lead flow 1–3.

13. **Wave 4: bounded worker pool**  
    Run goroutine workers in parallel through `RunWorker` under `max_workers` and `concurrency`. Plan reference: V2-PLAN §6 Wave 4 key task 3 and §4 "bounded fan-out".

14. **Wave 4: synthesis and citation pass**  
    Implement `synthesise.go` and `cite.go` to merge worker findings into one report with a deduplicated `Citation` list. Plan reference: V2-PLAN §6 Wave 4 key task 4; V2-RA §6 lead flow 6–8.

15. **Wave 4: in-memory ContextStore and findings by reference**  
    Implement `internal/memory/inmemory.go` with `Put`/`Get`, `OpenSession`, TTL/GC and the `Remember`/`Recall` decision, and have each worker write its findings as it goes. This PR rejects `memory: inmemory` until then (C3-12). Plan reference: V2-PLAN §6 Wave 4 deliverable 2 and key task 5, D4, SP-E, and §4 bullet 3.

16. **Wave 4: fleet observability spans and per-run metric rollup**  
    Emit decompose, delegate, synthesise and cite spans, and roll up per-worker metrics per run. Plan reference: V2-PLAN §6 Wave 4 key task 6; V2-RA §6 "Spend and observability".

17. **Wave 4: eval-vs-baseline harness and single-call-default gate**  
    Build the eval harness and judge that compare the fleet against the managed Deep Research baseline, and gate on the single-call default. Plan reference: V2-PLAN §6 Wave 4 deliverable 4 and key task 8; SP-F; V2-RA §7.

18. **Wave 4: wire `--agent fleet`, the fleet golden report and Wave 4 docs**  
    Replace the `checkResearcherWired` guard with real construction, and add the fleet golden report and in-memory store tests. Also add the AGENTS.md map entries and the five Wave 4 DECISIONS entries. Plan reference: V2-PLAN §6 Wave 4 acceptance 1 and deliverable 5; V2-RA §6 deliverables and §8.

19. **Durable recovery of in-process worker runs**  
    `wkr_` ids are attribution handles only and cannot be resumed after process death. This PR rejects them locally in `get` and `follow-up` (C3-13). Plan reference: V2-RA §3; V2-PLAN Wave 7 key task 4.
