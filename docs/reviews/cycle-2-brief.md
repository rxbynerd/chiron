# Remediation Brief — Chiron Cycle 2 (Final v1)

**Scope:** Full v1 — M0–M7 + cycle-1 remediation; HEAD `90fe191` on `main`  
**Reviewers:** code-reviewer · security-reviewer · spec-compliance-reviewer · test-coverage-auditor · consistency-reviewer  
**Date:** 2026-06-07  
**Raw findings:** 27 (5 code · 1 security · 1 spec · 13 test-coverage · 7 consistency)  
**After deduplication:** 27 — no merges; all findings are genuinely distinct  
**Distribution:** 1 Critical · 5 High · 7 Medium · 14 Low

**Re-ratings:** None. All reviewer severity assignments are consistent; no cross-reviewer scale conflicts.

**Synthesizer observations:**

- The team-lead brief described tests.md as "2 Critical, 3 High"; the report contains **1 Critical + 4 High**. C2-TEST-2 (`statusDetail` 1-of-5) is the likely candidate for a second Critical in that count — it is placed first within High and its blast radius is noted. The test reviewer's High classification is used.
- C2-TEST-3 (`planner.round` ErrRequiresAction path) contains an assertion about code correctness, not just test coverage: the test reviewer states that `planner.go:133` wraps `ErrRequiresAction` non-transparently, meaning `errors.Is` would return false and the run would surface as an infrastructure error. The code reviewer did not exercise this path. C2-TEST-3's fix includes verifying the error chain is transparent; this may require a code fix in addition to the test.

---

## Critical — fix first

---

### C2-TEST-1 — `eventStatus` at 0% coverage; `interaction.completed` and `interaction.created` terminal paths completely untested
**(1-reviewer: test-coverage-auditor)**  
**File:** `internal/researcher/gemini/stream.go:145–176`

`consumeStream` has two event branches: `EventInteractionStatusUpdate` (exercised by every existing stream test) and `EventInteractionCompleted`/`EventInteractionCreated` (exercised by nothing). The `eventStatus` helper at line 168–176 — which decides whether a `requires_action` status carried on a completion event is propagated — sits at **0.0% coverage**. If `eventStatus` were broken and always returned `""`, a `requires_action` received via `interaction.completed` would silently call `requiresActionErr("")`, return `nil`, and allow the run to proceed to the Result fetch as if the interaction completed successfully — wrong exit code, no diagnostic, no user action taken.

The re-attach path is equally untested: when `?last_event_id=` reconnects past the end of an already-completed interaction, the first event received is `interaction.created` with a terminal status. This path returns early from the await loop, skipping the poll fallback, via `requiresActionErr(eventStatus(ev))`. Correct in design; entirely unverified in practice.

**Fix — three tests in `internal/researcher/gemini/stream_test.go`:**

1. `TestAwaitStreamInteractionCompletedEvent`: server sends an `interaction.completed` SSE event (with and without an embedded `interaction` object containing `"status":"completed"`); `Await` returns `nil`; `ReconnectCount` and `PollCount` are both 0; `awaitPoll` is never called.

2. `TestAwaitStreamRequiresActionViaCompletedEvent`: server sends `interaction.completed` with embedded `{"status":"requires_action"}`; assert `errors.Is(err, interactions.ErrRequiresAction)` is true; assert the poll fallback is not reached.

3. `TestAwaitStreamInteractionCreatedAlreadyTerminal`: server sends `interaction.created` with `{"status":"completed"}` as the very first event (simulates re-attaching past the end); `Await` returns `nil` immediately without entering the poll fallback.

**Cross-reference:** The wire shape for `interaction.completed` with an embedded `interaction` object is already exercised by `TestStreamEventSequence` in the interactions package — use the same JSON structure in these gemini-package tests.

**Acceptance criteria:**
- All three tests pass with `-race`.
- `go tool cover` on `internal/researcher/gemini` shows `eventStatus` above 0% and both early-return branches of `consumeStream`'s completion handling covered.
- `TestAwaitStreamRequiresActionViaCompletedEvent` specifically asserts `errors.Is`, not just a non-nil error, to pin the typed-error guarantee through the `consumeStream` path.

---

## High — fix in this remediation wave

---

### C2-TEST-2 — `statusDetail` covers only `StatusFailed`; four failure statuses produce unverified report content
**(1-reviewer: test-coverage-auditor)**  
**File:** `internal/researcher/gemini/gemini.go:523` (`statusDetail`; 42.9% coverage)

`statusDetail` maps five terminal failure statuses — `failed`, `cancelled`, `incomplete`, `budget_exceeded`, `requires_action` — to human-readable strings. These strings populate `Interaction.StatusDetail`, which the formatter writes into the `status_detail` YAML front-matter field of every placeholder report. `TestFailedInteractionCarriesDetail` covers only `StatusFailed`. The detail strings for the other four statuses are never asserted: a swapped or emptied string would silently corrupt every placeholder report for those outcomes, with no test failing.

**Fix:**  
Extend `TestFailedInteractionCarriesDetail` into a table test (or add four sibling cases) in `internal/researcher/gemini/gemini_test.go`, covering each of `cancelled`, `incomplete`, `budget_exceeded`, and `requires_action`. Use the `Await` + `Result` pattern matching the existing test: server returns the relevant wire status; assert `in.StatusDetail` is non-empty and contains a string specific to that outcome (i.e., does not contain the `"failed"` detail text).

**Acceptance criteria:**
- All five status cases are exercised by the test.
- Each assertion checks for status-specific text — not just non-empty — so a transposition between detail strings is caught.
- `go test ./internal/researcher/gemini/... -run TestFailedInteraction -v` shows all five cases.

---

### C2-TEST-3 — `planner.round` untested for empty plan text and `ErrRequiresAction`; possible non-transparent error wrap
**(1-reviewer: test-coverage-auditor)**  
**File:** `internal/researcher/gemini/planner.go:112–140` (80.0% coverage)

Two `planner.round` paths are unexercised:

1. **Empty plan text guard** (planner.go:138): `PollUntilTerminal` returns a `completed` interaction with no `FinalText()`. The empty guard is not defensive no-op code — the Interactions API is beta and empty `model_output` steps are plausible. An empty plan renders nothing, silently spending a round budget with no user feedback.

2. **`ErrRequiresAction` from a plan interaction** (planner.go:133): the test reviewer asserts that the planner wraps the typed error as `"awaiting plan interaction: ..."`, which would break `errors.Is(err, ErrRequiresAction)` in `concludeRun` — surfacing a `requires_action` plan outcome as an infrastructure error with the wrong exit code. **Synthesizer note:** the code reviewer did not exercise this path; the transparent-wrap assertion is unverified by code review. The test should be written first to confirm or deny whether a code fix is also required.

**Fix — two tests in `internal/researcher/gemini/planner_test.go`:**

1. `TestPlannerRoundCompletesWithoutText`: server returns `"status":"completed"` with `steps` containing no text part; `Propose` returns a non-nil error whose message contains `"without plan text"` (or the actual guard text in the code).

2. `TestPlannerRoundRequiresAction`: server returns `"status":"requires_action"` for the plan GET; the test asserts `errors.Is(err, interactions.ErrRequiresAction)` is true through the planner wrapper. **If this assertion fails, the planner's error wrap at line 133 must be changed from `%v` to `%w`** (or equivalent transparent wrapping) to restore the typed-error contract.

**Acceptance criteria:**
- Both tests pass.
- `TestPlannerRoundRequiresAction` uses `errors.Is`, not string matching.
- If the test reveals a non-transparent wrap: the fix changes the wrap verb to `%w` and both tests pass with `-race`.
- Coverage for `round` rises above 80.0%.

---

### C2-TEST-4 — `loadBase` `--config <file>` path untested; config-file errors silently undetected
**(1-reviewer: test-coverage-auditor)**  
**File:** `internal/cli/commands.go:160` (`loadBase`; 41.7% coverage)

`loadBase` has four paths: no config (exercised everywhere), piped stdin (exercised by `TestResearchConfigPipelineComposition`), `path == "-"` (stdin literal, untested in isolation), and `path != ""` (explicit `--config <file>`, entirely untested). The explicit-file path is how an operator supplies a persistent base config to automate runs. Neither a missing file nor malformed YAML in that path is caught by any test. A regression silently breaking `--config` file loading would reach production undetected.

**Fix — two tests in `internal/cli/levers_test.go` or a new `internal/cli/config_test.go`:**

1. `TestLoadBaseExplicitFile`: write a temp YAML file with `agent: deep-research-max`; run `research-config --config <path>`; assert the JSON output carries `"agent":"deep-research-max"`.

2. `TestLoadBaseExplicitFileMissing`: pass `--config /no/such/file.yaml`; assert the command returns a non-nil error before any seam is built and no HTTP request is made.

**Acceptance criteria:**
- Both tests pass.
- `TestLoadBaseExplicitFile` asserts the overriding field is in the output, not just that the command succeeds (to catch a silent-no-op regression).
- `loadBase` coverage rises above 41.7%.

---

### C2-TEST-5 — `config.Duration.UnmarshalJSON` at 0%; JSON native round-trip not verified
**(1-reviewer: test-coverage-auditor)**  
**File:** `internal/config/duration.go:24` (`UnmarshalJSON`; 0.0% coverage)

`TestJSONRoundTripThroughDecode` calls `EncodeJSON` then `Decode`. `Decode` uses a `yaml.v3` decoder — which accepts JSON as a YAML subset and routes duration values through `UnmarshalYAML`, not `UnmarshalJSON`. `UnmarshalJSON` is never called. If it is broken (its `set` delegate has its own branch gaps at 45.5%), a `json.Unmarshal` call on a config document with a duration field would silently mis-parse (nanosecond integer rather than human-readable string), with no test failing.

This matters for any consumer that deserialises `research-config` output with `encoding/json` rather than a YAML decoder — including pipeline tooling that processes JSON natively.

**Fix:**  
Add one case to `internal/config/config_test.go`:

```go
// JSON native round-trip: EncodeJSON output must survive json.Unmarshal
b, _ := json.Marshal(cfg) // or use EncodeJSON into a buffer
var got ResearchConfig
require.NoError(t, json.Unmarshal(b, &got))
assert.Equal(t, cfg.Timeout, got.Timeout)
```

The JSON produced by `EncodeJSON` uses string-form durations (e.g. `"30m0s"`); `UnmarshalJSON` must correctly parse that form back into `Duration`.

**Acceptance criteria:**
- The test calls `encoding/json.Unmarshal` directly (not `yaml.Decoder.Decode`) on output produced by `EncodeJSON`.
- The `Timeout` field in the unmarshalled struct equals the original value.
- `UnmarshalJSON` coverage rises above 0%.
- If `UnmarshalJSON` or `set` contains bugs exposed by the test, fix them before the test is considered passing.

---

### C2-CONS-1 — Wrong error prefix in `gemini/stream.go`; `"interactions:"` instead of `"gemini:"`
**(1-reviewer: consistency-reviewer)**  
**File:** `internal/researcher/gemini/stream.go:130, 139`

Two errors in `consumeStream` carry the prefix `"interactions:"`:

```go
return false, fmt.Errorf("interactions: stream ended before the interaction concluded: %w", err)
return false, fmt.Errorf("interactions: stream error event: %w", ev.Err)
```

Every other error in the `researcher/gemini` package uses `"gemini:"` (e.g. `"gemini: creating interaction"`, `"gemini: awaiting interaction %s"`). When one of these errors propagates through `Await` → `run.go` → `root.Execute()` and appears on stderr, the `"interactions:"` prefix directs a debugging engineer to `internal/interactions` rather than the gemini adapter's streaming layer — the wrong package.

**Fix:**  
Replace both prefixes:

```go
return false, fmt.Errorf("gemini: stream ended before the interaction concluded: %w", err)
return false, fmt.Errorf("gemini: stream error event: %w", ev.Err)
```

**Acceptance criteria:**
- `grep -n '"interactions:' internal/researcher/gemini/stream.go` returns nothing after the fix.
- Existing stream tests continue to pass (no test keys on the error prefix text).
- A quick grep confirms no other `"interactions:"` prefix appears in files under `internal/researcher/`.

---

## Medium — fix in this wave

---

### C2-TEST-6 — `awaitStream` context-cancellation path untested; cancellation during backoff unverified
**(1-reviewer: test-coverage-auditor)**  
**File:** `internal/researcher/gemini/stream.go:66` (`awaitStream`; 86.2% coverage)

`TestPollContextCancellation` verifies that context cancellation stops polling. No equivalent test exists for the streaming await. Specifically: if the context is cancelled while `awaitStream` is sleeping in `backoff` (after a stream dial failure), the select in `backoff` should return `ctx.Err()` without falling back to polling. This is the correct behaviour per DECISIONS.md ("context cancellation never falls back"), but it is unverified.

**Required test:** `TestAwaitStreamCancelledDuringBackoff` in `internal/researcher/gemini/stream_test.go`: server returns a non-SSE response on the first dial (triggering the backoff sleep); context is cancelled during that sleep; assert `errors.Is(err, context.Canceled)` and that `awaitPoll` is not called.

**Acceptance criteria:**
- Test uses the existing `streamOpts` pattern to set a very short backoff, then cancels the context before the sleep expires.
- `errors.Is(err, context.Canceled)` asserts the propagated error (not just non-nil).
- Zero `GET` requests after the first failure confirm no poll fallback.

---

### C2-TEST-7 — `mimeFromName` fallback and additional MIME types untested
**(1-reviewer: test-coverage-auditor)**  
**File:** `internal/researcher/gemini/input.go:99` (`mimeFromName`; 33.3% coverage)

`TestMultimodalInputParts` covers only `.png` and `.pdf`. The function also handles `.jpg`/`.jpeg`, `.gif`, `.txt`, and an unknown-extension fallback (returns `"application/octet-stream"`). A wrong MIME type on a multimodal part reaches the API directly and may cause the create to fail or the model to misinterpret the grounding document.

**Required test:** Extend `TestMultimodalInputParts` (or add a table test) for `.jpg`, `.jpeg`, `.gif`, `.txt`, and an unknown extension (`foo.bin`). Assert the MIME type in the decoded create-request body for each case.

**Acceptance criteria:**
- `.jpg` and `.jpeg` both produce `"image/jpeg"`.
- `.bin` (unknown) produces `"application/octet-stream"`.
- `mimeFromName` coverage rises above 33.3%.

---

### C2-TEST-8 — `formatter/extensionFor` untested for JPEG and GIF MIME types
**(1-reviewer: test-coverage-auditor)**  
**File:** `internal/formatter/markdown.go:230` (`extensionFor`; 57.1% coverage)

Golden tests exercise PNG and SVG. JPEG (two MIME aliases: `image/jpeg`, `image/jpg`) and GIF are never tested. An incorrect extension in an asset name produces a broken relative image link in the Markdown report.

**Required test:** Add unit cases (table test or golden fixture) for `image/jpeg`, `image/gif`, and an unknown MIME type; assert `.jpg`, `.gif`, and `.bin` respectively.

**Acceptance criteria:**
- The unknown-MIME fallback returns `.bin` (or the actual fallback the code uses), not an empty string.
- `extensionFor` coverage rises above 57.1%.
- Existing golden-file tests continue to pass.

---

### C2-TEST-9 — `ApplyFlags` missing coverage for `--budget`, `--timeout`, `--out`, `--output`
**(1-reviewer: test-coverage-auditor)**  
**File:** `internal/config/flags.go:42` (`ApplyFlags`; 58.1% coverage)

`--budget`, `--timeout`, `--out`, `--template`, `--input`, `--file-search`, and `--output` are not exercised at the `ApplyFlags` unit level. Most are covered transitively by CLI e2e tests. The unit-level gap means a pflag type-coercion bug (e.g., `--budget` bound to the wrong field, or `--timeout` silently capped at zero) would not be caught without tracing through the full CLI stack.

**Required tests:** Add table cases to `TestApplyFlagsOverlaysOnlySetFlags` (or a dedicated `TestApplyFlagsMoneyFlags`) covering at minimum: `--budget 2.50` → `BudgetGBP == 2.50`; `--timeout 45m` → `Timeout == 45*time.Minute`; `--output json` → `Output == "json"`; `--out /tmp/x.md` → `OutPath == "/tmp/x.md"`.

**Acceptance criteria:**
- Each test case sets only the one flag under test (via `Set`) and asserts only that field changed; other fields remain at their defaults.
- `ApplyFlags` coverage rises above 58.1%.

---

### C2-TEST-10 — `conclude` nil-Interaction guard untested; seam contract unverified
**(1-reviewer: test-coverage-auditor)**  
**File:** `internal/run/run.go:196–198`

The guard `if in == nil && err == nil { err = errors.New("run: researcher returned no interaction") }` is reachable if a researcher implementation's `Result` returns `nil, nil`. No test exercises this path. A buggy researcher stub that returns `nil, nil` would currently cause `conclude` to proceed to formatting and panic downstream, rather than returning a clear error.

**Required test:** In `internal/run/` package tests: a fake `Researcher` whose `Result` returns `nil, nil` after a successful `Await`; assert `run.Run` returns a non-nil error (not a panic). This is a seam contract test.

**Acceptance criteria:**
- Test uses the fake researcher pattern already established in `run_test.go`.
- `run.Run` returns a non-nil error; the test does not rely on `recover()`.
- No change required to the guard itself — it is already correct.

---

### C2-CONS-2 — Table-test loop variable diverges: `tt` (majority) vs `tc` (gemini, CLI)
**(1-reviewer: consistency-reviewer)**  
**File:** `internal/researcher/gemini/gemini_test.go:453` · `gemini/stream_test.go:330` · `internal/cli/research_test.go:185`

Three packages (`interactions`, `secret`, `config`) use `tt` as the table-test loop variable; `researcher/gemini` and `cli` use `tc` in isolated cases. The gemini package is internally split: most tests use `t` or `tt`; the three affected loops use `tc`. No rationale distinguishes the two styles.

**Action:** Rename the three `tc` loop variables to `tt`:
- `gemini/gemini_test.go:453` — `TestConstructionValidation`
- `gemini/stream_test.go:330` — `TestThinkingSummariesFollowsStreamOption`
- `cli/research_test.go:185` — `TestStreamTogglesThinkingSummaries`

**Acceptance criteria:** `grep -rn '\bfor _, tc :=' internal/researcher/gemini/ internal/cli/` returns nothing after the rename. All tests pass.

---

### C2-CONS-3 — httptest server lifecycle pattern undocumented; two styles in use
**(1-reviewer: consistency-reviewer)**  
**File:** `internal/interactions/client_test.go` (internal close via `t.Cleanup`) vs `internal/researcher/gemini/gemini_test.go` (caller-managed `defer server.Close()`)

Neither pattern is wrong. The gemini/CLI pattern (caller-managed, explicit `defer`) is used by two packages and is more flexible (allows inspecting or varying the handler). The interactions pattern (helper-managed `t.Cleanup`) is used by one package. For new packages, the caller-managed pattern should be followed; this choice is not currently documented.

**Action:** Add a test-infrastructure note to `AGENTS.md`: "For new test packages, create `httptest.Server` at the call site with `defer server.Close()`; do not hide server lifecycle inside helper functions." No code changes required to existing tests.

**Acceptance criteria:** The note appears in `AGENTS.md` under the existing test-infrastructure section (or as a new one). No test changes.

---

## Low — non-blocking; address in this wave where trivial

Grouped by file where multiple findings touch the same location.

---

### Security and scrubbing

**C2-SEC-1 — Thought-summary text bypasses `secret.Scrub` before stderr emission**  
*(security-reviewer Low; cross-reference: OnThought path exercised by `TestResearchEndToEndText` but scrubbing is not asserted)*  
**File:** `internal/cli/research.go:234–249`  
`bindThoughtDisplay` passes `ev.Delta.Text` directly to `json.Marshal` without calling `secret.Scrub`. The API key itself will not appear in thought summaries, but `--input` files may contain credentials (service-account JSON, database passwords, SSH keys) that the Gemini API may quote in thought deltas; these would be written to stderr unscrubbed. Same structural gap as C1-SEC-6 (cycle-1 remediated), but for thought content rather than error messages.  
**Action:** `Text: secret.Scrub(text)` in `bindThoughtDisplay`. One line. Add an assertion to `TestResearchEndToEndText` (or a new test) that a scrubber pattern embedded in a thought delta is stripped before the delta reaches stderr.  
**CWE:** CWE-209

---

### Observability

**C2-SPEC-1 — `SpanPlan` defined but never emitted; plan phase invisible in OTel traces**  
**File:** `internal/trace/names.go:9` · `internal/cli/research.go` (`reviewPlan`)  
`trace.SpanPlan` is defined but used nowhere (single occurrence in `names.go`; confirmed by spec reviewer grep). When `--plan` is used, the collaborative-planning phase — which may consume several minutes and multiple API round-trips — produces no child span under the root `research` span. Backends see a gap between `run_started` and `start`.  
**Action:** Thread the tracer through `reviewPlan()` and wrap each `planner.Session.Run()` call in a `SpanPlan` child span, recording the plan's `InteractionID` as a span attribute.

**C2-CODE-4 — Streaming-to-polling degradation is silent in the event stream**  
**File:** `internal/researcher/gemini/gemini.go:372–384`  
When `awaitStream` exhausts its failure budget, `Await` falls back to `awaitPoll` with no transport event announcing the change. The only post-hoc signal is `reconnect_count: 4` in the terminal `cost_summary` event. An operator watching the NDJSON stream during a long run has no in-flight signal to distinguish a connectivity failure from a deliberate `--quiet` run.  
**Action:** Emit a best-effort transport event (e.g., `KindDelta` with `"type":"stream_degraded"` or a new `KindStreamDegraded` kind) immediately before the `awaitPoll` fallback, consistent with the run core's best-effort transport contract.

---

### Config validation

**C2-CODE-3 — MCP server URLs not validated for scheme**  
**File:** `internal/config/config.go:163–168` · `internal/researcher/gemini/gemini.go:273–276`  
`config.Validate()` and `assembleTools()` both check only that MCP name and URL are non-empty. A `file:///local/server` or `javascript:` URL passes both and reaches the API wire type, producing a confusing API error instead of a local validation message.  
**Action:** In `config.Validate()`, for each MCP URL: `strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")`. Return an error if neither is true.

---

### gemini package defensive coding (same directory — fix together)

**C2-CODE-1 — `stream.go:backoff` missing `n > 30` guard before shift**  
**File:** `internal/researcher/gemini/stream.go:199`  
`r.streamCfg.baseDelay << (n - 1)` has no pre-shift cap on `n` (contrast `interactions/client.go:backoffDelay` which guards `attempt > 30`). With `maxStreamFailures = 4`, overflow is impossible in practice; but if `maxStreamFailures` is ever increased, the `d <= 0` guard is the only safety net.  
**Action:** Add `if n > 30 { n = 30 }` before the shift, matching `client.go`'s pattern.

**C2-CODE-2 — Plan round interaction ID lost on context cancellation**  
**File:** `internal/researcher/gemini/planner.go:131`  
`Planner.round()` creates a planning interaction (`in.ID`), then polls. If the context is cancelled during polling, `in.ID` is never emitted, logged, or returned — the plan was paid for but cannot be recovered via `chiron get`. Research interaction IDs are emitted immediately via `KindInteractionCreated`; plan rounds have no equivalent recovery handle.  
**Action:** Return the plan's `in.ID` alongside any polling error from `round()`, and have `Session.Run` emit it to stderr (or log it at warn level) on failure, consistent with the research pattern.

---

### `run.go`

**C2-CONS-4 — `SetAttr("resumed", "true")` passes a string, not a bool**  
**File:** `internal/run/run.go:174`  
`root.SetAttr("resumed", "true")` produces a string OTel attribute rather than a bool. The OTel backend records the wrong type for this attribute; it diverges from Go idiom and from how other scalar span attributes are set.  
**Action:** `root.SetAttr("resumed", true)`. One character change.

---

### SSE test infrastructure (related findings — same pass)

**C2-TEST-14 — SSE test helpers in `research_test.go` missing `Flush()` call**  
*(code-reviewer Low)*  
**File:** `internal/cli/research_test.go:34–38`  
The `sseWrite` helper in `gemini/stream_test.go` calls `fl.Flush()` after writing all SSE frames. The streaming branches in `newInteractionsServer` and `sseStatus` in `research_test.go` do not. This works now because the handler returns immediately. If a future test adds a multi-frame sequence without an immediate handler return, the missing flush will cause the scanner to block.  
**Action:** Add `if fl, ok := w.(http.Flusher); ok { fl.Flush() }` after each SSE `w.Write` call in `newInteractionsServer` and `sseStatus`, consistent with `sseWrite`.

**C2-CONS-6 — `interactions/stream_test.go` uses raw SSE literals instead of a shared helper**  
*(consistency-reviewer Low)*  
**File:** `internal/interactions/stream_test.go`  
`gemini/stream_test.go` defines `sseWrite(t, w, frames...)` and uses it throughout. `interactions/stream_test.go` embeds SSE frames as raw multi-line string literals via `io.WriteString`. The SSE frame format exists in two independent representations — a maintenance discrepancy.  
**Action:** No immediate code change required. Add a note to `AGENTS.md`: "For new SSE tests in any package, use a `sseWrite`-style helper (`t.Helper`; calls `fl.Flush()`) rather than raw `io.WriteString` literals." Apply to new tests going forward; do not rewrite existing interactions tests unless already touching them.

---

### Remaining test gaps

**C2-TEST-11 — `error.go:snippet` truncation path untested**  
**File:** `internal/interactions/error.go:72`  
`snippet` truncates error bodies over 200 bytes; only the short path is tested. Extend `TestNonJSONErrorBody` with a body > 200 bytes; assert the resulting `APIError.Message` does not exceed the limit and does not contain tail-only content.

**C2-TEST-12 — `config.Duration.MarshalYAML` at 0%**  
**File:** `internal/config/duration.go:33`  
`research-config` emits JSON, so `MarshalYAML` is not on the critical path in v1. Add a unit test that marshals a `Duration` to YAML and asserts the string form (e.g., `"30m0s"`), not a nanosecond integer. Prevents a silent regression if `MarshalYAML` is ever on a production path.

**C2-TEST-13 — `WithHTTPClient` and `WithRequestTimeout` functional options at 0%**  
**File:** `internal/interactions/client.go:61,67`  
Neither option is directly verified. For `WithRequestTimeout`, a test that passes a very short deadline and confirms the request is cancelled (via `context.DeadlineExceeded`) would pin a contract that matters for 60-minute research runs. Low priority given that both options are exercised transitively; schedule with other interactions-package clean-up.

---

### Code quality and style

**C2-CONS-5 — `decodeJSONBody` helper defined in `stream_test.go` but not used in `gemini_test.go`**  
**File:** `internal/researcher/gemini/gemini_test.go:51–53` · `gemini/stream_test.go:392`  
`decodeJSONBody` is available to `gemini_test.go` (same package) but inline decoding is used there instead. Replace inline `json.NewDecoder(req.Body).Decode(...)` in `TestStartBuildsCreateRequest`, `TestTierMapping`, `TestCustomTemplate`, and `TestInputParts` with calls to `decodeJSONBody`. Pure clean-up; no behaviour change.

**C2-CONS-7 — Double package-doc comment in `formatter/markdown.go`**  
**File:** `internal/formatter/markdown.go`  
A block comment immediately before `package formatter` acts as a second package-doc; `formatter.go` already carries the canonical package doc. `go doc` shows both.  
**Action:** Move the comment to precede the `Markdown` type declaration (or `NewMarkdown`) where it belongs as a type-doc.

---

## Verified Closed — cycle-1 finding record

All cycle-1 findings are confirmed closed at HEAD `90fe191`. Verification source noted for each.

| ID | Description | Severity | Verified by |
|---|---|---|---|
| C1-SEC-1 | `CHIRON_GEMINI_BASE_URL` HTTPS enforcement | High | code-reviewer, security-reviewer |
| C1-CODE-1 | Unbounded `os.ReadFile` for `--input` files | High | code-reviewer |
| C1-CODE-2 | Researcher single-use race (`inFlight` CAS) | High | code-reviewer, security-reviewer |
| C1-SSE-1 | SSE per-event accumulation unbounded | Medium | code-reviewer, security-reviewer |
| C1-SEC-2 | API key forwarded on cross-host redirects | Medium | code-reviewer, security-reviewer |
| C1-CODE-3 | Unescaped `]`/`)` in Markdown citation links | Medium | code-reviewer |
| C1-CODE-4 | `stdinIsPiped` false-positive for non-`*os.File` | Medium | code-reviewer, security-reviewer, spec-reviewer |
| C1-M5-1 | `cfg.Stream` not wired to `ThinkingSummaries` | Medium (M5-delegated) | spec-reviewer, code-reviewer |
| C1-M5-2 | `ReconnectCount` always zero | Medium (M5-delegated) | spec-reviewer, code-reviewer |
| C1-SEC-4 | Report and asset files world-readable (`0o644`) | Low | code-reviewer |
| C1-CODE-6 | `MetricFailures` deferred-path count not asserted | Low | code-reviewer |
| C1-SEC-3 | CI actions pinned to mutable tags | Low | security-reviewer |
| C1-SEC-5 | Secret file permission not warned | Low | security-reviewer |
| C1-SEC-6 | API errors reach stderr without scrubbing | Low | security-reviewer |
| C1-CODE-5 | `backoffDelay` overflow comment missing | Low | code-reviewer |
| C1-CODE-7 | `crypto/rand.Read` go.mod version check | Low | code-reviewer |
| C1-CODE-8 + C1-SPEC-1 | `toDomain` follow-up model; `OutputThoughtSummary` | Low | code-reviewer, spec-reviewer |
| C1-SPEC-2 | `otel/trace` undeclared in DECISIONS.md | Low | spec-reviewer |
| C1-SEC-7 | OTel trace endpoint sensitivity | Info | security-reviewer |
| C1-SPEC-3 | M4-scope stubs inert | Info | spec-reviewer |

**Notes:**
- C1-SEC-1 remediation includes a loopback HTTP exemption (`localhost`, `127.0.0.0/8`, `::1`) that was not in the original brief fix. Security reviewer confirms the deviation is sound: DNS-rebinding is impossible because the validation compares the hostname string directly without calling a resolver. `TestBaseURLOverrideRejectedBeforeAnyRequest` covers all six original attack cases.
- C1-CODE-8 (toDomain model mapping) is closed by the full implementation of `chiron follow-up` in M4; `TestFollowUpChainsModelInteraction` pins the wire shape.
- C1-SPEC-1 (`OutputThoughtSummary` constant) is closed by `thoughtTexts()` in `toDomain()` mapping thought content parts in M5.
- C1-SPEC-3 (M4 stubs inert) is closed by full M4 implementation.
