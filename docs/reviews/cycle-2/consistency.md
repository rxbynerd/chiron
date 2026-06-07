# Consistency Review — Chiron v1 at HEAD (90fe191)

Reviewed by the consistency-reviewer agent. Scope: all packages changed across the
six implementer waves. Source read before flagging every finding; intentional
choices from `docs/DECISIONS.md` are excluded.

---

## Summary

The codebase reads as largely native and coherent. The seam interfaces are
uniform (ctx-first, error-last, `Start`/`Await`/`Result` verbs). Error wrapping
uses `%w` with lower-case package-prefixed messages throughout. Transport kind
constants, span names, and metric names are stable snake\_case. The M4 and M5
additions (plan, budget, model, streaming) go through the same
`RegisterFlags`/`ApplyFlags` path established in M0. The main visible seam
between authors is one misapplied error prefix that will mislead future
debugging, plus two test-infrastructure idioms that did not converge.

---

## Findings by Severity

### High

#### H1 — Wrong error prefix in `researcher/gemini/stream.go` (lines 130, 139)

`consumeStream` emits two errors with the prefix `"interactions:"` rather than
`"gemini:"`, which is the correct prefix for every other error in that package.

**Divergent style** (`internal/researcher/gemini/stream.go:130,139`):

```go
return false, fmt.Errorf("interactions: stream ended before the interaction concluded: %w", err)
return false, fmt.Errorf("interactions: stream error event: %w", ev.Err)
```

**Established convention** (`internal/researcher/gemini/gemini.go:~74,~240`):

```go
return fmt.Errorf("gemini: creating interaction: %w", err)
return fmt.Errorf("gemini: awaiting interaction %s: %w", id, err)
```

Impact: when `Await` returns one of these errors and a caller walks the chain
(e.g. in `run.go`), the `"interactions:"` prefix points at the wrong package.
A contributor debugging a streaming failure would look in `internal/interactions`
rather than the gemini adapter's stream layer.

Recommendation: replace `"interactions:"` with `"gemini:"` at both sites. The
`io.EOF` wrap at line 130 can optionally drop the `%w` and use a fresh
`errors.New`-style sentinel since callers key on message text, not unwrapping
behaviour for that path — but the prefix fix is the critical part.

---

### Medium

#### M1 — Table-test loop variable: `tt` (majority) vs `tc` (gemini, CLI)

Three packages establish `tt` as the table-test loop variable:

- `internal/interactions`: `client_test.go`, `poll_test.go`, `stream_test.go`
- `internal/secret`: `scrub_test.go`, `resolve_test.go`
- `internal/config`: `config_test.go`

Two later packages use `tc`:

- `internal/researcher/gemini`: `gemini_test.go:453` (`TestConstructionValidation`),
  `stream_test.go:330` (`TestThinkingSummariesFollowsStreamOption`)
- `internal/cli`: `research_test.go:185` (`TestStreamTogglesThinkingSummaries`)

This is a cross-package divergence with no apparent rationale. The gemini
package is also internally split: non-table tests there use `t` for the running
test and would have no conflict, but the table tests diverge from their siblings.

Recommendation: standardise on `tt`. It is the convention in three packages
(vs two) and is the dominant idiom in the broader Go community. Change the three
affected test loops at the sites listed above.

#### M2 — `httptest` server lifecycle: internal helper vs caller-managed defer

`internal/interactions` creates and closes the httptest server inside the test
helper — callers never see the server:

```go
// interactions/client_test.go
func newTestClient(t *testing.T, h http.Handler, opts ...Option) *Client {
    t.Helper()
    srv := httptest.NewServer(h)
    t.Cleanup(srv.Close)          // lifecycle managed here
    ...
}
```

`internal/researcher/gemini` (and `internal/cli`) create the server in the test
body and pass it into the helper:

```go
// researcher/gemini/gemini_test.go
server := httptest.NewServer(...)
defer server.Close()              // lifecycle at call site
r := newResearcher(t, server, ...)
```

Neither is wrong. The gemini/CLI pattern (2 packages) is more flexible because
it allows the test to capture the server or pass a nil handler to exercise
construction-only paths. The interactions pattern (1 package) is tidier for
uniform cases.

Recommendation: for new packages, follow the gemini/CLI pattern (external
creation, explicit `defer server.Close()`). It is the majority and more
readable when tests need to inspect or vary the handler. Document this in
`AGENTS.md` under the test-infrastructure note so future implementers do not
need to infer it.

---

### Low

#### L1 — `SetAttr("resumed", "true")` passes a string instead of a bool

`internal/run/run.go:174`:

```go
root.SetAttr("resumed", "true")
```

`SetAttr` accepts `any`; the OTel binding dispatches on the concrete type. A
string `"true"` produces a string attribute rather than a bool attribute,
diverging from how other scalar attributes are set (e.g. `SetAttr("interaction_id", id)`
passes a string because the value is genuinely a string).

Established pattern for boolean flags elsewhere in the span layer: pass the Go
`bool` value directly so the OTel backend records the correct type.

Recommendation: `root.SetAttr("resumed", true)`.

#### L2 — `decodeJSONBody` helper exists only in `gemini/stream_test.go`

`internal/researcher/gemini/stream_test.go:392` defines:

```go
func decodeJSONBody(t *testing.T, r *http.Request, into any) { ... }
```

`internal/researcher/gemini/gemini_test.go` (an older file in the same package)
does the same work inline at every call site:

```go
// gemini_test.go:51–53
if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
    t.Fatalf("decoding create body: %v", err)
}
```

Because both files share the same `package gemini` test binary, `decodeJSONBody`
is available to `gemini_test.go` but is not used there. This is not wrong, but
it means the helper is only half-adopted.

Recommendation: replace the inline decoding in `gemini_test.go` (`TestStartBuildsCreateRequest`,
`TestTierMapping`, `TestCustomTemplate`, `TestInputParts`) with calls to
`decodeJSONBody`. This is a pure clean-up with no behaviour change.

#### L3 — SSE frame writing: `sseWrite` helper vs raw string literals

`internal/researcher/gemini/stream_test.go:36` defines `sseWrite(t, w, frames...)`
and uses it consistently across all streaming tests.

`internal/interactions/stream_test.go` (written earlier) embeds SSE frames as
raw multi-line string literals written directly with `io.WriteString` or
`w.Write`, with no shared helper. This is functional but creates a maintenance
discrepancy: the SSE frame format exists in two independent representations.

Recommendation: low priority — the interactions package's SSE tests are complete
and the helper would only matter if new SSE tests are added there. Note for
`AGENTS.md`: if new streaming tests are added to the interactions package, use a
`sseWrite`-style helper consistent with `gemini/stream_test.go`.

#### L4 — File-level preamble comment in `formatter/markdown.go`

`internal/formatter/markdown.go` opens with a descriptive block comment placed
immediately before `package formatter` with no blank line between them, making
it a second package doc comment:

```go
// The markdown Formatter renders one Interaction as a single portable
// document (PROPOSAL.md §4.4): YAML front matter carrying the run's
// identity and cost signals ...
package formatter
```

`internal/formatter/formatter.go` already carries the canonical package doc
(`// Package formatter defines the Formatter seam:`). Every other implementation
file in the codebase (`client.go`, `stream.go`, `otel.go`, `jsonl.go`,
`stdout.go`, `stdio.go`) opens directly with `package X` or a declaration-level
`//` doc comment, not a secondary package doc.

Having two package-level comments causes `go doc` to show both. The intent here
is clearly to document the `Markdown` type's rendering contract, not the package
a second time.

Recommendation: move the comment to immediately precede the `Markdown` type
declaration (or `NewMarkdown`), where it belongs as a type doc comment:

```go
// Markdown renders one Interaction as a single portable document
// (PROPOSAL.md §4.4): YAML front matter ...
type Markdown struct{ ... }
```

---

## Intentional Deviations Noted

The following divergences were found but confirmed as documented decisions; they
are listed here for the record and must not be re-flagged.

- **Two clients in the gemini adapter** (`create` with `WithMaxRetries(0)`, `poll`
  with default retries) — DECISIONS.md §M2.
- **Config-struct `Options` in the gemini adapter vs functional options in the
  interactions client** — the gemini adapter's large, non-composable option
  surface makes a struct more legible; the interactions client's narrow,
  test-overridable options suit functional options. Both patterns are in active
  use in different layers. DECISIONS.md §M0/M2.
- **Streaming fallback to polling** after `maxStreamFailures` consecutive
  failures — DECISIONS.md §M5.
- **Metrics as span attributes** (not the OTel metrics SDK) — DECISIONS.md §M6.
- **`omitzero` tags on `time.Time` and struct-typed fields** alongside `omitempty`
  on scalar fields — DECISIONS.md §M2.
- **No committed proto-generated code** in `proto/chiron/v1` — DECISIONS.md §M7.
- **`Multi` factory function in `internal/sink`** without a `New` prefix — the
  function returns an interface over an unexported type; not a constructor for a
  named public type. Analogous to `config.Default()`.

---

## Questions

None. All ambiguous cases were resolved by reading the source or DECISIONS.md.

---

## Verdict

`mostly consistent (minor adjustments)`

One high-severity finding (H1) requires a fix before the error prefix misleads
a debugging session. Two medium findings (M1, M2) are worth unifying on a single
standard. The low findings are cosmetic and safe to batch with other clean-up.
The seam interfaces, error-wrapping discipline, flag plumbing, and naming
conventions are uniform across all six implementer waves.
