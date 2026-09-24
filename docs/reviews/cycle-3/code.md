# Cycle 3 — general code review (CODE)

Scope: all Go changed on `feat/v2-research-agent` (HEAD d4ac9f2) against `origin/main`.
Files read in full: `internal/researcher/fleet/{worker,worker_action,worker_prompt,worker_researcher}.go`,
`fleet/model/{model,generate,fake}.go`, `fleet/search/{search,mcp,rpc,fake}.go`,
`fleet/fetch/{fetch,do}.go`, `internal/cli/research.go`, `internal/config/{config,flags}.go`,
`internal/run/run.go` (unchanged, read for the seam contract), the worker/CLI tests, and the
DECISIONS.md entries added on this branch.

Findings are ordered by severity. No file outside `docs/reviews/cycle-3/` was modified.

---

### C3-CODE-1 — Action schema is invalid under OpenAI `strict: true`, so every real worker run fails on turn 1
**Severity:** High
**File:** internal/researcher/fleet/worker_action.go:60-96 (with internal/researcher/fleet/model/generate.go:204-213)

The model client always sends `strict: true` when a schema is set:

```go
JSONSchema: jsonSchemaSpec{ Name: req.SchemaName, Schema: req.JSONSchema, Strict: true },
```

The worker's schema declares five properties but only one is required, and the citation item
declares `title` without requiring it:

```json
"required": ["action"]
...
"properties": { "url": {...}, "title": {...} }, "required": ["url"]
```

OpenAI Structured Outputs in strict mode requires every key in `properties` to appear in
`required` (optional fields must be expressed as a `["string","null"]` union). OpenAI rejects
this schema with HTTP 400 ("'required' is required to be supplied and to be an array including
every key in properties. Missing 'query'"). The v2 plan names GPT-5.4/5.5 as a target model.
Every `--agent worker` run against OpenAI would therefore end `failed` on the first turn with
"model turn failed: HTTP 400". The fakes never validate the schema, so CI cannot catch this.
The comment at worker_action.go:53-56 relies on optional fields, which strict mode forbids.

**Fix:** List all five properties in `required`, type the per-action fields as nullable
(`"type": ["string","null"]`, `"type": ["array","null"]`), and make citation `title`
required-and-nullable. `parseAction` already tolerates JSON `null` for strings and slices. The
alternative is to make `Strict` a per-request option and send `strict: false` for this schema.
That weakens the enum guarantee on the wire, so the nullable form is preferred.

**Acceptance:** A unit test walks `actionSchema` and asserts that every object has
`additionalProperties:false` and that `required` equals the key set of `properties`, recursively.
A `parseAction` table case with `{"action":"search","query":"x","url":null,"answer":null,"citations":null}`
decodes to a valid search.

---

### C3-CODE-2 — The worker goroutine ignores the run's deadline and cancellation, and the comment claiming otherwise is wrong
**Severity:** High
**File:** internal/researcher/fleet/worker_researcher.go:100-110

```go
// ... Cancellation and the wall-clock bound are carried by Await's context and the per-worker
// Timeout cap inside RunWorker ... Using the Start ctx here would cancel the run the instant
// Start returns.
go func() {
    st.finding = RunWorker(context.WithoutCancel(ctx), w.deps, brief)
```

- **The deadline is dropped.** `context.WithoutCancel` also removes the parent's deadline. The run-level `--timeout` never reaches the loop, so only `fleet.worker_timeout` bounds it.
- **Await does not cancel the loop.** When Await's context ends, Await returns (lines 127-128) but nothing cancels the goroutine. Paid model turns keep running until `MaxTurns` or `WorkerTimeout` fires.
- **The comment is factually wrong.** The run core passes a span-carrying child of the run context to Start (run.go:132-133). That context is not cancelled when Start returns, so the stated reason for detaching does not hold.

Consequences:
- **Lost partial result.** If `--timeout` is shorter than `fleet.worker_timeout` (for example `--timeout 2m` with the 5m default), Await fails. `run.Run` then returns an error and exits 1 with no report. The finding the loop would have produced is lost, and its partial usage is never reported.
- **Unreliable Stop.** A caller cannot stop spend by cancelling. SIGINT only helps because the CLI process exits. The Wave 4 lead is documented to reuse this in-process, and there an abandoned Await leaks paid turns.

**Fix:** Keep the parent's deadline and add a cancel handle:

```go
runCtx := context.WithoutCancel(ctx)
if dl, ok := ctx.Deadline(); ok { runCtx, cancel = context.WithDeadline(runCtx, dl) } else { runCtx, cancel = context.WithCancel(runCtx) }
```

Store `cancel` on `workerState`, and call it from Await's `ctx.Done()` branch. Better still, derive the run from the Start context directly and drop the comment. Also validate `fleet.worker_timeout < timeout`, so the loop's own Incomplete finding lands before the run deadline.

**Acceptance:** With a blocking model server, `Start` is called with a context carrying a 200ms
deadline. The model server observes the request context cancelled within about 200ms. A config
test rejects `worker_timeout >= timeout`.

---

### C3-CODE-3 — Any single fetch or search error fails the whole paid run
**Severity:** High
**File:** internal/researcher/fleet/worker.go:258-262, 294-297; internal/researcher/fleet/fetch/do.go:51-53

```go
page, err := w.deps.Fetch.Fetch(ctx, act.URL)
if err != nil {
    return true, w.failed("fetch failed: " + w.scrub(err.Error()))
}
```

`Fetch` returns an error for:
- any non-2xx response, which includes the very common 403 bot-block, 404 and paywall 401;
- DNS failure;
- a TLS error;
- the SSRF guard refusing a search result that resolves internally;
- a non-http(s) scheme appearing in a search result.

Go's default `User-Agent` also draws 403s from many CDNs. Each of these ends a run that has
already paid for several model turns as `failed`, with no answer. That is the most common
real-world outcome of fetching arbitrary search hits.

The unseen-URL path already shows the right pattern at lines 281-289: feed the error back and
let the model choose again. A `tools/call` `isError` from search likewise fails the run.

**Fix:** Treat per-URL fetch errors as recoverable. Append
`errorFeedbackMessage("fetching <url> failed: <scrubbed reason>")` and continue. The turn cap
still bounds the loop. Reserve hard failure for context errors and a nil client. Apply the same
rule to search tool-level errors, meaning `isError` and JSON-RPC errors. Transport failures
against the configured search endpoint can stay fatal. Optionally set a descriptive
`User-Agent` on fetch.

**Acceptance:** A worker test scripts search, then a fetch of a result URL that returns 404,
then final. The finding is `completed`, the transcript sent on turn 3 contains the fetch-failure
feedback, and the model call count is 3.

---

### C3-CODE-4 — The transcript has no size or cost bound: multi-megabyte pages are re-sent on every turn, and the default token cap is off
**Severity:** High
**File:** internal/researcher/fleet/worker_prompt.go:153-167; internal/cli/research.go:208-211; internal/config/config.go:190-197; internal/researcher/fleet/fetch/fetch.go:38

- **Page size.** `buildWorker` leaves `MaxContentBytes` unset, so each fetched page can be up to 8 MiB (`defaultMaxContentBytes = 8 << 20`). `fetchedPageMessage` writes the raw bytes verbatim into a user turn (`b.Write(page.Content)`).
- **No filtering.** The bytes are raw HTML, and there is no Content-Type filter, so PDFs and images arrive as binary. `json.Marshal` then coerces invalid UTF-8 to U+FFFD.
- **Quadratic cost.** The full transcript is re-sent on every subsequent turn, so input cost grows quadratically with turns. A single 1 MiB HTML page is roughly 250k tokens. Fetched on turn 2 of 8, it is paid for six more times.
- **No token cap by default.** The default `MaxTokens` is 0, meaning uncapped. `CeilingGBP` can never fire, see C3-CODE-9. The turn cap therefore bounds the number of calls but not their size. A page above the context window gets a 400, which C3-CODE-3 turns into a failed run.
- **Search too.** Search results are likewise bounded only by the 8 MiB MCP body cap.

This is the money path. V2-PLAN D6/D7 defers budgeting but relies on "per-worker turn limit,
token/cost ceiling" as the structural ceiling. As shipped, the token ceiling is off by default.

**Fix:**
- Give the worker its own small per-page transcript budget, for example 32 to 64 KiB. Either set `fetch.Options.MaxContentBytes` in `buildWorker` or truncate in `fetchedPageMessage`.
- Accept only textual content types (`text/*`, `application/json`, `application/xhtml+xml`). Refuse other types as recoverable feedback.
- Strip HTML tags to text with the standard library.
- Cap snippet length and result count in `searchResultsMessage`.
- Ship a non-zero default `fleet.max_tokens`.

**Acceptance:** A worker test with a 1 MiB fetch page asserts that the next model request's page
message is at most the configured cap and is marked truncated. A binary content-type page yields
feedback, not bytes. `defaultFleet().MaxTokens > 0`.

---

### C3-CODE-5 — A wall-clock timeout or token truncation ends the run as `failed`, although the documentation says `incomplete`
**Severity:** Medium
**File:** internal/researcher/fleet/worker.go:185-192, 217-238

The context check that yields `incomplete` runs only at the top of the loop. Almost all
wall-clock time is spent inside `Generate`, `Search` or `Fetch`. When `Caps.Timeout` fires there,
the tool returns `context deadline exceeded` and the loop returns
`w.failed("model turn failed: ...")`. The documented contract, "A timeout is Incomplete", holds
only in the rare case where the deadline passes between turns. That is the only case the test
covers: `TestRunWorkerTimeoutIsIncomplete` pre-cancels the context.

Similarly, `resp.FinishReason` is never inspected. When the per-turn `max_tokens` from
`remainingTokenBudget` truncates the reply (`finish_reason: "length"`), the partial JSON fails
`parseAction`. The run then ends `failed` with "invalid model action" instead of `incomplete`
with the token-cap reason. Reasoning models make this likely, because reasoning tokens count
against `max_completion_tokens`.

**Fix:** In `turn`, `doSearch` and `doFetch`, when `err != nil && ctx.Err() != nil`, return
`w.incomplete("worker context ended: ...")`. In `turn`, when `resp.FinishReason == "length"` and
a token cap is set, return `w.incomplete(<token-cap reason>)` before parsing.

**Acceptance:** A test uses a model server that sleeps past a 100ms `Caps.Timeout` and asserts
`StatusIncomplete`. A test scripts `FinishReason:"length"` with truncated content under a
`MaxTokens` cap and asserts `StatusIncomplete` with the cap in `Detail`.

---

### C3-CODE-6 — "Partial citations are preserved on all outcomes" is vacuous: a stopped or failed run always has zero citations
**Severity:** Medium
**File:** internal/researcher/fleet/worker.go:311-321, 326-348, 353-363

`w.citations` is populated only in `finalise`, from the model's final action. Search results go
into `seenURLs`, and fetched pages go only into the transcript. An `incomplete` or `failed`
Finding therefore always carries `Citations: nil`, regardless of how many sources were read. This
contradicts:
- the `Finding` and `RunWorker` documentation, "Partial citations gathered before a stop are preserved on all outcomes";
- DECISIONS.md, "a bounded or failed run never discards the sources it found";
- V2-RESEARCH-AGENT §8, "Partial worker failure is represented ... without losing completed findings".

The test comment in `TestRunWorkerCapsStopDeterministically` (worker_test.go:224-227) admits this
("The worker gathers citations from final actions, so ... we drive a final on the last allowed
turn instead"), but the test does not do what that comment says.

**Fix:** Record each successfully fetched page, using the final URL and a title if known, as a
provisional source on `workerRun`. Surface those on `incomplete` and `failed` outcomes. Either
update the documentation to say "fetched sources" or genuinely preserve them. Fix or remove the
misleading test comment.

**Acceptance:** A test runs search, then fetch, then a model 5xx. The `failed` Finding carries
one citation for the fetched URL.

---

### C3-CODE-7 — Citations in the final answer are not checked against sources the worker actually saw
**Severity:** Medium
**File:** internal/researcher/fleet/worker.go:311-314

```go
for _, c := range act.Citations {
    w.addCitation(c.URL, c.Title)
}
```

Any URL the model emits becomes a report source. That includes hallucinated URLs, or URLs
planted by a prompt-injected page. `TestWorkerAgentIsWired` (internal/cli/research_test.go) shows
this directly: `https://example.org/sky` is never searched or fetched, yet it appears in the
report's sources. The worker already holds the ground truth in `seenURLs` and the set of fetched
URLs. Citation validity is an explicit eval criterion (V2-RESEARCH-AGENT §7).

**Fix:** Keep only final citations whose URL is in `seenURLs` or the fetched set. Record dropped
ones in `Detail` or a span attribute, or mark them as unverified. Adjust the CLI smoke test to
search first, or to expect the unverified URL to be dropped.

**Acceptance:** A final action cites one seen and one unseen URL, and the Finding carries only
the seen URL.

---

### C3-CODE-8 — Deep-research levers are silently ignored for `--agent worker`, including `--plan` and `--budget`
**Severity:** Medium
**File:** internal/cli/research.go:125-145; internal/config/config.go:233-243

`runWorkerResearch` bypasses the budget gate and plan review ("--plan and --budget ... do not
apply here"). `Validate` does not reject them. A user who runs
`chiron research --agent worker --plan --budget 1` gets real spend with no plan approval and no
budget check, and no error or warning. That breaks the AGENTS.md expectation that `--plan`
gates spend.

`--input`, `--mcp`, `--file-search`, `--tools`, `--template`, `--visualise` and `--model` are
dropped the same way. For example, grounding documents passed with `--input` are silently
discarded. D6 defers budget *enforcement*, but accepting a safety flag and ignoring it is a
different failure.

**Fix:** In `ResearchConfig.Validate`, when `Agent` is `worker` or `fleet`, reject non-zero
`Plan`, `AcceptPlan`, `BudgetGBP`, `Inputs`, `MCP`, `FileSearch`, `Tools`, `Template`,
`Visualise` and `Model`, with a message naming the flag.

**Acceptance:** A table test in `internal/config` covers each lever. A CLI test shows
`--agent worker --plan` exiting non-zero before any request reaches the fake model, with a call
count of 0.

---

### C3-CODE-9 — `fleet.ceiling_gbp` is accepted and documented as honoured, but can never fire
**Severity:** Medium
**File:** internal/researcher/fleet/worker.go:200-202, 369-375; internal/config/config.go:155-159

`EstimatedCostGBP` is never assigned anywhere in the worker: `accumulateModelUsage` sums tokens
only. The check `w.usage.EstimatedCostGBP >= w.deps.Caps.CeilingGBP` is therefore always false.
DECISIONS.md says "both are honoured: exceeding either ends the loop incomplete". Config, the
`--fleet-ceiling` flag help and AGENTS.md all present it as a live spend cap. An operator who
sets `ceiling_gbp: 2` believes they are capped when they are not.

**Fix:** Either reject a non-zero `ceiling_gbp` with "not yet enforced: no price table", or add
a per-model rate (for example `fleet.input_price_per_mtok_gbp` and output equivalent) so the
estimate is real. Correct DECISIONS.md either way.

**Acceptance:** Either a config test rejects `ceiling_gbp > 0`, or a worker test with a rate
shows the ceiling stopping the loop `incomplete`.

---

### C3-CODE-10 — The `max_tokens` wire field is rejected by current OpenAI reasoning models and can exceed a model's output limit
**Severity:** Medium (medium confidence on the provider behaviour)
**File:** internal/researcher/fleet/model/generate.go:81-86; internal/researcher/fleet/worker.go:383-392

GPT-5-family and o-series models on Chat Completions reject `max_tokens` with "Unsupported
parameter ... use 'max_completion_tokens'". Separately, `remainingTokenBudget` returns the whole
remaining cumulative budget, which includes input tokens. With `fleet.max_tokens: 200000`, the
first turn sends `max_tokens: 200000`. That exceeds every model's completion limit and draws a
400. Both paths hit only when a token cap is configured, which C3-CODE-4 recommends making the
default.

**Fix:** Send `max_completion_tokens`, or make the field name an option. Clamp the per-turn value
to a configured per-turn maximum, for example `min(remaining, fleet.max_output_tokens)`.

**Acceptance:** A model test asserts the wire body carries `max_completion_tokens`. A worker test
with `MaxTokens` 200000 asserts the per-turn value is at most the per-turn clamp.

---

### C3-CODE-11 — Fetch's per-call timeout inherits the whole worker budget, and the SSRF DNS lookup ignores the context
**Severity:** Medium
**File:** internal/cli/research.go:189-211; internal/researcher/fleet/fetch/fetch.go:172

`buildWorker` sets `RequestTimeout: callTimeout`, which is `WorkerTimeout` and 5m by default, on
all three clients. For fetch, this replaces the 30s default: one slow-loris page can consume the
entire worker budget in a single call.

`guardHost` calls `net.LookupIP(host)`, which takes no context. It runs before
`context.WithTimeout` in `Fetch` and inside `CheckRedirect`, so a slow resolver is not bounded by
the request or worker deadline.

**Fix:**
- Leave fetch on its own short default, or add a `fleet.fetch_timeout`, and clamp it to at most `WorkerTimeout`.
- Resolve with `net.DefaultResolver.LookupIPAddr(ctx, host)`, using the request context in `Fetch` and `req.Context()` in `CheckRedirect`.
- Move the `WithTimeout` above the guard.

**Acceptance:** A fetch test with a resolver that never answers returns within `RequestTimeout`.
A `buildWorker` test asserts that the fetch timeout is shorter than the worker timeout.

---

### C3-CODE-12 — The SSRF deny-list misses several non-public ranges
**Severity:** Medium (overlaps the security review)
**File:** internal/researcher/fleet/fetch/fetch.go:191-199

`isInternal` covers only `IsPrivate`, link-local, loopback and unspecified. Unrefused ranges:
- **CGNAT 100.64.0.0/10.** This includes Alibaba's metadata address 100.100.100.200 and is commonly used for internal cluster ranges.
- **NAT64 64:ff9b::/96 and 6to4 2002::/16.** Both can embed 169.254.169.254 or RFC1918 addresses.
- **Other non-public space.** 0.0.0.0/8 beyond 0.0.0.0, 192.0.0.0/24, 198.18.0.0/15, 240.0.0.0/4, 255.255.255.255, and non-link-local multicast are also not refused.

DNS rebinding is a recorded follow-up in DECISIONS.md, but these gaps are not.

**Fix:** Switch to an allow-only-global rule: refuse unless `ip.IsGlobalUnicast()` and the
address is outside an explicit special-purpose list. Unwrap NAT64 and 6to4 embedded IPv4
addresses before checking.

**Acceptance:** A table test covers `100.100.100.200`, `64:ff9b::a9fe:a9fe`, `2002:a9fe:a9fe::1`,
`198.18.0.1` and `224.0.0.1`. All are refused, with and without `AllowLoopback`.

---

### C3-CODE-13 — Worker metrics are emitted after the span ends and double-counted with the run core; span parenting and comment are wrong
**Severity:** Low
**File:** internal/researcher/fleet/worker.go:140-152, 416-424

- **Metrics after `End`.** `emitWorkerMetrics(ctx, ...)` runs after `span.End(nil)`. The OTel binding implements `Metric` as `SpanFromContext(ctx).SetAttributes`, which is a no-op on an ended span, so the worker metrics are dropped.
- **Double counting.** The JSONL binding writes them, and `run.recordMetrics` then emits the same metric names from the same `Usage`, so every counter is doubled.
- **Wrong parent.** The worker context descends from the Start context, so `SpanWorker` is a child of the already-ended `start` span, not `await`.
- **Stale comment.** The comment at lines 146-149 says a context error "is recorded as the span's error", but the code always passes `nil`.

**Fix:** Emit worker metrics before `span.End`, or set them as span attributes such as `worker.search_count` rather than shared metric names, so the run core stays the sole emitter. Make the comment match the code, or pass the context error to `End`.

**Acceptance:** A JSONL-tracer test of a worker run through `run.Run` shows each metric name exactly once.

---

### C3-CODE-14 — `parseAction` accepts trailing garbage and discards refusals
**Severity:** Low
**File:** internal/researcher/fleet/worker_action.go:114-119; internal/researcher/fleet/model/generate.go:104-110

`dec.Decode(&a)` reads only the first JSON value, so `{"action":"final","answer":"x"} {"action":"search"}`
is accepted. That runs against the "decoded strictly" claim in the comment. Separately, a
structured-output refusal (`message.refusal` set, `content` null) is read as an empty string. It
surfaces as "model returned an empty action", losing the provider's reason.

**Fix:** After `Decode`, require `dec.More() == false` or a second `Decode` returning `io.EOF`.
Parse `choices[0].message.refusal` and surface it as a scrubbed failure detail.

**Acceptance:** A `parseAction` test rejects trailing content. A model test surfaces the refusal
text.

---

### C3-CODE-15 — The search MCP client opens a new, never-terminated session per search and misclassifies server requests
**Severity:** Low
**File:** internal/researcher/fleet/search/mcp.go:72-88; internal/researcher/fleet/search/rpc.go:232-237

Every `Search` performs `initialize`, `initialized` and `tools/call`. That is three round trips
and a fresh `Mcp-Session-Id` per query, and the session is never closed with HTTP `DELETE` as the
MCP transport spec asks. Stateful servers accumulate orphaned sessions.

`rpcResponse.Method()` does not look at a `method` field. It returns true only when `id`,
`result` and `error` are all absent. A server-initiated request on the SSE stream, such as `ping`
with an `id`, is therefore treated as *the* response. `callSearch` then fails decoding a nil
`result`. The name `Method()` also misdescribes what it tests.

**Fix:** Decode `method` into `rpcResponse` and skip frames that carry it. Rename the predicate
`isResponse`. Cache the session on the client with a mutex and re-initialise on HTTP 404, or at
least send `DELETE` with the session id after `tools/call`.

**Acceptance:** An SSE test sends a `{"jsonrpc":"2.0","id":99,"method":"ping"}` frame before the
reply and still gets results. A fake records a `DELETE` or a single initialize across two
searches.

---

### C3-CODE-16 — Untrusted page and snippet text is inlined with no data boundary
**Severity:** Low
**File:** internal/researcher/fleet/worker_prompt.go:120-167

Fetched content and search snippets are appended to user-role messages with no delimiter, and
the system prompt does not tell the model to treat them as data. The closed action vocabulary
and the `seenURLs` fetch allow-list limit the damage to a steered answer or citations, and
C3-CODE-7 covers the citations. Cheap hardening still helps.

**Fix:** Wrap tool output in explicit markers, for example `<untrusted_page url="...">…</untrusted_page>`, after escaping any closing marker inside the content. Add one sentence to the system prompt saying tool output is data and never instructions.

**Acceptance:** A prompt test asserts the markers are present and that an embedded closing marker is escaped.

---

### C3-CODE-17 — Minor lifecycle and mapping issues in the `Worker` adapter
**Severity:** Low
**File:** internal/researcher/fleet/worker_researcher.go:96-98, 179; internal/cli/research.go:525-539

- **Map never pruned.** `w.runs` is never pruned. That is harmless for the one-shot CLI but a leak for any long-lived holder.
- **Wrong `CompletedAt`.** `CompletedAt: time.Now()` records the time `Result` was called, not when the loop finished. Record it in the goroutine before `close(st.done)`.
- **Exit code for a cap stop.** A cap stop (`incomplete`) exits 2 ("research failed"). A deliberate structural stop is closer to exit 3 ("stopped"). Decide and document this in the exit-code help.

**Fix and acceptance:** Delete the entry after `Result` maps a finished run, set `CompletedAt` at loop end, and add a test that `CompletedAt` is at most the time `Await` returned.

---

### C3-CODE-18 — Stale or inaccurate comments, help text and docs
**Severity:** Low
**File:** several

- **ModelName default.** `internal/config/config.go:137-138` and the `--fleet-model-name` help at `flags.go:41` say "empty selects the adapter's documented default". `buildWorker` requires the field and the adapter has no default.
- **Agent comment.** `config.go:72` says "Agent selects the tier: deep-research or deep-research-max", which omits worker and fleet.
- **Endpoint wording.** `config.go:129-130, 189, 274-275, 322` say endpoints are resolved "in a later wave". `buildWorker` resolves them now.
- **AGENTS.md package map.** It still says `internal/researcher/fleet` is "a stub that returns not-implemented" and has no rows for `fleet/search` or `fleet/fetch`.
- **DECISIONS.md contradiction.** The 2026-07-01 model-adapter entry says "worker reasoning/synthesis calls leave it [JSONSchema] nil". The worker sets it on every turn, and the worker-loop entry says so.
- **Test comments.** `worker_test.go:305` names `TestRunWorkerFinalPreservesCitationsOnLaterStop`, but the function is `TestRunWorkerFinalPreservesCitations`. `worker_test.go:224-227` describes behaviour the test does not implement.
- **Span comment.** The comment at `worker.go:146-149` does not match `span.End(nil)`, see C3-CODE-13.
- **CLI test comment.** `research_test.go` `TestWorkerAgentIsWired` refers to a "loopback model MCP-style endpoint". The model endpoint is not MCP.

**Fix:** Correct each item.
**Acceptance:** Grep for "adapter default", "later wave" and "a stub that returns" finds no remaining matches in the touched files.

---

### C3-CODE-19 — Comments break the code-comment policy with forward-looking and session narrative and 5+ line blocks
**Severity:** Low
**File:** several (examples below)

The policy bars speculation about future change and session or history framing.

Violations:
- `worker.go:20-25, 29, 48, 115`: "factored so Wave 4's lead can reuse it", "the lead will be another".
- `worker_researcher.go:20-21, 29`: "Wave 4's fleet lead is a separate binding that will…", "until control-plane durability lands".
- `worker_prompt.go:88`: "Wave 4's lead supplies…".
- `model/fake.go:12-13` and `search/fake.go:12, 39`: "the later worker/lead chunks", "reusing chunks". These are session vocabulary.
- `trace/names.go:18`: "Wave 4's lead reuses it".
- `cli/research.go:82, 340-342`: "until Wave 4", "Wave 4 flips fleet".
- `research_test.go` `TestFleetAgentNotYetWired`: "The worker agent is now wired", "Wave 4 replaces this".
- `worker.go:373, 406`: "unless a future rate is wired in", "a future error source".

Many doc comments run 6 to 12 lines of rationale that DECISIONS.md already records, for example
`worker_action.go:9-16, 49-59, 98-108`, `fetch.go:142-156`, `model.go:108-113` and
`search.go:111-116`.

**Fix:** Rewrite these in the present tense, stating what the code does and its invariant. Move rationale to DECISIONS.md, which already holds most of it. Keep doc comments to one to four lines.

**Acceptance:** Grep for `Wave [0-9]|chunk|will be|now wired|future` in the changed Go comments returns nothing.

---

### C3-CODE-20 — Test hygiene gaps
**Severity:** Low
**File:** internal/researcher/fleet/worker_testservers_test.go; internal/researcher/fleet/worker_test.go:184-192; internal/researcher/fleet/worker_researcher_test.go:86-163

- **Hidden server lifecycle.** `newFetchPage`, `newBlockingModelServer` and `newLeakySearchServer` hide `httptest.Server` construction in helpers, against AGENTS.md "do not hide server lifecycle inside helper functions". The exported `FakeServer`s are a recorded exception, but these unexported helpers are not.
- **Vacuous assertion.** In `TestRunWorkerRefusesSideEffectingActions`, `fetchSrv` is never referenced by any scripted action, so its `t.Error` guard can never fire. The comment on `searchSrv`, "fails the test if ever reached", is also inaccurate: it counts calls and does not fail.
- **Detached goroutines.** `TestWorkerStartReturnsImmediately` and `TestWorkerAwaitRespectsContext` leave `RunWorker` goroutines running after the test returns, a consequence of C3-CODE-2. In the first test, a `t.Errorf` from the Start goroutine after the 2s timeout would panic.
- **Missing coverage.** No tests exist for a mid-call timeout (C3-CODE-5), a fetch 404 (C3-CODE-3), `finish_reason: "length"`, or a large page (C3-CODE-4).

**Fix:** Inline the servers at the call site. Point a scripted fetch action at `fetchSrv` so the guard is live. Add the missing cases.

**Acceptance:** The new tests exist and pass. `go test -race ./internal/researcher/fleet/...` is clean.

---

### C3-CODE-21 — A same-host redirect from https to http would send the bearer token in cleartext
**Severity:** Low
**File:** internal/researcher/fleet/model/model.go:181-189; internal/researcher/fleet/search/search.go:204-212

`refuseCrossHostRedirects` compares `URL.Host` only. A same-host redirect from `https://api.example.com`
to `http://api.example.com` is followed, and net/http re-sends `Authorization` because the domain
matches. This is the same shape as the v1 `internal/interactions` helper, so it may be accepted
risk, but the credential-bearing endpoints are validated as https for exactly this reason.

**Fix:** Also refuse when `req.URL.Scheme != via[0].URL.Scheme`.
**Acceptance:** A redirect-to-http test is refused.

---

## Verified OK

- **Termination (turn count).** The loop always terminates on turns. `MaxTurns` is checked before each paid turn, `NewWorker` rejects `MaxTurns <= 0`, and config enforces a positive value with default 8. Every unseen-URL nudge also consumes a turn.
- **No auto-retry on paid calls.** There is no retry anywhere in the model, search or worker code, and `TestRunWorkerNoRetryOnPaidTurn` and `TestSearchToolCallNotRetried` pin it. A 307 or 308 same-host redirect would replay the POST, which is server-directed and acceptable.
- **Id emitted early.** `Start` returns the local id before any model call, and the run core emits it immediately.
- **Closed action vocabulary.** The dispatch is a closed switch. `parseAction` rejects unknown kinds and unknown fields, with no execution path for other kinds. Research-only holds.
- **HTTP body lifecycle.** Every `Do` is followed by `defer resp.Body.Close()`. Reads are bounded: model 16 MiB and error bodies 1 MiB, search 8 MiB including the aggregate SSE stream and scanner token, fetch capped with a truncation marker.
- **Per-call deadlines.** `context.WithTimeout` is applied per call and yields to a tighter caller deadline, as the tests show.
- **Credential handling.** Keys travel only in the `Authorization` header. Errors are scrubbed by exact key match plus `secret.Scrub`, and the worker scrubs `Detail` again. The OTel and JSONL bindings scrub span attributes, so the query in the `objective` attribute is covered. Cross-host redirects are refused for model and search.
- **Endpoint validation.** It is enforced in config, in `model.New`, and in `search.New`. Key refs must be `secret://`. `buildWorker` resolves secrets at the composition root only, and `AllowLoopback` is false in production.
- **Concurrency.** `workerState.finding` is written before `close(done)` and read only after `<-done`, a correct happens-before. The `runs` map is mutex-guarded.
- **Exit codes.** A worker `failed` or `incomplete` result maps to exit 2 via `exitForStatus`. Setup errors map to exit 1, and fleet still returns the typed "not yet wired" usage error.
- **Seam and dependency invariants.** No new third-party dependency, no vendor SDK, and `internal/run` is unchanged and seam-only.
- **Test tables.** Table loop variables are named `tt` throughout. No test reaches the real network: all servers are loopback `httptest`, and the fetch guard tests use literal IPs.
