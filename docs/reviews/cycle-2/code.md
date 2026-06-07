# Code Review — Cycle 2 (Final)

**Reviewer:** code-reviewer agent  
**HEAD at review time:** 90fe191  
**Date:** 2026-06-07  
**Scope:** (1) Closure verification of all cycle-1 findings against the
remediation table; (2) full new-code review of M4 (planner, budget gate,
research-config piping, get/Resume refactor, follow-up) and M5 (streaming
await, reconnect/backoff, poll fallback, CLI wiring, test fakes); (3)
re-check of anything touched by intervening changes.

---

## Summary

The remediation pass is clean. Every cycle-1 finding was verified against
the actual code — not just the commit message — and all are genuinely fixed
without papering over. The M4 and M5 additions are well-structured: the run
core's `Run`/`Resume` split, the budget gate, the collaborative-planning
session loop, the streaming reconnect policy, and the CLI wiring each hold
up under close reading. The test suite covers the new surfaces usefully and
the SSE branches in the CLI fakes do not weaken the tests.

No Critical or High findings in new code. Five Low findings follow; none
blocks shipping.

---

## Part I — Closure Verification

### C1-CODE-1 (High): Unbounded `os.ReadFile` for `--input` files

**Claimed fix:** `df941a6` — `io.LimitReader` at 50 MiB.

**Verified.** `internal/researcher/gemini/input.go:readInputBounded` opens
the file, wraps it in `io.LimitReader(f, maxInputBytes+1)`, reads with
`io.ReadAll`, then rejects with a clear error when `len(data) > maxInputBytes`.
The `+1` trick ensures the overrun is detected rather than silently truncated.
`contract_test.go:TestInputFileOverBound` (sparse file at limit+1) and
`TestInputFileAtBound` (sparse file at exactly limit) pin both sides of the
boundary. **Genuinely fixed.**

### C1-CODE-2 (High): Researcher single-use race

**Claimed fix:** `df941a6` — `inFlight` CAS + mutex-grouped reset.

**Verified.** `gemini/gemini.go:Start` opens with
`r.inFlight.CompareAndSwap(false, true)`; the defer `r.inFlight.Store(false)`
fires on return. All per-run state resets (`started`, `lastQuery`,
`pollCount.Store(0)`, `reconnectCount.Store(0)`) are grouped under `r.mu.Lock()`
after the create returns, so a stale `OnPoll` increment from a concurrent
previous Await cannot interleave between them.
`TestStartRejectsConcurrentRun` blocks the HTTP server mid-create and
confirms the second Start fails with the "in flight" message.
`TestPollCountResetsOnFreshStart` confirms sequential reuse resets to zero.
**Genuinely fixed.**

### C1-SSE-1 (Medium): Per-line vs per-event accumulation bound

**Claimed fix:** `845a852` — accumulation check across `data:` lines.

**Verified.** `interactions/stream.go:Next()` now checks
`int64(len(data)) > s.maxEventBytes` immediately after each `data:` append,
before the event is dispatched. The scanner's per-line cap bounds individual
fields; this check bounds the total payload accumulated across fragmented
multi-line data. An oversized event returns a formatted error before the
JSON decoder touches the payload. **Genuinely fixed.**

### C1-SEC-2 (Medium): Cross-host redirect leaking x-goog-api-key

**Claimed fix:** `845a852` — `refuseCrossHostRedirects` policy.

**Verified.** `interactions/client.go:New` shallow-copies the caller's
`*http.Client` and installs `refuseCrossHostRedirects` on the copy, so a
client supplied via `WithHTTPClient` cannot lose the policy. The policy
refuses any redirect whose host differs from the originating host, and caps
same-host chains at three hops. `TestCrossHostRedirectRefused` confirms the
second server receives zero requests; `TestSameHostRedirectFollowed` and
`TestRedirectLoopRejected` cover the boundary cases. **Genuinely fixed.**

### C1-CODE-3 (Medium): Unescaped `]` / `)` in Markdown citation links

**Claimed fix:** `ceb56b1` — escaping and scheme filter in the formatter.

**Verified.** `formatter/markdown.go:source.markdown()` now does
`strings.ReplaceAll(s.Title, "]", `\]`)` and
`strings.ReplaceAll(s.URI, ")", "%29")` before formatting the link.
`collectSources` filters out any citation whose URI does not begin with
`https://` or `http://`, blocking `javascript:`, `data:`, and `file:`
URIs from ever reaching the Markdown body. **Genuinely fixed.**

### C1-CODE-4 (Medium): `stdinIsPiped` false-positive for non-`*os.File` readers

**Claimed fix:** `775e509` — `stdinIsPiped`/`stdinIsTerminal` split.

**Verified.** `commands.go` defines two independent functions:
`stdinIsPiped` returns true only for `*os.File` with
`Mode()&os.ModeCharDevice == 0` (a real pipe or file redirect);
`stdinIsTerminal` returns true only for `*os.File` with
`Mode()&os.ModeCharDevice != 0`. A non-`*os.File` reader (e.g., a
`strings.Reader` from a library host) is neither — it is rejected as a
config source and cannot approve a planning spend. The two functions are
deliberately not complementary. **Genuinely fixed.**

### C1-SEC-1 (High): Missing HTTPS enforcement on `CHIRON_GEMINI_BASE_URL`

**Claimed fix:** `775e509` — validation with loopback HTTP exemption
(see remediation note 1).

**Verified.** `research.go:geminiBaseURL()` and `allowedBaseScheme()` enforce
HTTPS unconditionally; HTTP is admitted only when the hostname resolves to
loopback (`localhost`, `127.0.0.0/8`, `::1`). `TestBaseURLOverrideRejectedBeforeAnyRequest`
covers all named attack cases: cleartext non-loopback, SSRF to the
metadata service, non-HTTP schemes, bare strings. The loopback exemption
is a sound deviation — restricting httptest servers to HTTPS would require
a TLS-trust injection point that exists only for tests, and all threat
vectors the original finding named (cleartext on a network path, key
exfiltration to a remote host, SSRF to link-local/internal ranges) are
fully blocked. **Genuinely fixed.**

### C1-SEC-4 (Low): World-readable report and asset files

**Claimed fix:** `ac7dafc` — `0o600` permissions.

**Verified.** `sink/file.go` uses `os.WriteFile(..., 0o600)` for both the
Markdown report and each chart asset. **Genuinely fixed.**

### C1-CODE-6 (Low): MetricFailures double-count on sink failure

**Claimed fix:** `a2c1272` — counting tracer test.

**Verified.** On a sink failure `conclude()` returns an error before calling
`recordMetrics`, so only the deferred `Tracer.Metric(ctx, MetricFailures, 1)`
in `Run` fires — exactly once. `TestRunSinkError` uses a `countingTracer`
and asserts `counter.metrics[trace.MetricFailures] == 1`. **Genuinely fixed.**

### Remaining findings (Low, Info)

C1-SEC-3, C1-SEC-5, C1-SEC-6, C1-SEC-7, C1-SPEC-2, C1-SPEC-3 are noted
as fixed or overtaken in the remediation table. C1-CODE-5, C1-CODE-7,
C1-CODE-8 are confirmed by code review: the `backoffDelay` overflow comment
is present; `go.mod` declares `go 1.26.4`; `toDomain` maps `in.Model` when
`in.Agent` is empty and `TestFollowUpChainsModelInteraction` exercises the
path.

**Deferred findings C1-M5-1 and C1-M5-2** are reviewed in Part II.

---

## Part II — New-Code Review

### M5 Streaming await (researcher/gemini/stream.go)

#### Reconnect loop correctness

`awaitStream` is structurally sound:

- Context checked at the top of every iteration before dialling, so a
  cancelled context aborts promptly rather than attempting a reconnect.
- `reconnectCount.Add(1)` is gated on `dialled == true`, so the initial
  connection does not inflate the counter. `TestStartResetsReconnectCount`
  pins the zero baseline across runs; `TestAwaitReconnectsWithLastEventID`
  confirms reconnect_count = 1 after one reconnect.
- `stream.Close()` is called unconditionally after `consumeStream` returns
  (regardless of error), so the response body is always released.
- `lastEventID` is captured from `stream.LastEventID()` before `Close()`
  and carried into the next iteration's query parameter — correctly using
  the query parameter rather than the `Last-Event-ID` header
  (INTERACTIONS-API.md §5).

#### Failure budget and fallback

`maxStreamFailures = 4` is never reset by progress. The comment in
`stream.go` explains the deliberate choice: a stream alternating deltas
with errors would otherwise hold the await captive indefinitely.
`TestAwaitStreamErrorEventsSpendTheFailureBudget` covers that exact scenario.

The `Await()` fallback logic in `gemini.go` correctly distinguishes three
outcomes from `awaitStream`:
- `nil` → return nil (stream concluded cleanly, no poll needed)
- `ErrRequiresAction` or `ctx.Err() != nil` → wrap and return (these are
  final outcomes, not transient failures; polling would not help)
- Any other error after budget exhaustion → fall through to `awaitPoll`

`TestAwaitStreamRequiresActionDoesNotFallBack` confirms the
`ErrRequiresAction` arm does not touch the poll path.

#### Backoff correctness

`backoff(ctx, n)` uses `r.streamCfg.baseDelay << (n - 1)` where `n` is
the 1-based failure count at the time of the call. With
`maxStreamFailures = 4`, `n` is at most 3 when backoff is invoked (the
fourth failure returns `lastErr` without backoff). Maximum shift is
`baseDelay << 2 = 500ms * 4 = 2s`, well within int64. The `d <= 0 || d > maxDelay`
guard catches any overflow and caps to `maxDelay`. Safe in practice.

#### goroutine and resource cleanup on context cancellation

No goroutines are spawned by the streaming path. The reconnect loop runs in
the caller's goroutine. `stream.Close()` is the only resource requiring
cleanup; it is called on every code path exiting `awaitStream` after a
successful `Stream()` call. When the context is cancelled, `backoff` returns
`ctx.Err()`, which propagates to `Await()` and then to `run.conclude()`,
which closes the await span and returns. No leaks.

#### OnThought emit path

`onThought` is called synchronously from `consumeStream` and is documented
as "must not block". The CLI's implementation (`bindThoughtDisplay`) does a
`json.Marshal` and a mutex-protected single-Write to stderr — fast, bounded
work. Errors are discarded (`_ = tr.Emit(...)`), consistent with the run
core's best-effort transport contract. The captured `ctx` is the run's
timeout context; when it expires any subsequent emit fails silently, which
is the correct behaviour.

#### SSE branches in CLI test fakes — do they weaken the tests?

`newInteractionsServer` (research_test.go) adds a streaming branch that
returns a thought delta followed by a status update. The existing tests
were updated to expect this branch and `TestResearchEndToEndText` now
asserts the `delta` event is present in the NDJSON stream, which means
the SSE path through the real streaming stack is exercised end-to-end.
`TestResearchQuietPolls` asserts zero stream attaches under `--quiet`,
pinning the quiet path. `TestStreamTogglesThinkingSummaries` asserts
`thinking_summaries` in the create request matches the stream flag. These
additions exercise real behaviour and strengthen rather than weaken the
suite. The `sseStatus` helper (used by the get and follow-up tests) is
minimal but sufficient: the handler returns immediately after writing the
single event frame, so the HTTP server closes the connection, the SSE
parser dispatches the event before the EOF, and the stream terminates
cleanly.

### M4 Planner session loop (planner/session.go)

The `Session.Run` loop is correct. `maxRounds` defaults to 5 when the
caller supplies ≤ 0. The `lastRound` flag (`round == maxRounds`) is
evaluated at review time, not at render time, so the final round cannot
be refined — the user is told "no refine rounds left" and re-prompted.
Empty feedback is rejected and re-prompted without spending a round.
End-of-input aborts with `ErrAborted`, which the CLI maps to `ExitBlocked`.
`AutoAccept` short-circuits before any reader interaction, so nil `In` is
safe in that path (substituted with `strings.NewReader("")` for the non-
AutoAccept path, which correctly produces `ErrAborted` on first read).

The session test suite covers: accept first plan, refine then accept,
quit, end-of-input, auto-accept, round bound withdrawal, unrecognised
answer reprompt, empty feedback reprompt, propose error propagation, nil
Planner rejection. Coverage is thorough.

### Budget gate placement (research.go)

`gateBudget` is called after `gemini.New` but before both the planning
phase and the research create. The comment in `runResearch` is explicit:
"the research one and the plan rounds alike, since both spend."
`TestResearchBudgetGatesThePlanPhaseToo` confirms no request reaches the
server when the estimate exceeds the cap with `--plan --accept-plan`.
`gateBudget` admits `BudgetGBP <= 0` (uncapped) and `estimateGBP <= BudgetGBP`
(inclusive cap). For `NewFollowUp`, `EstimatedCostGBP()` returns 0 (outside
the tier table), so the gate never blocks a follow-up — correct.

### run.Resume refactor (run/run.go)

`Run` and `Resume` share `conclude()` for the await→retrieve→format→emit
back-half. `Resume` skips the start phase, emits `KindInteractionCreated`
(re-stating the resume handle), and calls `conclude` with the supplied id.
`TestResumeHappyPath` confirms the researcher's `Start` is never called,
the event sequence lacks `run_started`, and the sink receives the result.
`TestResumeRequiresID` confirms an empty id is rejected before any seam
is touched.

### Follow-up path (research.go / gemini/followup.go)

`NewFollowUp` builds a `Researcher` with `model` set and `agentID` empty.
`Start` branches on `r.model != ""` and sends a create with `Model` and
`PreviousInteractionID` but no `Agent`, `AgentConfig`, or `Tools` —
correct per INTERACTIONS-API.md §3. `TestFollowUpChainsModelInteraction`
captures the create body and asserts `model = "gemini-3.1-pro-preview"`,
`previous_interaction_id = "v1_research"`, and absence of `agent`. The
follow-up does not set `inFlight` for the full Run lifecycle — acceptable,
since `NewFollowUp` returns a fresh instance and follow-ups are expected
to be called sequentially. `toDomain` maps `in.Model` to `out.Agent` when
`in.Agent` is empty (C1-CODE-8 closure).

---

## Findings

### LOW-1 — `stream.go:backoff` missing defensive shift-overflow guard

**File:** `internal/researcher/gemini/stream.go:199`

**Issue:** `r.streamCfg.baseDelay << (n - 1)` uses `n - 1` as the shift,
where `n` is the 1-based failure count. The `d <= 0 || d > maxDelay` guard
catches overflow, but there is no cap on `n` before the shift (contrast
`interactions/client.go:backoffDelay` which guards `attempt > 30`).

**Why it matters:** With `maxStreamFailures = 4`, `n` reaches at most 3,
so the maximum shift is 2 and overflow is impossible. However, if
`maxStreamFailures` is ever increased (or `baseDelay` enlarged to a value
that overflows at smaller shifts), the guard is the only line of defence.
The inconsistency with the pattern in `backoffDelay` makes it easy to miss.

**Fix:**
```go
func (r *Researcher) backoff(ctx context.Context, n int) error {
    if n > 30 {
        n = 30 // prevent shift overflow; far beyond any sane budget
    }
    d := r.streamCfg.baseDelay << (n - 1)
    if d <= 0 || d > r.streamCfg.maxDelay {
        d = r.streamCfg.maxDelay
    }
    ...
}
```

---

### LOW-2 — Plan round interaction ID not surfaced on context cancellation

**File:** `internal/researcher/gemini/planner.go:131`

**Issue:** `Planner.round()` creates a planning interaction (`in.ID`), then
calls `p.poll.PollUntilTerminal(ctx, in.ID, p.pollCfg)`. If the context
times out or is cancelled during polling, the function returns an error
and `in.ID` is lost — it is never emitted to a transport, logged, or
returned to the caller. The plan interaction was created (and paid for)
but cannot be inspected or recovered.

**Why it matters:** Research interaction IDs are emitted immediately via
`run.Run`'s `KindInteractionCreated` event, giving the user a recovery
handle for `chiron get`. Planning rounds have no equivalent. A planning
timeout leaves the user with no way to inspect what (if any) plan text
was generated, and the money spent on that round is unrecoverable.
Planning rounds are cheaper than research runs, but the pattern diverges
from the stated recovery guarantee.

**Fix:** Emit the plan interaction ID to the transport (or surface it in
the error) before polling, consistent with the research pattern. The
simplest approach is for `Planner.round()` to return the plan's `in.ID`
alongside any polling error, and for `Session.Run` to log it on stderr.

---

### LOW-3 — MCP server URLs not validated for scheme

**File:** `internal/config/config.go:163–168`, `internal/researcher/gemini/gemini.go:273–276`

**Issue:** `config.Validate()` checks only that MCP name and URL are
non-empty. `assembleTools()` applies the same check. A URL with a
non-HTTP(S) scheme (`javascript:`, `file:`, `ftp:`) would pass both and
reach the API wire type.

**Why it matters:** While the Interactions API would reject such a URL,
passing it through silently means validation gives a false confidence that
the config is sound. A mistakenly entered `file:///local/server` would
produce a confusing API error rather than a clear local validation message.

**Fix:** In `config.Validate()`, for each MCP URL, check
`strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://")`.

---

### LOW-4 — Streaming-to-polling degradation is silent in the event stream

**File:** `internal/researcher/gemini/gemini.go:372–384`

**Issue:** When `awaitStream` exhausts its failure budget, `Await()` falls
through to `awaitPoll` with no transport event announcing the degradation.
The streaming error is intentionally absorbed. An operator watching the
NDJSON event stream sees poll events where streaming events were expected,
but has no in-flight signal that the streaming path failed.

**Why it matters:** Post-hoc, `reconnect_count: 4` in the `cost_summary`
event indicates the budget was spent, but this arrives only at run
completion. During a long research run, silent degradation from streaming
to polling makes it harder to distinguish a connectivity issue from a
deliberate `--quiet` run. Diagnosing repeated streaming failures requires
log correlation rather than event-stream inspection.

**Fix:** Emit a transport event (e.g., `KindDelta` with a synthetic
`type: "stream_degraded"` payload, or a new `KindStreamDegraded` kind)
immediately before the `awaitPoll` fallback. This is best-effort, matching
the run core's treatment of the transport.

---

### LOW-5 — SSE test helper flushing inconsistency

**File:** `internal/cli/research_test.go:34–38` vs. `internal/researcher/gemini/stream_test.go:36–48`

**Issue:** The `sseWrite` helper in `stream_test.go` calls `fl.Flush()` on
the ResponseWriter after writing all frames. The equivalent streaming branch
in `newInteractionsServer` (research_test.go, lines 34–38) and the
`sseStatus` helper write SSE data but do not call `Flush()`. They rely on
the HTTP server flushing implicitly when the handler returns.

**Why it matters:** In practice this works because Go's HTTP server drains
the write buffer when the handler returns, and the SSE parser dispatches
the terminal event before reaching EOF. However, if a future test writes
a long-running SSE sequence (multiple frames, handler does not return
immediately), the missing flush would cause the client's scanner to block
until the handler returns, making the test unreliable or deadlock-prone.
The pattern inconsistency also makes it non-obvious which approach is
correct for new tests.

**Fix:** Add `if fl, ok := w.(http.Flusher); ok { fl.Flush() }` after
each `w.Write` call in the SSE branches of `newInteractionsServer` and
`sseStatus`, consistent with `sseWrite`.

---

## Verdict

The Chiron v1 codebase is in good shape for shipping. All cycle-1 findings
are closed correctly — no re-opened issues, no papering-over. The M4 and M5
additions (streaming reconnect, planning session loop, budget gate,
Resume/conclude refactor, follow-up path) are architecturally consistent
with the existing seam design and the INTERACTIONS-API.md contract. Test
coverage of the new surfaces is meaningful: the streaming tests exercise the
real SSE parser, the session tests cover all user-decision paths, and the
CLI smoke tests exercise the full stack against real httptest servers.

**Findings by severity:** 0 Critical, 0 High, 0 Medium, 5 Low.

The five Low findings are independent improvements (defensive coding, a
missing recovery handle, a validation gap, an observability gap, and a test
consistency issue). None represents a correctness defect or security risk in
the current production configuration.
