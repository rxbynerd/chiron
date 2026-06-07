# Code Review — Cycle 1

Reviewer: code-reviewer agent  
Date: 2026-06-07  
Scope: waves 0–2 (M0–M2 + M3/M6/M7 per task list), HEAD on main.  
Excluded: `internal/cli`, `internal/config`, `internal/planner` — actively modified by impl-levers (M4 in progress). CLI files reviewed only where already committed.

---

## Summary

The overall architecture is sound and well-executed. The seam design is clean, the wire/domain split is correctly implemented, the bounded-read posture in the interactions client is thorough, and the secret scrubber is structurally watertight. The test suite is good: end-to-end smoke tests, golden-file formatter tests, wire-decode pinning, and scrubber coverage all stand out positively.

Two findings require attention before shipping: the `--input` file path ingests unbounded data into memory (contradicting the bounded-read ethos applied everywhere else), and the `Researcher` type's single-use contract is documented but not enforced, which is a correctness footgun for the v2 fleet reuse case. Three medium findings follow, with four low notes at the end.

---

## Findings

### HIGH

---

#### H1 — Unbounded memory allocation for `--input` files

**File:** `internal/researcher/gemini/input.go:46`

```go
data, err := os.ReadFile(in)
```

`os.ReadFile` reads the entire file into memory with no size limit. A user passing `--input /path/to/large.pdf` (a 500 MB corpus document, say) will OOM the process before the API even sees the request. This directly contradicts the bounded-read posture applied everywhere else in the codebase (`defaultMaxBodyBytes = 64 MiB`, `defaultMaxEventBytes = 16 MiB`, `maxErrorBodyBytes = 1 MiB` — all with explicit failure on overrun).

The base64-encoded body of a local file also factors into the JSON request size; the interactions client's `maxBodyBytes` would catch oversized *responses*, but the request body is built in `gemini.go` before any client bounds apply.

**Fix:** Stat the file before reading and reject it if it exceeds a configurable limit (suggested: 50 MiB, matching the Gemini API's documented per-part size guidance). Use `os.Open` + `io.LimitReader` + `io.ReadAll` to enforce it, and return a clear error naming the limit:

```go
const maxInputBytes = 50 << 20 // 50 MiB

f, err := os.Open(in)
if err != nil { ... }
defer f.Close()
data, err := io.ReadAll(io.LimitReader(f, maxInputBytes+1))
if err != nil { ... }
if int64(len(data)) > maxInputBytes {
    return interactions.Content{}, fmt.Errorf(
        "gemini: input %s exceeds %d-byte limit; use a URL reference instead", in, maxInputBytes)
}
```

---

#### H2 — `Researcher` single-use contract unenforced; `pollCount` and `lastQuery` are raceable on reuse

**File:** `internal/researcher/gemini/gemini.go:79–102, 248–283`

The type comment says "One instance serves one run at a time", but nothing enforces it. The relevant state:

- `r.pollCount.Store(0)` (line 283) resets the counter at the *end* of `Start()`, after `mu.Unlock()`.  
- `r.mu.Lock()` (line 279) guards only `lastQuery`, not `pollCount`.

If a caller invokes `Start()` on the same `Researcher` while a previous `Await()` is still running (incrementing `pollCount` via `OnPoll`), the reset races with live increments. The `lastQuery` would also be overwritten, corrupting the domain `Interaction` returned by `Result()` for the first run.

For v1, the run core serialises Start/Await/Result so this does not fire. But the seam interface is explicitly designed for v2 fleet reuse (DECISIONS.md), and a fleet orchestrator that reuses a `Researcher` across concurrent tasks would silently corrupt both cost signals and the query field in the domain model.

**Fix 1 (minimal, v1):** Move `pollCount.Store(0)` inside the mutex and add a comment that the lock guards both fields:

```go
r.mu.Lock()
r.lastQuery = task.Query
r.pollCount.Store(0)
r.mu.Unlock()
```

**Fix 2 (robust, pre-v2):** Add an `inFlight atomic.Bool` and guard `Start()`:

```go
if !r.inFlight.CompareAndSwap(false, true) {
    return "", errors.New("gemini: researcher already has a run in flight")
}
// ... clear on return:
defer r.inFlight.Store(false)
```

---

### MEDIUM

---

#### M1 — SSE `maxEventBytes` bounds individual scanner lines, not total event payload

**File:** `internal/interactions/stream.go:123–128, 182–186`

```go
bufSize := min(int64(64<<10), c.maxEventBytes)
scanner := bufio.NewScanner(resp.Body)
scanner.Buffer(make([]byte, bufSize), int(c.maxEventBytes))
```

`bufio.Scanner` enforces the max on individual tokens (lines). The SSE spec allows a single event's data to be split across multiple `data:` lines, each joined with `\n`. The `Next()` accumulation loop appends each line's value without a total-size guard:

```go
case "data":
    if haveData {
        data = append(data, '\n')
    }
    data = append(data, value...)  // unbounded across lines
    haveData = true
```

A server that splits a multi-MiB payload into many small `data:` lines (each under `maxEventBytes`) bypasses the bound entirely: every individual line passes the scanner check, but `data` grows without limit.

The existing `TestStreamEventTooLarge` only tests a single-line overrun. `TestStreamEventSequence` covers multi-line data (the `step.delta` event uses two `data:` lines) but does not verify the aggregate bound.

For the production Google API, events are always single-line JSON; the risk is against a misbehaving or MitM'd endpoint.

**Fix:** Track the accumulated `data` length and fail early:

```go
case "data":
    if haveData {
        data = append(data, '\n')
    }
    data = append(data, value...)
    haveData = true
    if int64(len(data)) > s.maxEventBytes {
        return nil, fmt.Errorf("interactions: SSE event payload exceeds %d-byte bound", s.maxEventBytes)
    }
```

Add a test: `WithMaxEventBytes(128)` with a two-line `data:` field whose combined payload exceeds 128 bytes but each line is under.

---

#### M2 — `stdinIsPiped` returns `true` for non-`*os.File` readers, enabling implicit config reads in embedding contexts

**File:** `internal/cli/commands.go:178–189`

```go
func stdinIsPiped(in io.Reader) bool {
    f, ok := in.(*os.File)
    if !ok {
        // Non-file readers (tests, future embedding) are explicit inputs.
        return true
    }
    ...
}
```

When a non-`*os.File` reader is set on stdin (e.g., a `*strings.Reader`, `*bytes.Buffer`, or any `io.Reader` a library host supplies), `stdinIsPiped` returns `true`, causing `loadBase` to call `config.Decode` on it. In tests this is safe because `config.Decode` returns defaults on EOF. In a future embedding context, a host that sets stdin to a live reader (a network connection, say) and does not intend to provide a base config would have that reader drained and interpreted as YAML.

The comment acknowledges the intent ("Non-file readers are explicit inputs"), but it inverts the usual principle: a non-file reader should be explicit about whether it carries config, not implicitly treated as one. The current behaviour also creates a silent dependency between `stdinIsPiped` and `config.Decode`'s EOF-tolerance that is not tested together.

**Fix:** Require the host to explicitly signal a piped config via `--config -` rather than relying on the reader type. For tests, replace `strings.NewReader("")` with an `os.File` pointed at `/dev/null`, or add a dedicated `Config.StdinPiped bool` field to the command context. At minimum, add a test that verifies `stdinIsPiped(strings.NewReader(""))` does not block or corrupt a run that supplies all flags explicitly.

---

#### M3 — `source.markdown()` injects unescaped API-provided strings into Markdown link syntax

**File:** `internal/formatter/markdown.go:72–78`

```go
func (s source) markdown() string {
    title := s.Title
    if title == "" {
        title = s.URI
    }
    return fmt.Sprintf("[%s](%s)", title, s.URI)
}
```

`Title` and `URI` come from Gemini citation annotations (`url_citation`, `file_citation`) via the adapter. Neither is sanitised before formatting:

- A `Title` containing `]` produces a malformed link: `[Title with ] bracket](url)`.
- A `URI` containing `)` produces a malformed link: `[Title](url-with)-paren)`.
- A `URI` of the form `javascript:alert(1)` (unlikely from Google but possible from a compromised or mocked response) would be embedded verbatim; some Markdown renderers pass through `javascript:` URIs to HTML.

The golden test `complete.md` only exercises well-formed URIs and titles, so these cases are not pinned.

**Fix:** At minimum, reject or percent-encode a `Title` containing `]` and a `URI` containing `)`. A conservative approach rejects any citation whose URI does not start with `https://` or `http://` (i.e., does not allow `javascript:`, `file:`, `data:` etc.):

```go
if s.URI != "" && !strings.HasPrefix(s.URI, "https://") && !strings.HasPrefix(s.URI, "http://") {
    // file and data URIs are not web-verifiable sources; skip.
    return ""
}
```

Add golden-file test cases for a citation with a `]` in its title and a citation with a `)` in its URI.

---

### LOW

---

#### L1 — `backoffDelay` uses bit-shift on `time.Duration` with implicit overflow detection

**File:** `internal/interactions/client.go:261–268`

```go
if attempt > 30 {
    return c.retryMaxDelay
}
d := c.retryBaseDelay << attempt
if d <= 0 || d > c.retryMaxDelay {
    d = c.retryMaxDelay
}
```

Left-shifting a `time.Duration` (an `int64`) by `attempt` is correct because Go's integer overflow is well-defined (wraps), and the `d <= 0` check catches a wrapped negative. The `attempt > 30` guard is a belt-and-braces over the `d <= 0` check.

The logic is correct but relies on two non-obvious mechanisms working in concert. A future maintainer unfamiliar with Go's overflow semantics might assume unsigned arithmetic. A floating-point multiply is clearer:

```go
d := time.Duration(float64(c.retryBaseDelay) * math.Pow(2, float64(attempt)))
if d <= 0 || d > c.retryMaxDelay {
    d = c.retryMaxDelay
}
```

Opinion: not worth changing while the tests pass, but worth a comment explaining why `d <= 0` is the overflow guard.

---

#### L2 — No test pins `MetricFailures` recording via the deferred path on infrastructure errors

**File:** `internal/run/run.go:111–116`, `internal/run/run_test.go`

`recordMetrics` records `MetricFailures` for failure-variant statuses (`result.Status != StatusCompleted`). The deferred closure records `MetricFailures` when `Run()` returns an error (infrastructure failures: start, await, format, sink). No test verifies that exactly one `MetricFailures` metric is emitted for a sink failure. The existing `TestRunSinkError` only checks that `run_completed` is not emitted; it does not instrument the tracer to count metric calls.

Consequence: if `recordMetrics` were accidentally called before the sink write (a refactor risk), metrics could double-count. The test should use a counting tracer to pin the count to 1.

---

#### L3 — `crypto/rand.Read` error dropped without minimum Go version enforcement

**File:** `internal/trace/jsonl.go:182–186`

```go
func newID(n int) string {
    b := make([]byte, n)
    _, _ = rand.Read(b)
    return hex.EncodeToString(b)
}
```

The comment cites the Go 1.24+ guarantee that `crypto/rand.Read` always succeeds. If the module's `go` directive in `go.mod` is set below 1.20 (where this guarantee was not present), the drop is silently unsafe. Trace IDs are not security-sensitive, but all-zero IDs would make spans uncorrelatable.

**Fix:** Verify `go.mod` specifies `go 1.21` or later (where the guarantee was documented). No code change needed if confirmed.

---

#### L4 — `toDomain` does not map `in.Model` for follow-up interactions; will misattribute the agent field

**File:** `internal/researcher/gemini/gemini.go:316–325`

```go
out := &types.Interaction{
    ...
    Agent: in.Agent,   // empty for follow-up Q&A interactions, which use in.Model
    ...
}
if out.Agent == "" {
    out.Agent = r.agentID  // falls back to the deep-research tier ID
}
```

The Interactions API uses `model` (e.g., `gemini-3.1-pro-preview`) instead of `agent` for follow-up Q&A interactions (INTERACTIONS-API.md §3). `toDomain` maps `in.Agent` and falls back to `r.agentID` when empty, so the domain `Interaction.Agent` for a follow-up would report the deep-research tier ID rather than the conversational model.

The `follow-up` command is not implemented yet (M4), so this has no current impact. Flag here so it is caught when `chiron follow-up` is wired in: either map `in.Model` to `Agent` or add `Model` to `types.Interaction`.

---

## Test quality assessment

The test suite is solid overall. Specific strengths:

- `TestAPIKeyNeverInErrors` and `TestKeyCannotTransitLogger` are structural proofs, not sample checks.
- Golden-file formatter tests with `--update` support.
- `TestCreateIsNeverRetried` pins the exact-one-attempt requirement against a 502.
- `TestPollContextCancellation` uses a 1-hour interval to guarantee the cancel fires in the wait, not the poll — a subtle but correct technique.

Gaps worth addressing:

- SSE multi-line data aggregate size not tested (see M1).
- `MetricFailures` deferred path not counted (see L2).
- `source.markdown()` malformed-input cases not in golden files (see M3).
- `toDomain` with `Model`-typed interaction not tested (no server response with `model` set, `agent` empty).
- `stdinIsPiped` with non-file readers and the implicit config-decode coupling not tested as a unit (see M2).

---

## Verdict

The code is safe to continue shipping on its current trajectory. There are no critical bugs, no secret leaks in the current code paths, and no data-loss risks in the run core. The two HIGH findings (unbounded `--input` reads, unenforced single-use on `Researcher`) should be addressed before M5 streaming work builds on these paths. The three medium findings are real but not show-stoppers; they can be addressed in the remediation wave. The seam architecture, retry logic, poll correctness, SSE parser, scrubber, and tracer are all well-implemented.
