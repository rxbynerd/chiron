# Remediation Brief — Chiron Cycle 1

**Scope:** Milestones M0–M3, M6, M7; HEAD `dea3bf3` on `main`  
**Reviewers:** code-reviewer · security-reviewer · spec-compliance-reviewer  
**Date:** 2026-06-07  
**Raw findings:** 22 (9 code · 8 security · 5 spec)  
**After deduplication:** 21 — one merge: Code M1 + Security MEDIUM-2 → **C1-SSE-1** (SSE
accumulation, 2-reviewer consensus)  
**Distribution:** 3 High · 4 Medium · 10 Low · 2 Info/no-action · 2 Delegated to M5

**Re-ratings:**  
- Security INFO-1 (API errors to stderr) elevated to **Low** — one-line fix; eliminates a
  structural scrubbing-bypass path even though it is not currently exploitable.  
- Security INFO-2 (OTel endpoint sensitivity) retained as **Info** — standard OTel behaviour;
  mitigation is documentation only, no code change.

---

## High — must fix before M5 proceeds

---

### C1-SEC-1 — `CHIRON_GEMINI_BASE_URL` accepts HTTP and arbitrary hosts: API-key exfiltration and SSRF
**(1-reviewer: security-reviewer)**  
**Files:** `internal/cli/research.go:44,88–90` · `internal/interactions/client.go:53–55`

`CHIRON_GEMINI_BASE_URL` is forwarded to `interactions.WithBaseURL()` without any validation
of scheme, host, or format — `WithBaseURL` only strips a trailing slash. Because the HTTP
client sets `x-goog-api-key` on every outbound request (`client.go:228`), an attacker who
can write to the process environment (compromised CI runner, shared host, misconfigured
container) points the variable at a server they control, and the API key is delivered on the
first request. Three distinct attack shapes: (1) `https://collect.attacker.com` — direct key
exfiltration; (2) `http://...` — key transmitted in cleartext, interceptable on any network
path; (3) `http://169.254.169.254` or any internal address — SSRF using the CLI host's
credentials. The variable is currently undocumented as security-sensitive, so operators have
no indication it should never be set in production.

**Fix:**  
Validate the variable at startup in `research.go` before constructing any client:

```go
if raw := os.Getenv(envGeminiBaseURL); raw != "" {
    u, err := url.Parse(raw)
    if err != nil || u.Scheme != "https" || u.Host == "" {
        return fmt.Errorf(
            "CHIRON_GEMINI_BASE_URL must be an absolute https:// URL, got %q", raw)
    }
    baseURL = raw
}
```

Additionally, document `CHIRON_GEMINI_BASE_URL` in `AGENTS.md` (or a new `SECURITY.md`) as
security-sensitive: its absence is the safe default; it must never be set in production; in
v2 GKE deployments consider a build tag or compile-time sentinel that disables it entirely in
release builds.

**CWE:** CWE-918 (SSRF) · CWE-319 (Cleartext Transmission of Sensitive Information)

**Acceptance criteria:**
- A test in `internal/cli/` sets `CHIRON_GEMINI_BASE_URL=http://evil.example.com` and asserts
  that `runResearch()` (or the config-validation step) returns a validation error _before_
  making any outbound HTTP request.
- A second test case covers a missing scheme, an `ftp://` URL, and a bare string with no host.
- A valid `https://` override (e.g., pointing at an `httptest.Server`) is accepted and used.
- `CHIRON_GEMINI_BASE_URL` is present in `AGENTS.md` or `SECURITY.md` with the security
  caveat noted.

---

### C1-CODE-1 — Unbounded `--input` file reads: OOM on large input files
**(1-reviewer: code-reviewer)**  
**File:** `internal/researcher/gemini/input.go:46`

```go
data, err := os.ReadFile(in)
```

`os.ReadFile` reads the entire named file into memory with no size cap. A user passing a
500 MiB PDF corpus as `--input` will exhaust process memory before the API request is built.
This directly contradicts the bounded-read posture applied everywhere else in the codebase
(`defaultMaxBodyBytes = 64 MiB`, `defaultMaxEventBytes = 16 MiB`, `maxErrorBodyBytes = 1 MiB`,
all with explicit failure on overrun). The base64-encoded file body also inflates the JSON
request size; the interactions client's `maxBodyBytes` guards only _responses_, not the
outbound request.

**Fix:**  
Replace `os.ReadFile` with a bounded read using `io.LimitReader`:

```go
const maxInputBytes = 50 << 20 // 50 MiB — matches Gemini API per-part size guidance

f, err := os.Open(in)
if err != nil {
    return interactions.Content{}, err
}
defer f.Close()

data, err := io.ReadAll(io.LimitReader(f, maxInputBytes+1))
if err != nil {
    return interactions.Content{}, err
}
if int64(len(data)) > maxInputBytes {
    return interactions.Content{}, fmt.Errorf(
        "gemini: input %s exceeds %d-byte limit; use a URL reference instead",
        in, maxInputBytes)
}
```

**Acceptance criteria:**
- A test writes a temp file exactly 1 byte over `maxInputBytes` and verifies the error message
  names both the file path and the limit.
- A test writes a file at exactly `maxInputBytes` and verifies it is accepted (boundary is
  inclusive).
- `grep -r 'os\.ReadFile' internal/researcher/gemini/input.go` returns nothing after the fix.

---

### C1-CODE-2 — `Researcher` single-use contract unenforced; `pollCount` and `lastQuery` raceable on reuse
**(1-reviewer: code-reviewer)**  
**File:** `internal/researcher/gemini/gemini.go:79–102, 248–283`

The type comment states "One instance serves one run at a time", but nothing enforces it.
Two problems coexist: (1) `r.pollCount.Store(0)` at line 283 resets the counter _after_
`mu.Unlock()`, racing with any live `OnPoll` increment if the same instance is reused; (2)
`r.mu.Lock()` guards `lastQuery` only, not `pollCount`. In v1 the run core serialises
Start/Await/Result, so neither fires in practice. The seam is explicitly designed for v2 fleet
reuse (DECISIONS.md): a fleet orchestrator that reuses a `Researcher` across concurrent tasks
would silently corrupt both the `Interaction.Query` field (returned by `Result()`) and the
poll-count cost signal.

**Fix — two parts, apply both:**

1. Move `pollCount.Store(0)` inside the existing mutex so both state resets are atomic:

```go
r.mu.Lock()
r.lastQuery = task.Query
r.pollCount.Store(0)
r.mu.Unlock()
```

2. Add an `inFlight atomic.Bool` to enforce the contract at runtime:

```go
if !r.inFlight.CompareAndSwap(false, true) {
    return "", errors.New("gemini: researcher already has a run in flight")
}
defer r.inFlight.Store(false)
```

**Acceptance criteria:**
- `go test ./internal/researcher/gemini/... -race` passes with the fix applied.
- A test calls `Start()` a second time while the first is still in-flight (use a channel to
  block the mock server) and asserts the `"already has a run in flight"` error.
- A test (or an extension of the existing `TestPollContextCancellation`) asserts that
  `pollCount` reads zero at the start of a fresh `Start()` after the previous run completed
  normally.

---

## Medium — fix in this remediation wave

---

### C1-SSE-1 — SSE multi-line `data:` accumulation within one event is unbounded
**(2-reviewer consensus: code-reviewer, security-reviewer)**  
**File:** `internal/interactions/stream.go:123–128, 163–187`

The SSE parser applies `maxEventBytes` (default 16 MiB) as the maximum size of _individual
scanner lines_ via `scanner.Buffer(...)`, not as a cap on the total payload accumulated from
multiple `data:` fields within a single event. Each line is bounded by the scanner; the
accumulation loop appends lines without a total-size guard:

```go
case "data":
    if haveData {
        data = append(data, '\n')
    }
    data = append(data, value...)   // unbounded across lines
    haveData = true
```

A server that fragments a multi-MiB payload across many small `data:` lines (each individually
under `maxEventBytes`) bypasses the bound entirely; `data` grows without limit until the event
is dispatched. The security reviewer notes this is only reachable against an endpoint under
attacker control — if C1-SEC-1 is fixed, the residual exposure is a MitM scenario. The code
reviewer confirms the production Google API sends single-line JSON events. The existing
`TestStreamEventTooLarge` only covers a single-line overrun; `TestStreamEventSequence` does
not verify the aggregate bound.

**Fix:**  
Add an accumulation check immediately after each `data:` append:

```go
case "data":
    if haveData {
        data = append(data, '\n')
    }
    data = append(data, value...)
    haveData = true
    if int64(len(data)) > s.maxEventBytes {
        return nil, fmt.Errorf(
            "interactions: SSE event data exceeds %d-byte bound", s.maxEventBytes)
    }
```

**Acceptance criteria:**
- A test configures the stream with `WithMaxEventBytes(128)`, feeds an SSE event with two
  `data:` lines each containing 80 bytes (combined 161 bytes > 128), and asserts a
  bound-exceeded error is returned.
- Each individual line in the test is under 128 bytes (to confirm the scanner alone would not
  catch it).
- The error fires _before_ the event is dispatched to the caller.
- All existing stream tests (`TestStreamEventTooLarge`, `TestStreamEventSequence`) continue to
  pass.

---

### C1-SEC-2 — `x-goog-api-key` forwarded on cross-domain HTTP redirects
**(1-reviewer: security-reviewer)**  
**File:** `internal/interactions/client.go:105–116, 213–255`

Go's `net/http` client strips `Authorization`, `Www-Authenticate`, `Cookie`, and `Cookie2` on
cross-domain redirects (`shouldCopyHeaderOnRedirect`), but not `x-goog-api-key`. If the server
at `c.baseURL` responds with a 301/302/307/308 whose `Location` points to a different host, Go
follows the redirect and re-sends the API key header to the new host. Against the production
Google API this is not exploitable: Google controls its redirect destinations. Combined with
C1-SEC-1, an attacker-controlled relay can serve a redirect to a second logging server,
delivering the key to two distinct endpoints. This fix should be applied independently of
C1-SEC-1 so the protection holds regardless of what `baseURL` resolves to.

**Fix:**  
Supply a `CheckRedirect` function when constructing the `http.Client`:

```go
httpClient = &http.Client{
    CheckRedirect: func(req *http.Request, via []*http.Request) error {
        if req.URL.Host != via[0].URL.Host {
            return fmt.Errorf(
                "interactions: redirect to %s refused: cross-origin redirect with sensitive headers",
                req.URL.Host)
        }
        if len(via) >= 3 {
            return fmt.Errorf("interactions: too many redirects")
        }
        return nil
    },
}
```

**CWE:** CWE-601 (Open Redirect) · CWE-200 (Exposure of Sensitive Information)

**Acceptance criteria:**
- A test uses two `httptest.Server` instances on different ports; the first responds 302 to the
  second. The client returns an error and the `x-goog-api-key` header is not present in any
  request captured by the second server.
- A test confirms a same-host redirect (same `httptest.Server`, different path) is followed
  normally.
- `len(via) >= 3` redirect loops are rejected.

---

### C1-CODE-3 — `source.markdown()` injects unescaped API-provided strings into Markdown link syntax
**(1-reviewer: code-reviewer)**  
**File:** `internal/formatter/markdown.go:72–78`

`Title` and `URI` come from Gemini citation annotations (`url_citation`, `file_citation`) via
the wire adapter. Neither is sanitised before formatting into `[Title](URI)`:

- A `Title` containing `]` produces a malformed link: `[Title with ] bracket](url)`.
- A `URI` containing `)` produces a malformed link: `[Title](url-with)-paren)`.
- A `URI` of the form `javascript:alert(1)` is embedded verbatim; some Markdown renderers pass
  `javascript:` URIs through to HTML.

These values are API-provided and should not be treated as trusted. The golden test
`complete.md` only exercises well-formed URIs and titles.

**Fix:**  
Validate URI scheme and sanitise bracket characters before formatting:

```go
func (s source) markdown() string {
    if s.URI != "" &&
        !strings.HasPrefix(s.URI, "https://") &&
        !strings.HasPrefix(s.URI, "http://") {
        // file:, javascript:, data: URIs are not web-verifiable sources; omit.
        return ""
    }
    title := strings.ReplaceAll(s.Title, "]", `\]`)
    if title == "" {
        title = s.URI
    }
    uri := strings.ReplaceAll(s.URI, ")", "%29")
    return fmt.Sprintf("[%s](%s)", title, uri)
}
```

**Acceptance criteria:**
- Add three golden-file fixtures: (a) a citation with `]` in its title — verify the bracket is
  escaped in the output; (b) a citation with `)` in its URI — verify the paren is
  percent-encoded; (c) a citation with a `javascript:` URI — verify no Markdown link is emitted
  for that source.
- `go test ./internal/formatter/... -update` regenerates all golden files without error.
- `go test ./internal/formatter/...` (without `-update`) passes against the pinned golden files.

---

### C1-CODE-4 — `stdinIsPiped` treats non-`*os.File` readers as piped config, enabling implicit config reads in embedding contexts
**(1-reviewer: code-reviewer)**  
**File:** `internal/cli/commands.go:178–189`

When a non-`*os.File` reader is supplied as stdin (e.g., `*strings.Reader`, `*bytes.Buffer`,
or any `io.Reader` a library host provides), `stdinIsPiped` returns `true`, causing `loadBase`
to call `config.Decode` on it. In tests this is benign because `config.Decode` returns defaults
on EOF. In a future embedding context, a host that sets stdin to a live reader it controls and
does not intend to supply a base config would have that reader drained and silently interpreted
as YAML. The comment in the code acknowledges the intent ("Non-file readers are explicit
inputs") but inverts the safe principle: unknown reader types should be treated as _not_ piped,
not as implicitly carrying config.

**Fix:**  
Invert the default for non-`*os.File` readers:

```go
func stdinIsPiped(in io.Reader) bool {
    f, ok := in.(*os.File)
    if !ok {
        return false // unknown reader types are not implicit config sources
    }
    info, err := f.Stat()
    if err != nil {
        return false
    }
    return (info.Mode() & os.ModeCharDevice) == 0
}
```

If any existing test supplies `strings.NewReader("")` to simulate piped config, replace with
`os.Pipe()` or a temp file. If test injection is needed, add a `StdinPiped bool` to the command
context rather than relying on reader-type inspection.

**Acceptance criteria:**
- `go test ./internal/cli/...` passes after the inversion.
- A unit test asserts `stdinIsPiped(strings.NewReader(""))` returns `false`.
- A test verifies that a run supplying all required flags explicitly with a `strings.NewReader`
  as stdin completes without attempting to decode the reader as config.

---

## Low — non-blocking; address in this wave where trivial

Findings are grouped by file where multiple low findings touch the same location.

---

**C1-SEC-4 — Report and asset files written world-readable**  
**File:** `internal/sink/file.go:47,51`  
Both `os.WriteFile` calls use `0o644`; the JSONL trace correctly uses `0o600`. Reports contain
query text, citation lists, token usage, and YAML front matter — sensitive on multi-user or
shared-filesystem hosts.  
**Action:** Change both calls to `0o600`. If group-readable output is ever needed, make the
mode a configurable field on `File` defaulting to `0o600` rather than a constant.  
**CWE:** CWE-732

---

**C1-SEC-3 — CI actions pinned to mutable tag references**  
**File:** `.github/workflows/ci.yml:9–10,16–17,19,30,33–34`  
`actions/checkout@v6`, `actions/setup-go@v6`, `golangci/golangci-lint-action@v9` use mutable
tags. Note: `@v6` does not exist for `checkout` or `setup-go` at review time and may resolve
unexpectedly. Tags on third-party repos can be silently redirected.  
**Action:** Pin each action to its full commit SHA with a `# vX.Y.Z` comment. Verify current
correct versions (checkout@v4, setup-go@v5) before pinning. Consider Dependabot
`update-actions` for automated renewal.  
**CWE:** CWE-829

---

**C1-SEC-5 — `secret://file` resolver does not warn on world-readable key files**  
**File:** `internal/secret/resolve.go:113–129`  
A secret file with `0o644` or `0o640` is silently read and used with no feedback to the
operator.  
**Action:** After `os.ReadFile`, stat the file and emit `slog.Warn` if
`mode.Perm() & 0o177 != 0`. Use `slog.Warn` (not an error) to avoid blocking a run on a
mis-permissioned but otherwise valid file. The existing `ScrubHandler` covers the `path`
attribute.  
**CWE:** CWE-732

---

**C1-SEC-6 — API error messages reach `stderr` without credential scrubbing**  
**File:** `internal/cli/root.go:20`  
**Re-rated from:** Security INFO-1 → Low  
`fmt.Fprintln(os.Stderr, "chiron:", err)` bypasses `ScrubHandler`. The Gemini API does not
currently echo the API key in error messages, so this is not currently exploitable. The
structural guarantee ("a key cannot transit logs via any path") does not extend to this path.  
**Action:** Replace with `fmt.Fprintln(os.Stderr, "chiron:", secret.Scrub(err.Error()))`. One
function call; eliminates the bypass.

---

**C1-CODE-6 — `MetricFailures` deferred-path emission count not asserted in tests**  
**File:** `internal/run/run.go:111–116` · `internal/run/run_test.go`  
`TestRunSinkError` checks `run_completed` is not emitted but does not count `MetricFailures`
emissions. A refactor that double-counts would go undetected.  
**Action:** Extend `TestRunSinkError` (or add a sibling) with a counting tracer that asserts
`MetricFailures` is emitted exactly once on a sink failure.

---

**C1-CODE-7 — `crypto/rand.Read` error silently dropped; `go.mod` version not verified in comments**  
**File:** `internal/trace/jsonl.go:182–186` · `go.mod`  
`_, _ = rand.Read(b)` relies on the Go 1.20+ guarantee that `crypto/rand.Read` never fails.
No comment records this dependency; a reader might assume it is safe to lower the `go`
directive.  
**Action:** Verify `go.mod` specifies `go 1.21` or later (where the guarantee is documented).
No code change required if confirmed. Add a comment in `newID` citing the Go version
guarantee: `// crypto/rand.Read never fails on Go 1.20+; go.mod enforces >= 1.21.`

---

**C1-CODE-8 + C1-SPEC-1 — `toDomain` missing `in.Model` mapping for follow-up interactions; `OutputThoughtSummary` constant unreachable** *(same file — fix together)*  
**File:** `internal/researcher/gemini/gemini.go:316–325, 334–343`  
`toDomain` falls back to `r.agentID` when `in.Agent` is empty, but follow-up Q&A interactions
use `in.Model` rather than `in.Agent` (INTERACTIONS-API.md §3) — the fallback will misattribute
the domain `Interaction.Agent`. Separately, `OutputThoughtSummary` is declared in the domain
model but `toDomain` never assigns it; a caller iterating outputs for thought summaries finds
nothing. Both issues are pre-M4/M5 scope with no current impact.  
**Action:** No functional change required now. Add a `// TODO(M4): map in.Model when
chiron follow-up is wired` comment at the `out.Agent` fallback. Add a `// TODO(M5): map
thought content parts to OutputThoughtSummary before streaming display goes live` comment at
the `OutputThoughtSummary` constant. Ensures neither is missed when the respective milestones
land.

---

**C1-SPEC-2 — `otel/trace` direct dependency not listed in `DECISIONS.md`**  
**File:** `docs/DECISIONS.md`  
`go.mod` includes `go.opentelemetry.io/otel/trace v1.44.0` as a fourth direct OTel module;
DECISIONS.md lists only three. The code is correct; the decision log is incomplete.  
**Action:** Add `go.opentelemetry.io/otel/trace` to the OTel dependency entry in DECISIONS.md:
used for `oteltrace.Tracer` and `oteltrace.SpanFromContext` in `otel.go`.

---

**C1-CODE-5 — `backoffDelay` bit-shift overflow handling relies on two non-obvious mechanisms**  
**File:** `internal/interactions/client.go:261–268`  
The overflow guard (`d <= 0`) relies on Go's defined-wrapping integer overflow semantics; the
belt-and-braces `attempt > 30` guard is redundant but benign. Both together are correct but
opaque to a maintainer unfamiliar with Go's overflow contract.  
**Action:** Add a comment above the shift:
`// Left-shifting time.Duration (int64) wraps on overflow; d <= 0 catches a wrapped negative.`
No logic change.

---

## Info — no code change in this wave

**C1-SEC-7 — OTel trace endpoint receives research queries without authentication validation**  
*(Security INFO-2, retained as Info)*  
Standard OTel behaviour shared by all OTLP-instrumented applications; not a Chiron-specific
code bug. The scrubber runs before the OTel SDK sees any string, so the API key is protected;
but research queries and interaction IDs are visible to whatever endpoint is configured.
Mitigation is operational: document `OTEL_EXPORTER_OTLP_ENDPOINT` and
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` as security-sensitive deployment configuration in
`AGENTS.md` or `SECURITY.md` alongside C1-SEC-1. In GKE v2, restrict both to
operator-supplied Kubernetes Secrets rather than user-configurable env vars.

**C1-SPEC-3 — M4-scope stubs registered but inert**  
*(Spec F5, informational)*  
`research-config`, `get`, `follow-up` commands return `errNotImplemented`; `--plan` and
`--budget` are parsed but not forwarded. Expected pre-M4 state; no spec violation within the
M0–M3+M6+M7 scope. No action required in this wave.

---

## Delegated to M5 — do not fix in this remediation wave

These two findings are spec-compliance gaps that are only meaningful once M5 (streaming) ships.
They are tracked on task #8 (M5 streaming). **The M5 implementer must address both the API
request wiring and the metric update alongside the display-layer work** — adding the display
layer alone will produce nothing to display and emit a misleading metric.

---

### C1-M5-1 — `--stream` / `--quiet` flags have no effect on the API `thinking_summaries` field
*(Spec Finding 1, Medium — deferred to M5)*  
**Files:** `internal/cli/research.go:88–99`

`runResearch()` builds `gemini.Options` without setting `ThinkingSummaries`; `cfg.Stream` is
never read in this path:

```go
res, err := gemini.New(gemini.Options{
    APIKey:    apiKey,
    Tier:      cfg.Agent,
    Visualise: cfg.Visualise,
    // cfg.Stream is never read here
})
```

`gemini.Researcher` therefore always emits `thinking_summaries: "none"` on the create request,
regardless of whether `--stream` (default `true`) or `--quiet` is passed. The two flags are
mutually exclusive in the CLI definition but produce identical API requests. DECISIONS.md
documents the deferral explicitly: "polling runs leave it false ('none') — there is nothing to
display them on until the M5 streaming surface."

**Why M5 must fix both this and the display layer:** wiring the display surface without also
toggling `ThinkingSummaries` means the API never sends summaries to display. The connection is
a one-liner but must land before the streaming path is exercised.

**Fix for M5 implementer:**

```go
res, err := gemini.New(gemini.Options{
    APIKey:            apiKey,
    Tier:              cfg.Agent,
    Visualise:         cfg.Visualise,
    ThinkingSummaries: cfg.Stream, // wires --stream/--quiet to the API request
})
```

**Acceptance criteria for M5:**
- An integration test with a mocked server and `--stream` verifies the create payload contains
  `thinking_summaries` set to a non-`"none"` value.
- An integration test with `--quiet` verifies `thinking_summaries: "none"`.
- The existing `TestCreateRequestEmitsExplicitBooleans` is extended (or a sibling added) to
  cover `thinking_summaries`.

---

### C1-M5-2 — `reconnect_count` metric always emits 0; `toDomain` never sets `Usage.ReconnectCount`
*(Spec Finding 2, Medium — deferred to M5)*  
**Files:** `internal/run/run.go:222` · `internal/researcher/gemini/gemini.go:349–360`

`recordMetrics()` emits `MetricReconnectCount` from `result.Usage.ReconnectCount`. The
`toDomain()` mapping never sets this field:

```go
out.Usage = types.Usage{
    // ...
    // ReconnectCount not set — permanently 0
}
```

The metric is structurally correct: wired, recorded, exported to both OTel and JSONL. It just
always reports 0. If M5 adds reconnect logic to the adapter without also updating this field,
the metric will silently misrepresent reconnects.

**Fix for M5 implementer:**  
When the SSE streaming path with `last_event_id` resume is implemented, maintain a reconnect
counter in the streaming loop (increment on each successful `last_event_id`-based resume) and
assign it in `toDomain`:

```go
out.Usage = types.Usage{
    // ...
    ReconnectCount: reconnects,
}
```

**Acceptance criteria for M5:**
- An integration test simulating one `last_event_id` resume verifies `MetricReconnectCount = 1`
  is emitted.
- A polling run (no reconnects) continues to emit `MetricReconnectCount = 0`.
