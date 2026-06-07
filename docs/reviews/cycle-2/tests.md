# Test Coverage Audit — Chiron Cycle 2

**Auditor:** test-coverage-auditor  
**Commit:** `90fe191` (HEAD, post-M5 + cycle-1 remediation)  
**Date:** 2026-06-07  
**Run command:** `go test -count=1 -coverprofile=cover.out -race ./...`  
**Race detector:** applied to all packages (see per-package notes below)

---

## Summary

All 15 test packages pass cleanly with `-race` enabled. Overall statement
coverage is **86.1%**. The money-critical and security-critical contracts
are largely well-pinned; two functional gaps in the streaming completion
path have no tests at all. Configuration-layer coverage has meaningful
holes that would not catch regressions in flag handling or duration
round-trips.

### Per-package coverage table

| Package | Coverage | Criticality | Target |
|---|---|---|---|
| `cmd/chiron` | 0.0% | Low (entrypoint only) | — |
| `internal/cli` | 79.3% | High (composition root, exit codes, money gate) | ≥ 85% |
| `internal/config` | 74.8% | Medium (flag binding, duration serde) | ≥ 80% |
| `internal/formatter` | 91.4% | Medium | ≥ 80% ✓ |
| `internal/interactions` | 91.4% | High (wire client, retry, SSE) | ≥ 85% ✓ |
| `internal/memory` | 0.0% | Low (no-op only, intentional) | — |
| `internal/planner` | 96.4% | High (spend-gated plan loop) | ≥ 85% ✓ |
| `internal/researcher` | — | — (interface only) | — |
| `internal/researcher/fleet` | 100% | Low (stub) | — |
| `internal/researcher/gemini` | 83.7% | Critical (all spend paths) | ≥ 90% |
| `internal/run` | 94.4% | High (core loop) | ≥ 85% ✓ |
| `internal/secret` | 97.1% | Critical (key hygiene) | ≥ 90% ✓ |
| `internal/sink` | 80.5% | Medium | ≥ 75% ✓ |
| `internal/trace` | 86.5% | Medium | ≥ 75% ✓ |
| `internal/transport` | 90.0% | Medium | ≥ 75% ✓ |
| `internal/types` | 100% | High (status enum) | ≥ 85% ✓ |

---

## DECISIONS.md contract coverage

Each behavioural contract recorded in `docs/DECISIONS.md` that carries
"a test pins" language was spot-checked.

| Contract | Test pinning it | Status |
|---|---|---|
| Create is never retried (£1–7 risk) | `TestCreateIsNeverRetried` | ✓ pinned |
| Plan-phase create never retried | `TestPlannerCreateIsNeverRetried` | ✓ pinned |
| background requires store | `TestCreateValidation` | ✓ pinned |
| Budget gate before any create | `TestBudgetGateBlocksBeforeAnyCreate` | ✓ pinned |
| Budget gate covers the plan phase | `TestResearchBudgetGatesThePlanPhaseToo` | ✓ pinned |
| Plan round bound (DefaultMaxRounds 5) | `TestSessionRoundBoundWithdrawsRefine` | ✓ pinned |
| requires_action typed error (poll) | `TestPollRequiresActionReturnsTypedError` | ✓ pinned |
| requires_action typed error (stream) | `TestAwaitStreamRequiresActionDoesNotFallBack` | ✓ pinned |
| requires_action never falls back to poll | `TestAwaitStreamRequiresActionDoesNotFallBack` | ✓ pinned |
| SSE resume via `?last_event_id=` query param | `TestAwaitReconnectsWithLastEventID` | ✓ pinned |
| Failure budget of 4, never reset by progress | `TestAwaitStreamErrorEventsSpendTheFailureBudget` | ✓ pinned |
| Poll fallback when failure budget spent | `TestAwaitFallsBackToPollingWhenStreamingRepeatedlyFails` | ✓ pinned |
| Base-URL validated before any client | `TestBaseURLOverrideRejectedBeforeAnyRequest` | ✓ pinned |
| Cross-host redirect refused | `TestCrossHostRedirectRefused` | ✓ pinned |
| SSE per-event accumulation bound | `TestStreamEventDataAccumulationBounded` | ✓ pinned |
| 50 MiB input file bound | `TestInputFileOverBound` / `TestInputFileAtBound` | ✓ pinned |
| NDJSON events on stderr, not stdout | `TestResearchEndToEndText` (checks both streams) | ✓ pinned |
| Unknown stdin is neither config nor terminal | `TestStdinIsPipedRejectsNonFileReaders` / `TestStdinIsTerminalRejectsNonFileReaders` | ✓ pinned |
| **`interaction.completed` event terminates stream await** | **no test** | **✗ UNPINNED** |
| Follow-up estimate is zero | `TestFollowUpEstimatesZero` | ✓ pinned |
| Resumed interaction records no tool set | `TestResultWithoutStartRecordsNoToolsAndDerivesEstimate` | ✓ pinned |
| Exit codes 0/1/2/3/4 | `TestResearchFailedExitCode`, `TestResearchBudgetExceededExitCode`, `TestBudgetGateBlocksBeforeAnyCreate`, `TestResearchRequiresActionExitCode` | ✓ pinned (all five) |
| Asset names deterministic (chart-N.ext) | `TestResearchEndToEndFileSink` (png only) | partial |

---

## Findings

### Critical — money/key behaviour unpinned

---

**C2-TEST-1 — `eventStatus` function at 0% coverage; `interaction.completed` stream path completely untested**

`researcher/gemini/stream.go:168` — `eventStatus`, `consumeStream` lines 145–153

The `consumeStream` function in the streaming await path has two event
branches: `EventInteractionStatusUpdate` and
`EventInteractionCreated`/`EventInteractionCompleted`. Every stream test
exercises only the `status_update` branch. The `completed` branch (and the
`created` branch, which handles re-attaching to an already-terminal
interaction) call `eventStatus` — which sits at **0.0%** coverage.

`eventStatus` (stream.go:168–176) decides whether a `requires_action`
status on a completion event is propagated or silently treated as a normal
completion. If the function were broken — for instance, always returning
`""` — a `requires_action` received via an `interaction.completed` event
would call `requiresActionErr("")`, return `nil`, and the run would
proceed to the Result fetch as if the interaction completed normally.
Because `requires_action` cannot legitimately occur for deep research
(DECISIONS.md, "requires_action returns a typed error"), this failure
mode would leave the user without the diagnostic and without the correct
exit code.

Additionally, the "re-attach to an already-complete stream" path (where
the first event is `interaction.created` with a terminal status, as
happens when `?last_event_id=` resumes past the end) is entirely
untested. This path returns early from the await loop, skips the poll
fallback, and calls `requiresActionErr(eventStatus(ev))` — all of which
are behaviourally correct but empirically unverified.

**Required tests:**

- `TestAwaitStreamInteractionCompletedEvent` in `gemini/stream_test.go`:
  server sends `interaction.completed` SSE event (with and without an
  embedded interaction); `Await` returns nil; `in.Usage.ReconnectCount`
  and `PollCount` are both 0.
- `TestAwaitStreamRequiresActionViaCompletedEvent`: server sends
  `interaction.completed` with embedded `{"status":"requires_action"}`;
  `errors.Is(err, interactions.ErrRequiresAction)` and no poll fallback.
- `TestAwaitStreamInteractionCreatedAlreadyTerminal`: server sends
  `interaction.created` with `{"status":"completed"}` as the very first
  event (simulates re-attaching to a finished run); `Await` returns nil
  immediately.

---

### High — important behaviour untested or weakly tested

---

**C2-TEST-2 — `statusDetail` covers only 1 of 5 failure statuses**

`researcher/gemini/gemini.go:523` — `statusDetail`, 42.9%

`TestFailedInteractionCarriesDetail` tests only `StatusFailed`. The
`cancelled`, `incomplete`, `budget_exceeded`, and `requires_action`
detail strings are never asserted. These strings travel into
`Interaction.StatusDetail`, which the formatter puts in the front-matter
`status_detail` field of every placeholder report. A swapped or empty
string would silently corrupt every report for those four outcomes.

**Required tests:**

Add four sub-cases to the existing `TestFailedInteractionCarriesDetail`
test (or extend it into a table test): server returns each of
`cancelled`, `incomplete`, `budget_exceeded`, and `requires_action`; for
each, assert that `in.StatusDetail` is non-empty and contains a string
specific to that status (not the `failed` string). Use the `Await` +
`Result` path, matching the existing test's structure in
`gemini/gemini_test.go`.

---

**C2-TEST-3 — `planner.round` has no test for plan interactions that complete without text**

`researcher/gemini/planner.go:112` — `round`, 80.0%

`TestPlannerFailedPlanRoundSurfaces` tests a plan poll that returns
`failed`. The code path where `PollUntilTerminal` returns a
`completed` interaction with no `FinalText()` — the guard at planner.go:138
— is not exercised. This is not just defensive code: the Interactions API
is beta and could return an empty model_output step. An empty plan text
means the session renders nothing, and the user's round budget is spent
with no feedback.

Also untested: `round` when `PollUntilTerminal` returns
`ErrRequiresAction` (a plan interaction entering `requires_action`). Per
`interactions.PollUntilTerminal`, this returns the typed error immediately.
The planner wraps it at planner.go:133 as "awaiting plan interaction",
losing the typed wrapper — meaning the `errors.Is(err, ErrRequiresAction)`
check in `concludeRun` would return false and the run would surface as an
infrastructure error rather than a research outcome.

**Required tests:**

- `TestPlannerRoundCompletesWithoutText` in `gemini/planner_test.go`:
  server returns `completed` status but `steps` with no text part;
  `Propose` returns a non-nil error containing "without plan text".
- `TestPlannerRoundRequiresAction`: server returns `requires_action` for
  the plan GET; verify `errors.Is(err, interactions.ErrRequiresAction)`.

---

**C2-TEST-4 — `loadBase` file-path branch (41.7%) has no test**

`internal/cli/commands.go:160` — `loadBase`, 41.7%

The three `loadBase` paths are:
1. `path == "-"` (stdin literal): not tested in isolation  
2. `path != ""` (explicit `--config <file>`): not tested at all  
3. `stdinIsPiped` (piped stdin): tested by `TestResearchConfigPipelineComposition`  
4. default (no config): tested everywhere else  

Path 2 — `--config <file>` — is how an operator would supply a
persistent base config to automate runs. If `os.Open` fails (permissions,
missing file) the error surfaces; if `Decode` fails (malformed YAML) it
surfaces. Neither is currently caught by a test.

**Required tests:**

- `TestLoadBaseExplicitFile` in `cli/levers_test.go` or
  `cli/stdin_test.go`: write a temp YAML file with `agent:
  deep-research-max`; run `research-config --config <path>`; assert
  the JSON output carries the override.
- `TestLoadBaseExplicitFileMissing`: `--config /no/such/file.yaml`;
  assert the command returns an error before any seam is built.

---

**C2-TEST-5 — `config.Duration.UnmarshalJSON` at 0%; JSON round-trip not verified end-to-end**

`internal/config/duration.go:24` — `UnmarshalJSON`, 0.0%

`TestJSONRoundTripThroughDecode` calls `EncodeJSON` followed by `Decode`.
`Decode` is a YAML decoder — `yaml.v3` accepts JSON as a subset of YAML
and marshals the duration value through `UnmarshalYAML`, not
`UnmarshalJSON`. The JSON round-trip test therefore never calls
`UnmarshalJSON`. If that function is broken (it currently delegates to
`set`, which has its own branch gaps at 45.5%), a `json.Unmarshal` call
on a config with a duration field would silently use the default
`time.Duration` JSON form (nanosecond integer) and fail or mis-parse.

**Required test:**

Add one case to `config_test.go`: `json.Unmarshal` a `ResearchConfig`
JSON document (as would be produced by `EncodeJSON`) directly into a
`ResearchConfig` struct using `encoding/json`; assert the `Timeout` field
survives as the expected `Duration`. This would catch any
`UnmarshalJSON`/`set` bugs in the JSON-native path.

---

### Medium — gaps that matter but do not directly risk money or keys

---

**C2-TEST-6 — `awaitStream` context cancellation path not tested**

`researcher/gemini/stream.go:66` — `awaitStream`, 86.2%

`TestPollContextCancellation` verifies that cancellation stops polling.
There is no equivalent test for the streaming await. Specifically: if the
context is cancelled while `awaitStream` is sleeping in `backoff` (after a
stream dial failure), the select in `backoff` should return `ctx.Err()`
and `awaitStream` should propagate it without falling back to polling.
This is the correct behaviour per DECISIONS.md ("context cancellation
never falls back"), but it is unverified.

**Required test:**

`TestAwaitStreamCancelledDuringBackoff` in `gemini/stream_test.go`:
server returns a non-SSE response (triggering a backoff); context is
cancelled; assert `errors.Is(err, context.Canceled)` and zero poll
fallback calls.

---

**C2-TEST-7 — `mimeFromName` at 33.3%; unrecognised file extension not tested**

`researcher/gemini/input.go:99` — `mimeFromName`, 33.3%

`TestMultimodalInputParts` covers only `.png` and `.pdf`. The function
also handles `.jpg`/`.jpeg`, `.gif`, `.txt`, and the fallback for unknown
extensions. The fallback (returning `"application/octet-stream"`) controls
what MIME type is sent for any grounding file whose extension the code
does not recognise. A wrong MIME type on a multimodal part would reach the
API and could cause the create to fail or the model to misinterpret the
grounding document.

**Required test:**

Extend `TestMultimodalInputParts` (or add a dedicated table test) for
`.jpg`, an unknown extension (`foo.bin`), and no extension. Assert the
expected MIME types in the decoded create body.

---

**C2-TEST-8 — `formatter/extensionFor` at 57.1%; JPEG and GIF asset extensions untested**

`internal/formatter/markdown.go:230` — `extensionFor`, 57.1%

The formatter golden tests exercise PNG and SVG chart assets. JPEG (two
MIME type aliases) and GIF images are never tested. An incorrect
extension in an asset name would produce a broken relative image link in
the Markdown report (e.g. `![Chart 1](chart-1.jpg)` pointing to a file
written as `.gif`).

**Required test:**

Add cases to the formatter test for `image/jpeg`, `image/gif`, and an
unknown MIME type; assert `extensionFor` returns `.jpg`, `.gif`, and
`.bin` respectively. This can be a direct unit test of the unexported
function from `formatter_test.go` or added to the golden fixture set.

---

**C2-TEST-9 — `ApplyFlags` at 58.1%; several flags lack unit-level coverage**

`internal/config/flags.go:42` — `ApplyFlags`, 58.1%

Flags exercised by `ApplyFlags` unit tests: `--agent`, `--visualise`,
`--quiet`, `--plan`, `--accept-plan`, `--model`, `--mcp`. Flags not
exercised at this level: `--budget`, `--timeout`, `--out`, `--template`,
`--input`, `--file-search`, `--output`. Most are exercised transitively
through CLI e2e tests (`TestBudgetGateBlocksBeforeAnyCreate`,
`TestResearchEndToEndFileSink`), but flag binding errors in `ApplyFlags`
would not be caught if pflag's type coercion silently uses the wrong
default.

**Required tests:**

Add table cases to `TestApplyFlagsOverlaysOnlySetFlags` covering:
`--budget 2.50`, `--timeout 45m`, `--output json`, `--out /tmp/x.md`.
Each should assert the corresponding field in the resulting config.

---

**C2-TEST-10 — `conclude` in `run/run.go` has no test for a nil Interaction from the researcher**

`internal/run/run.go:197` — `conclude`, guard at line 196–198, 94.4% overall

The guard `if in != nil && err == nil { err = errors.New("run: researcher returned no interaction") }`
would never be reached if `Await` returns an error, but is reachable if
`Await` returns nil and `Result` returns `nil, nil` — a programming error
in a researcher implementation. The condition is present but unverified; a
researcher stub that returns `nil, nil` from `Result` would currently pass
the run core silently or panic downstream.

**Required test:**

In `internal/run` package tests: a fake researcher whose `Result` returns
`nil, nil`; `run.Run` must return a non-nil error. This is a seam contract
test, not an edge case.

---

### Low — minor gaps; no direct behaviour risk

---

**C2-TEST-11 — `interactions/error.go:snippet` at 50%; truncated snippet path untested**

`internal/interactions/error.go:72` — `snippet`, 50%

`snippet` truncates the raw API error body when the body is over 200
bytes. Only the short path (body fits) is tested. A large error body
(e.g. a verbose HTML error page from a proxy) hitting this path would
still produce a correct `*APIError`, but the truncation has never been
verified.

**Required test:**

Extend `TestNonJSONErrorBody` with a response body of more than 200 bytes;
assert the resulting `APIError.Message` does not exceed the truncation
limit and does not contain the marker that would appear only in the
un-truncated tail.

---

**C2-TEST-12 — `config.Duration.MarshalYAML` at 0%**

`internal/config/duration.go:33` — `MarshalYAML`, 0%

`research-config` emits JSON (not YAML), so `MarshalYAML` is not on the
critical path in v1. If a caller serialises a `ResearchConfig` to YAML
directly, however, durations would silently marshal as nanosecond integers
rather than human-readable strings. Add a unit test to
`config/config_test.go` that marshals a `Duration` to YAML and asserts
the string form.

---

**C2-TEST-13 — `interactions.WithHTTPClient` and `WithRequestTimeout` at 0%**

`internal/interactions/client.go:61,67`

Functional options that are not exercised by any test. They are used
transitively by the gemini adapter (`newClients`), which passes a shared
`*http.Client`, but the options themselves are never directly verified to
apply their effect. Specifically: `WithRequestTimeout` cannot be
verified to apply the per-request deadline without a test that measures
request timing — an intrusive but valuable contract given that the
Interactions API tasks run for up to 60 minutes. A test that passes a
very short `WithRequestTimeout` and confirms the request is cancelled
would pin this.

---

## Fake / wire shape assessment

The test fake servers in `gemini/gemini_test.go`, `gemini/stream_test.go`,
`cli/research_test.go`, and `cli/levers_test.go` were compared against
`docs/INTERACTIONS-API.md` §3 (create shapes) and §4 (response shapes).

- `steps[]` / `content[]` / `annotations` shapes: correct throughout.
- `usage` block with `grounding_tool_count`: correct in `TestEndToEndCompletedMapsWireToDomain`.
- `Api-Revision` header pinning: verified by `TestCreateSendsWireRequest`.
- `background`/`store`/`stream` explicit (never `omitempty`): verified by `TestCreateRequestEmitsExplicitBooleans`.
- SSE event names (`step.delta`, `interaction.status_update`, `interaction.completed`): match §5 throughout.

One gap: no fake server exercises an `interaction.completed` event with an
embedded `interaction` object (the `"interaction":{...}` wrapper in §5's
table). The stream parser test `TestStreamEventSequence` does exercise this
shape, but the higher-level `gemini.consumeStream` never sees it. This is
the same gap as C2-TEST-1 viewed from the wire-shape angle.

## Race detector findings

No data races were reported. The `-race` flag was applied to the full
`./...` test run. Key concurrent code verified clean:
- `inFlight atomic.Bool` in the gemini adapter
  (`TestStartRejectsConcurrentRun`)
- `pollCount`/`reconnectCount atomic.Int64` under the mutex reset in
  `Start` (`TestPollCountResetsOnFreshStart`, `TestStartResetsReconnectCount`)
- `awaitStream` read loop concurrent with the reconnect counter increment
  (`TestAwaitFallsBackToPollingWhenStreamingRepeatedlyFails`)

## Flaky-prone constructs

None found. All timing-sensitive tests override delays to 1ms via
`fastPoll` (poll intervals) and `streamOpts` (reconnect backoffs). No
`time.Sleep` calls appear in any test file. `TestPollContextCancellation`
uses `time.Hour` as the poll interval to guarantee the select fires on
cancellation rather than on a poll tick — this is deliberate and
not flaky-prone.
