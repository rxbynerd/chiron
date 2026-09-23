# Cycle 3 consistency review — v2 research-agent PR

Reviewer: Consistency Reviewer (fit against the established v1 codebase only;
correctness, security and spec conformance are covered by sibling reviews).

Scope: `feat/v2-research-agent` (HEAD `d4ac9f2`) vs `origin/main` (`926d7bf`),
35 files, +7676/-18. Read in full: `internal/researcher/fleet/{model,search,fetch}`,
`internal/researcher/fleet/{worker.go,worker_action.go,worker_prompt.go,
worker_researcher.go}` and their tests, `internal/cli/research.go`,
`internal/config/config.go` + `flags.go`, `internal/trace/names.go`, and the
relevant `docs/DECISIONS.md` entries. Compared against `internal/interactions`,
`internal/researcher/gemini`, `internal/sink`, `internal/formatter`,
`internal/planner`, and `AGENTS.md`.

## Summary

The PR reads as native to the codebase. The three new HTTP clients
(`fleet/model`, `fleet/search`, `fleet/fetch`) faithfully replicate the
Gemini-adapter/`interactions` hardening idioms (bounded reads, cross-host
redirect refusal, capped timeouts, single-attempt paid POSTs, `Options` +
`New` construction, package-prefixed errors), and every accepted duplication
(`allowedEndpointScheme`, `refuseCrossHostRedirects`, `readBounded`) is
recorded in `docs/DECISIONS.md` exactly as `AGENTS.md` requires. The one
recurring problem is process, not architecture: internal roadmap labels
("Wave 4", "Wave 3/4", "Wave 6") have leaked from planning docs into code
comments — and in one case into a user-facing CLI error string — which the
project's own code-comment policy rules out. There is also a small, isolated
error-prefixing gap in `worker_action.go`.

## Findings

### C3-CONS-1 — "Wave N" planning labels in code comments and one user-facing error string
**Severity:** Medium
**File:** `internal/trace/names.go:18`; `internal/config/config.go:44-45,122,186`; `internal/cli/research.go:82,340-341,348`; `internal/researcher/fleet/worker.go:20,29,48,115`; `internal/researcher/fleet/worker_researcher.go:20`; `internal/researcher/fleet/worker_prompt.go:88`

`~/.claude/CLAUDE.md`'s code-comment policy bars "speculation about how a
future change might behave" and historical/session narrative from code
comments, reserving that for `DECISIONS.md`/planning docs. This PR introduces
13 comments across 6 files that name an internal wave number and describe
what that not-yet-written component will do, e.g.:

```go
// trace/names.go:16-18
// Worker span names for Chiron's in-process research agents
// (docs/V2-RESEARCH-AGENT §5). SpanWorker wraps one worker's bounded
// search -> read -> synthesise loop; Wave 4's lead reuses it for its
// per-worker delegate spans.
```
```go
// worker.go:20-25
// structured action per turn. It is factored so Wave 4's lead can reuse it
// per-brief: RunWorker takes a Brief (...) and shared WorkerDeps, and returns
// a Finding (...). The single-query --agent worker path (Worker, in
// worker_researcher.go) is one caller; the lead will be another, ...
```

This is a different pattern from the codebase's existing milestone tags —
`internal/cli/research.go:69` ("M2 awaits by polling; the M5 streaming
surface arrives with --stream support") and `internal/researcher/gemini/gemini.go:222`
("the M4 planning flow flips it per request") on `origin/main` — which tag
**already-implemented** behaviour for cross-reference, with no speculation
about a future consumer. The new comments instead narrate a component that
does not exist yet and guess at how it will use the code, which is exactly
the "speculation about how a future change might behave" the policy
excludes; `docs/V2-PLAN.md` §6 already owns this rationale.

One instance is worse than a comment: `internal/cli/research.go:348` puts the
label in the string a user sees:

```go
return fmt.Errorf("agent %q: not yet wired (v2 Wave 4 in progress)", agent)
```

The sibling precedent for "not implemented yet" is
`internal/researcher/fleet/fleet.go:22`, which reads as a product statement
with no internal tracking label: `"stirrup-fleet researcher is a v2 seam,
not implemented in v1"`. A `chiron research --agent fleet` user has no
context for "Wave 4" and the label will be stale the moment the plan
re-numbers a wave.

**Fix:** Rewrite each comment to state the present contract (e.g. "RunWorker
is deliberately decoupled from Worker so a future orchestrator can dispatch
it directly" or simply drop the forward-looking clause) and move the
roadmap-sequencing detail to `docs/V2-PLAN.md`/`DECISIONS.md`, which already
carry it. Reword the `checkResearcherWired` error to match the
`fleet.go:22` style, e.g. `"agent %q: the fleet orchestrator is not wired
yet"`.
**Acceptance:** `grep -rn "Wave [0-9]" --include='*.go' internal/ | grep -v _test.go` returns no matches outside comments that only cite an already-shipped milestone the way `M2`/`M4`/`M5` do; the `checkResearcherWired` error string carries no wave/version label.

### C3-CONS-2 — `worker_action.go`'s errors break the package-prefix convention
**Severity:** Low
**File:** `internal/researcher/fleet/worker_action.go:112,118,124,128,135,140`

Every other error constructed anywhere in this PR — and everywhere in v1
(`sink:`, `formatter:`, `planner:`, `interactions:`, `gemini:`, and this PR's
own `model:`, `search:`, `fetch:`, `fleet:`) — is prefixed with its owning
package name (confirmed by grep across `internal/sink`, `internal/formatter`,
`internal/planner`, plus every new file in `fleet/model`, `fleet/search`,
`fleet/fetch`, `worker.go` and `worker_researcher.go`). `parseAction`'s six
error sites are the sole exception in the diff:

```go
return action{}, fmt.Errorf("model returned an empty action")
return action{}, fmt.Errorf("search action is missing a query")
return action{}, fmt.Errorf("fetch action is missing a url")
return action{}, fmt.Errorf("model action is missing the required %q discriminator", "action")
return action{}, fmt.Errorf("model action is not valid JSON for the action schema: %v", err)
return action{}, fmt.Errorf("model requested unknown action %q: only search, fetch and final exist", a.Kind)
```

These are true `error` returns from a function that crosses a file boundary
within the package, not `Finding.Detail` strings (those are legitimately
unprefixed, matching `gemini.statusDetail`'s already-unprefixed human-readable
text — not a finding). Today the caller in `worker.go:237` immediately folds
the `.Error()` text into a bigger message, so nothing currently leaks
unprefixed, but the inconsistency will surface the moment one of these is
returned or wrapped directly (e.g. if `parseAction` gains a unit-test that
asserts on the raw error, or a caller starts using `errors.Is`/`%w`).
**Fix:** Prefix all six with `fleet:` to match every other error in the
package (`errWorkerNoModel`, the `NewWorker` validation errors, `Worker.Start`, etc.).
**Acceptance:** `grep -n 'fmt.Errorf("' internal/researcher/fleet/worker_action.go` shows every message starting with `fleet: `.

### C3-CONS-3 — Search fake's SSE frame is written without an `sseWrite`-style helper
**Severity:** Low
**File:** `internal/researcher/fleet/search/fake.go:186-192`

`AGENTS.md` ("Test infrastructure conventions") states: "For new SSE tests in
any package, use an `sseWrite`-style helper (`t.Helper()`; calls
`http.Flusher.Flush()` after writing) rather than raw `io.WriteString`
literals... so a handler that keeps the connection open cannot leave the
scanner blocked" — established by `internal/researcher/gemini/stream_test.go:36-46`.
`fake.go`'s SSE path writes the one scripted frame with a raw `fmt.Fprintf`
and returns, with no `Flush()`:

```go
if f.useSSE {
    w.Header().Set("Content-Type", "text/event-stream")
    w.WriteHeader(http.StatusOK)
    _, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
    return
}
```

This happens to work today because the handler returns immediately after the
single write (`net/http` flushes on handler completion), so there is no
active bug — this is a fit/precedent gap, not a functional one. It is also a
slightly different situation from the documented convention, which is framed
around a `*testing.T`-taking helper in a `_test.go` file, whereas `fake.go` is
shipped as non-test package code (itself an intentional, `DECISIONS.md`-recorded
deviation for cross-package reuse) and has no `t` to call `t.Helper()` on.
**Fix:** Either add a small unexported `sseWrite`-equivalent inside `fake.go`
that flushes explicitly (future-proofing against a later change that streams
more than one frame or interleaves writes), or add a one-line comment noting
why an explicit flush is unnecessary here (single write, handler returns
immediately) so the deviation from the documented convention reads as
deliberate rather than missed.
**Acceptance:** Either `fake.go` flushes after every SSE write, or a comment
at the `useSSE` branch explains why not.

## Intentional deviations noted (confirmed against DECISIONS.md — not flagged)

- Triplicated `allowedEndpointScheme`/`refuseCrossHostRedirects`/`readBounded`
  across `internal/config`, `internal/cli`, `fleet/model`, `fleet/search`
  (and `internal/interactions`'s own unexported originals) — recorded in
  `docs/DECISIONS.md` 2026-07-01 entries and cross-referenced from each
  package's doc comment and from `AGENTS.md`'s "Security-sensitive
  configuration" section. Naming is consistent (`allowedEndpointScheme`,
  matching `internal/config`, rather than `internal/cli`'s
  `allowedBaseScheme` — reasonable, since the fleet clients mirror config's
  copy, which is the one they're validated against at the CLI layer).
- `fetch.readCapped` truncates instead of erroring on an oversize body,
  unlike `model`/`search`/`interactions`'s `readBounded` — documented at
  `fetch/do.go:74-78` and in `DECISIONS.md`'s 2026-07-01 web_fetch entry as a
  deliberate SSRF-adjacent difference (partial page text is still useful).
- `FakeServer` shipped as exported, non-`_test.go` package code in both
  `fleet/model` and `fleet/search` — no v1 precedent (gemini's tests build
  raw `httptest.Server`s inline), but explicitly justified in `DECISIONS.md`
  as the trade for cross-chunk fake reuse (Wave-4 lead / worker sharing one
  scripted transport) — a case where citing forward reuse belongs in
  `DECISIONS.md`, and it correctly lives there rather than in the code
  comment.
- `ResearchConfig.Fleet` using `omitzero` while sibling scalar fields use
  `omitempty` — matches the existing `time.Time`/struct-field `omitzero`
  convention noted for v1.
- Error-message style split: package-prefixed (`model:`, `search:`,
  `fetch:`, `fleet:`) for adapter/seam errors vs. field-prefixed
  (`fleet.max_turns:`, `fleet.memory:`) for `config.Validate`/`FleetConfig.validate`
  — matches `internal/config`'s pre-existing field-prefix style exactly
  (`agent:`, `output:`, `budget:`, `mcp:`), not a package prefix.

## Verified OK

- `Options` struct + `New(Options)` constructor pattern in `fleet/model`,
  `fleet/search`, `fleet/fetch` matches the `gemini` adapter's
  config-struct convention (not `interactions`'s functional options) — the
  documented model for new researcher/adapter backends.
- `BaseURL`/endpoint override exposed as an `Options` field in all three new
  clients, resolved once at the CLI composition root
  (`internal/cli/research.go:buildWorker`), mirroring `gemini.Options.BaseURL`
  and `CHIRON_GEMINI_BASE_URL`.
- Table-test loop variable is `tt` throughout the new test files
  (`worker_test.go`, `worker_researcher_test.go`, `flags_fleet_test.go`,
  `fleet_test.go`), consistent with the codebase's dominant convention.
- `httptest.Server`/`FakeServer` lifecycle: created at the call site with
  `defer ...Close()` in every new `_test.go` file — the majority v1 pattern,
  not the `interactions`-package minority pattern.
- Flag naming (`fleet-model-endpoint`, `fleet-max-turns`, ...) is
  consistently `fleet-`-prefixed and maps 1:1 onto `FleetConfig`'s
  JSON/YAML field names via `flags.go`'s `ApplyFlags` switch, matching the
  existing flag/config wiring style; `fleet-ceiling` deliberately drops the
  `-gbp` suffix the same way the existing `--budget` flag does for
  `BudgetGBP`.
- `secret.Scrub` usage: every diagnostic path in `fleet/model` and
  `fleet/search` scrubs by exact-match-on-the-live-key first, then
  `secret.Scrub`, consistently, on every error-construction site (`Generate`,
  `errorFromResponse`, `doRequest`, `readResponse`, `readEventStream`) —
  matches the `AGENTS.md` "every output path... routes through secret.Scrub"
  rule and is applied uniformly, not spottily.
- `internal/trace` span/attribute usage: `SpanWorker` added alongside the
  fixed `SpanResearch/Plan/Start/Await/Format/Emit` vocabulary without
  altering it; `worker.go`'s `SetAttr` keys (`objective`, `status`, `turns`,
  `detail`) are lowercase/snake_case singular nouns, matching `run.go`'s
  (`query`, `agent`, `status`, `interaction_id`).
- en-GB spelling held throughout the new doc comments and prompt text
  ("synthesise", "visualise", "behaviour" not present but no en-US drift
  found either); the only American-spelled tokens are MCP/JSON-RPC wire
  method names (`initialize`, `notifications/initialized`), which correctly
  mirror the external spec the same way v1's wire types mirror the Gemini
  API's American field names.
- `docs/DECISIONS.md` entries added on this branch follow the exact
  `## YYYY-MM-DD — Title` header format used by every pre-existing entry.
- Exit-code path: `runWorkerResearch`/`buildWorker` errors are untyped and
  fall through to `ExitUsage` the same way a misconfigured Gemini run does;
  no new exit code was invented for the worker path.
- CLI composition root discipline: `buildWorker` resolves both fleet secret
  references itself, at the composition root, never inside the worker loop
  — matching `withRunSeams`'s Gemini-key resolution pattern and the
  "seams take a context and Deps, read no environment" rule in `AGENTS.md`.

## Questions

- None — the "Wave N" pattern (C3-CONS-1) and the missing prefixes
  (C3-CONS-2) are unambiguous against the cited sibling code and stated
  policy, not judgment calls.

## Verdict

**mostly consistent (minor adjustments)** — the new clients and worker loop
are a strong architectural match for the v1 house style; the fixes above are
a documentation/wording cleanup (C3-CONS-1), a six-line error-prefix fix
(C3-CONS-2), and an optional hardening of a test fake (C3-CONS-3), none of
which touch behaviour.
