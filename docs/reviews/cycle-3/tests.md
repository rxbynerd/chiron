# Cycle 3 review — test coverage audit (TEST)

Branch `feat/v2-research-agent` at `d4ac9f2`, against `origin/main`.
Reviewer role: test adequacy for the new fleet, config and CLI code.

## Summary

The suite is green and stable. It passes under `-race` at `-count=1`, `-count=3` and `-count=5`, and `go vet` is clean. The happy paths, the no-retry rule, the closed action vocabulary and the Interaction-level credential scrub are all pinned well.

The gaps are in the stop and failure paths that the spec calls out by name:

- A wall-clock timeout that fires mid-turn ends the run as **Failed**, not Incomplete. No test covers it. A probe confirmed the behaviour.
- No test asserts anything about the worker's span or metrics. The metrics are emitted after the span has ended, so the OTel binding drops them.
- The token cap has no test.
- The branch of the SSRF guard that resolves hostnames through DNS has no coverage. Every real search-result URL takes that branch.

The verdict is **NEEDS WORK**. The target is 90% on the critical paths (worker loop termination, SSRF guard, credential-bearing clients) and 80% elsewhere.

## Method

```
go test -race -count=1 -coverprofile=$SCRATCH/cover.out ./internal/researcher/fleet/... ./internal/config/... ./internal/cli/...
go tool cover -func=$SCRATCH/cover.out
go test -race -count=3 ./internal/researcher/fleet/...
go test -race -count=5 ./internal/researcher/fleet/... ./internal/cli/... ./internal/config/...
go test -count=1 -coverpkg=./internal/researcher/fleet/...,./internal/config/...,./internal/cli/... ...   # cross-package view
```

Behavioural probes used `go test -overlay` with test files kept in the session scratchpad. Nothing was written into the repository.

## Coverage table

Per-package statement coverage, measured without `-coverpkg`:

| Package | Coverage | Target |
|---|---|---|
| internal/researcher/fleet | 84.3% | 90% (critical: loop termination, action guard) |
| internal/researcher/fleet/fetch | 86.8% | 90% (critical: SSRF) |
| internal/researcher/fleet/model | 92.5% | 90% |
| internal/researcher/fleet/search | 86.2% | 80% |
| internal/config | 94.5% | 80% |
| internal/cli | 82.2% | 80% |
| Total (these packages) | 87.3% | — |

Functions that matter and fall below target. "Cross-pkg" is the `-coverpkg` figure, which counts calls from other packages' tests.

| Function | Per-pkg | Cross-pkg | Why it matters |
|---|---|---|---|
| fleet `RunWorker` (worker.go:122) | 61.9% | 95.2% | The traced path runs only in the CLI smoke test, with no assertions |
| fleet `remainingTokenBudget` (worker.go:383) | 33.3% | 33.3% | Token cap arithmetic |
| fleet `totalTokens` (worker.go:399) | 0.0% | 0.0% | Token cap measure |
| fleet `emitWorkerMetrics` (worker.go:416) | 0.0% | 83.3% | Spend signals, no assertions |
| fleet `loop` (worker.go:181) | 88.2% | 88.2% | Token and cost cap branches uncovered |
| fleet `doFetch` (worker.go:280) | 80.0% | 80.0% | Hard fetch failure path uncovered |
| fleet `parseAction` (worker_action.go:109) | 81.2% | 81.2% | Three malformed-output branches uncovered |
| fleet `Worker.Result` (worker_researcher.go:136) | 66.7% | 66.7% | In-progress branch |
| fetch `guardHost` (fetch.go:157) | 42.9% | 42.9% | DNS-resolution branch at 0% |
| model `refuseCrossHostRedirects` (model.go:181) | 40.0% | 40.0% | Same-host follow and hop cap |
| search `refuseCrossHostRedirects` (search.go:204) | 40.0% | 40.0% | Same-host follow and hop cap |
| search `readEventStream` (rpc.go:146) | 65.3% | 65.3% | SSE framing edge cases |
| search `errorFromResponse` (rpc.go:244) | 71.4% | 71.4% | Empty-body branch |
| model `errorFromResponse` (generate.go:222) | 71.4% | 85.7% | Empty-body branch |
| cli `buildWorker` (research.go:157) | 64.5% | 64.5% | Every validation and resolution error path |

## Findings

### C3-TEST-1 — A wall-clock timeout that fires mid-turn ends the worker as Failed, and no test covers it
**Severity:** High
**File:** internal/researcher/fleet/worker.go:223
The `Caps` contract says a cap stop is Incomplete:

```go
// Caps bound one worker run deterministically ... Exceeding any of them ends the loop with
// StatusIncomplete rather than an error
```

`turn` maps every `Generate` error to `failed`, including the context deadline set from `Caps.Timeout`:

```go
if err != nil {
    return true, w.failed("model turn failed: " + w.scrub(err.Error()))
}
```

The only timeout test is `TestRunWorkerTimeoutIsIncomplete` (worker_test.go:370). It cancels the context *before* the loop starts, so it only reaches the top-of-loop `ctx.Err()` check. An overlay probe set `Caps.Timeout` to 150ms and used a model handler that blocks until cancelled. The result was:

```
status=failed detail="model turn failed: model: POST /chat/completions: Post \"http://127.0.0.1:55440/chat/completions\": context deadline exceeded" elapsed=151.5ms
```

In practice the timeout nearly always fires during a model, search or fetch call, not between turns. So the documented Incomplete outcome is effectively unreachable. The same mis-mapping applies to `doSearch` and `doFetch`.
**Fix:** In `turn`, `doSearch` and `doFetch`, check `ctx.Err()` when a tool call returns an error, and return `w.incomplete(...)` when the context has ended. Then add tests:
- `TestRunWorkerTimeoutDuringModelTurnIsIncomplete`: the model handler drains the body and blocks on `r.Context().Done()`, with `Caps.Timeout` at 150ms. Assert Incomplete, a detail containing "context", and exactly one model call.
- `TestRunWorkerTimeoutDuringSearchIsIncomplete`: the same shape with a blocking MCP `tools/call` handler. Assert Incomplete and that the turn-1 usage is preserved.

**Acceptance:** Both tests pass. `TestRunWorkerTimeoutIsIncomplete` stays green.

### C3-TEST-2 — No test covers the worker's trace or metrics, and the metrics are emitted after the span ends
**Severity:** High
**File:** internal/researcher/fleet/worker.go:140
The spec's testing requirements (V2-RESEARCH-AGENT §8) require: "Trace/span scrub tests prove no model/search/Langfuse credential leaks". §5 acceptance adds "scrubbed from logs, traces, stderr". Every fleet test builds `WorkerDeps` with a nil `Tracer`, so the span path never runs in the fleet package (61.9% per-package). The CLI passes `trace.Noop{}`, which runs the path but records nothing.

The untested code also has an ordering defect that a recording-tracer test would expose:

```go
span.End(nil)
emitWorkerMetrics(ctx, deps.Tracer, finding.Usage)
```

`trace.OTel.Metric` sets span attributes on `SpanFromContext(ctx)` (otel.go:72). Here that is the worker span, which has already ended, so the OTel SDK drops the metrics. The worker's metric names also repeat the ones `run.recordMetrics` emits (run.go:267-269) from the same usage.
**Fix:** Emit the metrics before `span.End`, and decide whether the worker-level metrics duplicate the run-level ones. Then add tests:
- `TestRunWorkerTraceScrubsCredentials`: use `trace.NewJSONL(&buf)` as the tracer, a model 401 that echoes a high-entropy key, and a leaky search variant. Assert that `buf` never contains the key and does contain `status` = `failed` with a scrubbed detail.
- `TestRunWorkerMetricsLandOnOpenSpan`: use the OTel tracer with `tracetest.SpanRecorder`, following otel_test.go:17. Assert the ended worker span carries `metric.search_count` and `metric.input_tokens`.

**Acceptance:** Both tests pass. `RunWorker` reaches 100% per-package.

### C3-TEST-3 — No test covers the token cap
**Severity:** High
**File:** internal/researcher/fleet/worker.go:197
The spec requires that "Caps stop runaway workers deterministically". Only the turn cap is tested. The token-cap stop is uncovered (worker.go:197-199). So is the per-turn completion budget in `remainingTokenBudget`, including its floor of 1 (worker.go:387-391), and `totalTokens`. An overlay probe shows the logic works today but nothing pins it. It ran 40+10 tokens per turn against a cap of 120:

```
status=incomplete detail="reached the 120-token cap before a final answer" calls=3 max_tokens=[120 70 20]
```

**Fix:** Add tests:
- `TestRunWorkerTokenCapStops`: script ten search replies at 40 input and 10 output tokens with `MaxTokens` at 120. Assert Incomplete, a detail containing "120-token cap", and exactly three model calls. Assert that `modelSrv.Requests()` shows `MaxTokens` of `[120, 70, 20]`.
- `TestRunWorkerUncappedTokensSendsNoMaxTokens`: with `MaxTokens` at 0, every request has `MaxTokens == 0`.
- `TestRemainingTokenBudgetFloorsAtOne`: call `remainingTokenBudget` directly with usage above the cap. Assert it returns 1.

**Acceptance:** Those three statements are covered and `loop` reaches at least 95%.

### C3-TEST-4 — The DNS branch of the SSRF guard is untested, and the private-address tests would reach the real network if the guard regressed
**Severity:** High
**File:** internal/researcher/fleet/fetch/fetch.go:172
`guardHost` is at 42.9%. The branch that resolves a hostname and refuses if any answer is internal is at 0%:

```go
ips, err := net.LookupIP(host)
...
for _, ip := range ips {
    if isInternal(ip, allowLoopback) {
```

Search results are hostnames, so production takes this branch for essentially every fetch. Every SSRF test uses a literal IP. `net.LookupIP` is called directly, so there is no seam to stub DNS without the network.

The literal-IP tests also have weak assertions:
- `TestFetchPrivateAddressRefused` (fetch_test.go:73) and `TestFetchAllowLoopbackDoesNotRelaxPrivate` (fetch_test.go:100) assert only `err != nil`.
- If `isInternal` regressed, the client would really dial `10.0.0.1`, `192.168.1.1` or `169.254.169.254`. On a home network or a cloud CI runner, those addresses can answer.
- `TestFetchRedirectToPrivateRefused` (fetch_test.go:215) would likewise follow a redirect to the real metadata address.

**Fix:** Add an unexported resolver seam, for example a `lookupIP func(ctx, host) ([]net.IP, error)` field defaulting to `net.DefaultResolver.LookupIP`. Give the SSRF tests an `HTTPClient` whose `Transport.DialContext` calls `t.Error` and fails, so a regression fails loudly instead of dialling. Then add tests:
- `TestFetchHostnameResolvingInternalRefused`: stub `internal.example` to `10.0.0.5`. Assert an error containing "resolves to internal address" and no dial.
- `TestFetchHostnameMixedAnswersRefused`: stub a public and a private answer together. Assert refusal, since the guard refuses on the union.
- `TestFetchRedirectToHostnameResolvingInternalRefused`: a loopback start server redirects to `http://metadata.example/`, which is stubbed to `169.254.169.254`.
- `TestFetchResolveErrorSurfaced`: the stub returns an error. Assert a "resolving" error.
- `TestFetchEmptyHostRefused`: fetching `http:///x` fails with "no host".
- Extend the table in `TestFetchPrivateAddressRefused` with `[::ffff:127.0.0.1]`, `[::ffff:169.254.169.254]` and `[fd00::1]`. Assert the error contains "internal", not only `err != nil`.

One item is for the code reviewers rather than a test gap. `isInternal` does not refuse 100.64.0.0/10 (CGNAT, Tailscale) or 0.0.0.0/8 addresses other than `0.0.0.0`.
**Acceptance:** `guardHost` is at 100%. No SSRF test can open a socket to a non-loopback address. A test's dial hook proves this.

### C3-TEST-5 — Worker tests never assert what the model is sent
**Severity:** Medium
**File:** internal/researcher/fleet/worker_test.go:85
`model.FakeServer.Requests()` records every transcript, but no fleet test calls it. Every worker test asserts only outcomes, and the model replies are scripted. Any of these regressions would pass all worker tests and the golden:
- search results are not fed back;
- the fetched page text is dropped;
- the unseen-URL guidance message is missing;
- the action schema, schema name or strict flag is not sent;
- the Brief is not rendered into the system prompt.

The golden's final answer is scripted, so it does not show that the fetched page ever reached the model.
**Fix:** Add tests:
- `TestRunWorkerTranscriptCarriesToolOutput`, reusing the search, fetch and final script from `TestRunWorkerSearchFetchFinal`:
  - `Requests()[1].Messages` contains the search result URL;
  - `Requests()[2].Messages` contains "Rayleigh scattering makes the sky appear blue.";
  - every request has `ResponseFormatType == "json_schema"`, `SchemaName == "worker_action"`, `Strict == true` and a schema equal to `actionSchema`;
  - `Requests()[0].Messages[0]` is the system role and contains the objective.
- `TestRunWorkerUnseenFetchFeedsGuidance`: extend `TestRunWorkerRefusesUnseenFetchURL` (worker_test.go:333). Assert that the second request's last user message contains "did not appear in any search result".
- `TestRunWorkerTruncatedPageMarked`: the fetch client has `MaxContentBytes` at 16 and serves a longer page. Assert the next request contains "[truncated".

**Acceptance:** Deleting the `searchResultsMessage` or `fetchedPageMessage` append in worker.go makes a test fail.

### C3-TEST-6 — CLI worker failure paths, exit codes and error handling are untested
**Severity:** Medium
**File:** internal/cli/research.go:157
`buildWorker` is at 64.5%:
- every required-field error is uncovered (research.go:160, 166, 169, 172);
- secret-resolution failure is uncovered (research.go:177);
- the search-key resolution path is uncovered (research.go:183-186).

The only worker CLI test, `TestWorkerAgentIsWired` (research_test.go:400), covers the Completed path. The following are all untested:
- the mapping of a Failed or Incomplete worker onto `ExitResearchFailed`, through `exitForStatus` (research.go:534);
- scrubbing the model key from stderr, which the §5 acceptance lists explicitly;
- the current silent ignoring of `--budget` and `--plan` with `--agent worker`.

**Fix:** Add tests:
- `TestWorkerAgentMissingFleetConfig`: a table with `tt` cases for missing model endpoint, model name, model key ref and search endpoint, plus an unresolvable `secret://`. Assert an error naming the field, that it is not an `*ExitError`, and that the model fake has zero calls.
- `TestWorkerAgentFailedExitCode`: the model returns a 401 echoing `sk-proj-…`. Assert an `ExitError` with code `ExitResearchFailed`, and that neither stdout nor stderr contains the key.
- `TestWorkerAgentIncompleteExitCode`: `--fleet-max-turns 1` with a search reply. Assert code `ExitResearchFailed`, which is today's contract, and that the report front matter shows `status: incomplete`.
- `TestWorkerAgentSendsSearchKey`: set `--fleet-search-key-ref secret://SEARCH_KEY` and script one search. Assert the search fake's `Authorization` is `Bearer <value>`.
- `TestWorkerAgentBudgetFlag`: pin the chosen behaviour. The flag should either be rejected as not applicable or be honoured. It should not be silently accepted. This is a code decision; flag it to the code and spec reviewers.

**Acceptance:** `buildWorker` reaches at least 90% and every worker terminal status has a CLI exit-code test.

### C3-TEST-7 — The MCP client's SSE framing and JSON-RPC error frames lack tests in their own package
**Severity:** Medium
**File:** internal/researcher/fleet/search/rpc.go:146
`readEventStream` is at 65.3%. Only the single-frame happy path and the oversize bound are tested. The following are uncovered:
- skipping a server notification frame that precedes the reply (rpc.go:175-176);
- joining multi-line `data:` fields (rpc.go:203-205);
- the per-event data bound (rpc.go:208-210);
- scanner errors, including `bufio.ErrTooLong` (rpc.go:216-220);
- a trailing frame with no blank line (rpc.go:224-228);
- "carried no JSON-RPC response" (rpc.go:229);
- SSE decode errors (rpc.go:168-170).

Real Streamable-HTTP servers often keep the stream open after the reply. No test proves the reader returns on the first response frame without waiting for EOF.

Error frames are also thin:
- The `initialize` rejection (mcp.go:107-108) is uncovered in every package.
- The `tools/call` JSON-RPC error is covered only through the fleet package's leaky server.
- `TestSearchToolCallNotRetried` (search_test.go:213) says "A server error on tools/call must not be retried", but it scripts a non-JSON result, which is a decode error, not an HTTP 5xx.

**Fix:** Add tests in search_test.go. Each handler lives at the call site and writes through an `sseWrite(t, w, frame)` helper that flushes:
- `TestSearchSSESkipsNotificationBeforeReply`
- `TestSearchSSEMultiLineData`
- `TestSearchSSETrailingFrameWithoutBlankLine`
- `TestSearchSSENoResponseIsError`
- `TestSearchSSEMalformedDataIsError`
- `TestSearchSSEReturnsWithoutEOF`: the handler writes the reply, flushes, then blocks on `r.Context().Done()`. Assert that Search returns within 1s.
- `TestSearchInitializeRejected`: a JSON-RPC `error` on initialize. Assert "initialize rejected" and zero `tools/call` requests.
- `TestSearchToolCallRPCErrorScrubbed`: a `tools/call` error echoing `testKey`. Assert the message is scrubbed.
- `TestSearchToolCall5xxNotRetried`: a 502 on tools/call. Assert `ToolCallCount() == 1`.
- `TestSearchNonTextBlocksIgnored`: an image block precedes a text block. Assert the text is used.

**Acceptance:** `readEventStream` is at least 90% and `initialize` and `callSearch` are at 100%.

### C3-TEST-8 — Redirect-hop caps and several model decode failures are untested in the credential-bearing clients
**Severity:** Medium
**File:** internal/researcher/fleet/model/model.go:185
`refuseCrossHostRedirects` is at 40% in both the model and search clients (model.go:185-188, search.go:208-211). Only the cross-host refusal is tested, not the same-host follow or the three-hop cap. On a POST, a 307 or 308 same-host redirect replays the paid request body. It deserves an explicit test.

`Generate` also has uncovered failure paths:
- a malformed 200 body (generate.go:173-174);
- `choices: []` (generate.go:176-177);
- an empty non-2xx body (generate.go:228-229).

**Fix:** Add tests:
- `TestSameHostRedirectFollowed` in model and search: a 307 to the same host's path. Assert success and one POST at the final path.
- `TestRedirectHopCap` in model and search: an endless same-host 307 loop. Assert "too many redirects" and at most three handler hits.
- `TestGenerateMalformedBody`: `RawBody: "not json"`. Assert "decoding response".
- `TestGenerateNoChoices`: `RawBody: {"choices":[]}`. Assert "no choices".
- `TestGenerateEmptyErrorBody`: status 503 with an empty body. Assert the error ends with "HTTP 503".

**Acceptance:** Both redirect policies and `Generate` are at 100%.

### C3-TEST-9 — Malformed-model-output cases in `parseAction` are incomplete
**Severity:** Medium
**File:** internal/researcher/fleet/worker_action.go:109
These branches are uncovered:
- an empty or whitespace reply (worker_action.go:111-112);
- `search` with no query (worker_action.go:123-124);
- `fetch` with no url (worker_action.go:127-128).

Nothing pins how prose, Markdown-fenced JSON or length-truncated JSON behave. The last happens when `finish_reason` is "length", which the worker never inspects.

`json.Decoder.Decode` reads only the first value, so trailing data is accepted today. A standalone check with `DisallowUnknownFields` confirmed this: `{"action":"final","answer":"x"} {"action":"shell"}` decodes with no error as a final.
**Fix:** Add `TestParseAction`, a table with `tt` cases:
- `""` and `"  \n"`;
- `{"action":"search"}` and `{"action":"search","query":"  "}`;
- `{"action":"fetch"}`;
- prose;
- a Markdown-fenced object;
- a truncated object;
- trailing data after a valid object.

Assert an error for each. Add positive cases for the three kinds, including a final with empty citations. For trailing data, add a `dec.More()` check in `parseAction`, or record the acceptance deliberately. Add `TestRunWorkerTruncatedReplyFails`: a reply with `FinishReason: "length"` and cut-off JSON. Assert Failed with "invalid model action" and one model call.

**Acceptance:** `parseAction` is at 100%, with the trailing-data behaviour pinned either way.

### C3-TEST-10 — The worker's hard fetch failure and the Researcher binding's defensive branches are untested
**Severity:** Medium
**File:** internal/researcher/fleet/worker.go:294
A hard fetch error ends the run as Failed (worker.go:295-297), but no test covers it. That includes an SSRF refusal on redirect, a non-2xx response and a transport error. The "nil fetch client" branch (worker.go:290-291) cannot be reached through `NewWorker`, but it can through `RunWorker`, which is the Wave 4 lead's entry point. The Researcher binding's unknown-id paths and the in-progress `Result` are also uncovered (worker_researcher.go:122, 139, 147-154).
**Fix:** Add tests:
- `TestRunWorkerFetchFailureIsFailed`: search returns a loopback page URL that responds 500. Assert Failed with "fetch failed", `SearchCount == 1`, and two model calls.
- `TestRunWorkerNilFetchClientFails`: call `RunWorker` directly with a nil `Fetch` and a search-then-fetch script. Assert Failed and zero fetch attempts.
- `TestWorkerAwaitUnknownID` and `TestWorkerResultUnknownID`: assert errors.
- `TestWorkerResultBeforeCompletion`: use the blocking model. `Result` returns `StatusInProgress` with the query and tools set, and does not block.

**Acceptance:** `doFetch`, `Await`, `Result` and `state` are at 100%.

### C3-TEST-11 — Test-infrastructure conventions from AGENTS.md are broken, and the test fakes ship in the binary
**Severity:** Low
**File:** internal/researcher/fleet/worker_testservers_test.go:14
AGENTS.md says: "Create `httptest.Server` at the call site … do not hide server lifecycle inside helper functions". Three helpers construct servers:
- `newFetchPage` (line 14);
- `newBlockingModelServer` (line 27);
- `newLeakySearchServer` (line 39).

The `newWorker` helper (worker_researcher_test.go:19) also creates the search fake internally and closes it through `t.Cleanup`, which hides the lifecycle from the call site.

The SSE fake writes raw output with no flush helper:

```go
_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
```

This is search/fake.go:189. AGENTS.md asks for an `sseWrite`-style helper that flushes.

`model/fake.go` and `search/fake.go` are non-test files that import `net/http/httptest`. As a result, `go list -deps ./cmd/...` shows `net/http/httptest` linked into the `chiron` binary. No non-test file on `origin/main` imports it.
**Fix:**
- Inline the three handlers at their call sites with `defer srv.Close()`.
- Have `newWorker` take the search fake from its caller.
- Route the SSE write through a flushing helper.
- Move the fakes to test-support sub-packages such as `model/modeltest` and `search/searchtest`, or behind a `_test.go` export. Either way keeps `httptest` out of the binary.

**Acceptance:** `grep -n httptest.NewServer` shows no helper-constructed servers. `go list -deps ./cmd/chiron | grep httptest` is empty.

### C3-TEST-12 — Two deadline tests cost 2s each because their handlers never see cancellation
**Severity:** Low
**File:** internal/researcher/fleet/model/model_test.go:286
`TestCallerContextDeadlineWins` and `TestSearchCallerContextDeadlineWins` (search_test.go:348) each take exactly 2.00s under `-v`. The handler never reads the POST body, so net/http never starts the background read that cancels `r.Context()`. The handler therefore waits out its 2s fallback timer, and `server.Close` blocks on it. An overlay probe drained the body with `io.Copy(io.Discard, r.Body)` first, and the same test finished in 0.1s. The assertions are correct but mask the issue. Across `-count=5` this adds about 20s.
**Fix:** Drain `r.Body` before the `select` in both handlers. The fetch variants use GET and are unaffected.
**Acceptance:** Both tests run in under 300ms under `-race`.

### C3-TEST-13 — Some test comments and assertions do not match what the tests check
**Severity:** Low
**File:** internal/researcher/fleet/worker_test.go:220
- `TestRunWorkerCapsStopDeterministically` says the citations gathered before the cap are preserved. It does not assert this, and it cannot hold, because `addCitation` is called only from `finalise`. A capped or failed run always has zero citations. Either correct the comment, or have search results feed citations and assert it.
- The doc comment of `TestRunWorkerFinalPreservesCitations` (worker_test.go:305) names `...OnLaterStop`.
- In `TestRunWorkerRefusesSideEffectingActions` (worker_test.go:189), `fetchSrv` is never referenced by any scripted action, so its `t.Error` guard cannot fire. Put `fetchSrv.URL` in the `write`/`fetch`-shaped cases, or drop it.
- The golden test comment (worker_golden_test.go:132) says it checks that the URL-less result "never became a citation", but it only checks body text. The golden bytes do cover this; fix the comment. Its local variable `cap` also shadows the builtin.
- `TestFetchSchemeRejected` (fetch_test.go:136-141) calls `err.Error()` after a non-fatal `t.Errorf`, so a regression panics instead of reporting. Use `t.Fatalf`.
- `TestFetchRedirectDepthCapped` (fetch_test.go:243) mutates `hops` from the handler without synchronisation. It is race-clean only because keep-alive serialises requests on one connection goroutine. Use `atomic.Int32`.

**Fix:** As listed.
**Acceptance:** The comments match the assertions, with no nil-deref-on-failure patterns and no unsynchronised handler state.

### C3-TEST-14 — Minor gaps in config and the golden test
**Severity:** Low
**File:** internal/config/config.go:349
- The `http://localhost` acceptance branch of `allowedEndpointScheme` (config.go:349-350) is uncovered. Add `"model endpoint http localhost"` and `"search endpoint http 127.0.0.1"` cases to `TestValidateAcceptsWorkerAndFleet`.
- `TestValidateRejectsBadFleet` (fleet_test.go:83) asserts only `err != nil`. Add a `field` column and assert the error contains it, for example `fleet.max_turns`, so a case cannot pass for the wrong reason.
- `CeilingGBP` is inert, because `EstimatedCostGBP` is never incremented (worker.go:372-374, DECISIONS.md:904). No test pins that `--fleet-ceiling` has no effect today. Add `TestRunWorkerCostCeilingInertWithoutRate` so a future rate wiring flips it knowingly.
- The golden's `volatilePort` normaliser (worker_golden_test.go:33) matches only `127.0.0.1:\d+`. `httptest` falls back to `[::1]` on hosts without IPv4 loopback, and the golden would then fail. Also normalise `\[::1\]:\d+`.

**Fix:** As listed.
**Acceptance:** `allowedEndpointScheme` in config is at 100% and the negative cases assert the field name.

## Flakiness

| Run | Result |
|---|---|
| `-race -count=1` (six packages) | pass |
| `-race -count=3 ./internal/researcher/fleet/...` | pass (fleet 1.8s, fetch 2.0s, model 7.3s, search 7.4s) |
| `-race -count=5` (fleet, cli, config) | pass |

The model and search runtimes come from C3-TEST-12, not from flakiness. The new tests use no `time.Sleep`. The time-based assertions allow wide margins: 100ms deadlines against a 1.5s ceiling, and a 2s bound on `Start`. The detached worker goroutines in `TestWorkerStartReturnsImmediately` and `TestWorkerAwaitRespectsContext` are released by `defer close(release)` before the server closes, so they do not leak past the test.

## Verified OK

- All table tests in the new files name their loop variable `tt`. The remaining `range` loops are not table tests.
- No test hits the real network today. Fetched URLs are loopback, SSRF-refused before dialling, or never fetched, such as the `example.org` citations. C3-TEST-4 covers the risk if that regresses.
- The golden test (worker_golden_test.go) is deterministic across `-count=5`. It runs the unchanged `run.Run` and compares the whole normalised report byte for byte, including front matter, token totals, search count, tools, sources and body. The id, timestamps and port are the only normalised fields. `testdata/worker-report.md` matches the scripted run.
- No-retry is pinned: model 5xx gives exactly one call (worker and model packages), and search `tools/call` gives exactly one call.
- The closed action vocabulary is pinned for shell, write, exec, unknown, a missing discriminator and extra-field smuggling. Each case asserts zero tool calls and one model turn.
- The model and search keys are scrubbed from `StatusDetail` and outputs at the Interaction level, and from the errors of both clients.
- Cross-host redirects are refused by both credential-bearing clients. The fetch client refuses a redirect to link-local and caps redirect depth.
- Body bounds are tested for the model JSON body, search JSON, search SSE, and fetch truncate-and-mark, including the exact-bound case.
- Config round-trips through JSON, strict decode rejects typos inside `fleet`, validation errors never echo a literal key, and fleet-only caps are ignored for the worker. Flag overlay is covered by flags_fleet_test.go.
- `--agent fleet` fails with a non-`ExitError` "not yet wired" before any secret is resolved.
- `go vet` is clean on all six packages.
