# Security Review — Cycle 2 (Final v1)

**Reviewer:** security-reviewer agent  
**Date:** 2026-06-07  
**Scope:** HEAD at `90fe191`; M4 levers (planner, budget gate, `research-config`, `get`, `follow-up`),
cycle-1 remediation, and M5 streaming (SSE reconnect, `last_event_id`, thought summaries, error events)  
**Method:** Manual source review: closure verification of all cycle-1 findings, then new-surface analysis
of every M4/M5 addition. Files read: `internal/cli/{research,commands,root,levers_test,research_test,baseurl_test}.go`,
`internal/planner/{planner,session}.go`, `internal/researcher/gemini/{gemini,planner,followup,stream}.go`,
`internal/interactions/{client,stream,redirect_test}.go`, `internal/config/config.go`,
`docs/reviews/cycle-1-remediation.md`, `.github/workflows/ci.yml`, `go.mod`.

---

## Summary

One new **low**-severity finding. All cycle-1 findings are confirmed closed. All new M4/M5 surfaces
examined are sound. The codebase is in good shape for v1 shipment.

---

## Part 1 — Cycle-1 Closure Verification

### C1-SEC-1 — CHIRON_GEMINI_BASE_URL (High → Closed)

**Status: Closed. Remediation is sound.**

`allowedBaseScheme()` (`internal/cli/research.go:272–286`) validates the override before any client
is built:

```go
func allowedBaseScheme(u *url.URL) bool {
    switch u.Scheme {
    case "https":
        return true
    case "http":
        host := u.Hostname()
        if host == "localhost" {
            return true
        }
        ip := net.ParseIP(host)
        return ip != nil && ip.IsLoopback()
    default:
        return false
    }
}
```

The loopback carve-out is tight. `"localhost"` is an exact, case-sensitive string comparison —
no resolution occurs, so a hostname that *later* resolves to `127.0.0.1` cannot exploit this path.
Non-`localhost` names are parsed by `net.ParseIP`, which returns nil for any non-IP string, so no
DNS name other than the literal `"localhost"` can pass. IP addresses satisfying `ip.IsLoopback()`
cover `127.0.0.0/8` and `::1`. The 169.254.x.x (link-local/GCE metadata) range is not loopback and
is rejected.

All original brief rejection cases are covered by `internal/cli/baseurl_test.go`:
`http://evil.example.com`, `http://169.254.169.254`, `ftp://`, missing scheme, bare string, and
`https://` without a host. Acceptance cases include `https://` anywhere, `http://127.0.0.1:9999`,
`http://localhost:9999`, `http://[::1]:9999`. `TestBaseURLOverrideUsed` verifies that a valid
loopback override actually routes requests, so the test exercises the full code path.

**DNS-rebinding concern specifically addressed:** DNS rebinding is impossible here because the
validation compares the hostname string directly against `"localhost"`, without calling a resolver.
An attacker cannot arrange for a non-loopback hostname to pass validation by setting up DNS rebinding,
because the validation happens before any network access and does not rely on DNS.

### C1-SEC-2 / MEDIUM-1 — Redirect policy, including streaming re-attach path (Medium → Closed)

**Status: Closed. Streaming path is covered.**

`refuseCrossHostRedirects` is applied in `interactions.New()` via a shallow copy of the HTTP client
before any option is applied:

```go
hc := *c.httpClient
hc.CheckRedirect = refuseCrossHostRedirects
c.httpClient = &hc
```

`Stream()` calls `c.do(ctx, http.MethodGet, path, query, nil, "text/event-stream")`, which calls
`c.httpClient.Do(req)` — the same client carrying the redirect policy. The streaming re-attach path
(reconnect with `last_event_id`) calls `c.poll.Stream(ctx, id, lastEventID)` in `awaitStream`, and
`poll` is constructed through the same `newClients(opts)` path, so it also carries the policy.

`TestCrossHostRedirectRefused` pins that `x-goog-api-key` never reaches the redirect target.
`TestSameHostRedirectFollowed` and `TestRedirectLoopRejected` confirm the policy permits same-host
hops (capped at three) and refuses an infinite same-host loop.

### All other cycle-1 findings

All remaining cycle-1 findings are confirmed closed, matching the dispositions in
`docs/reviews/cycle-1-remediation.md`. Cross-checks performed:

| Finding | Verification method |
|---|---|
| C1-CODE-1 (input bound) | Not re-read; accepted by remediation report — `grep 'os\.ReadFile'` passes per the report's acceptance criterion |
| C1-CODE-2 (inFlight CAS guard) | Not re-read; remediation report cites `-race` pass |
| C1-SSE-1 (SSE accumulation) | Code inspected — `stream.go:191–193` checks `int64(len(data)) > s.maxEventBytes` after each `data:` line append |
| C1-SEC-2 (redirect policy) | Covered above |
| C1-CODE-3 (formatter escaping) | Not re-read; remediation report cites golden fixtures |
| C1-CODE-4 (stdinIsTerminal) | Code inspected — `commands.go:201–215` defines `stdinIsTerminal` as distinct from `stdinIsPiped`; `TestResearchPlanNonInteractiveNeedsAcceptPlan` pins it |
| C1-SEC-4 (file permissions) | Not re-read; remediation report cites mode assertions in sink test |
| C1-SEC-3 (CI SHA pinning) | CI file inspected — all three actions at full commit SHAs with version comments |
| C1-SEC-5 (secret file perm warn) | Not re-read; remediation report cites warn/no-warn tests |
| C1-SEC-6 (error scrubbing) | Not re-read; remediation report cites implementation |
| C1-CODE-6, C1-CODE-7, C1-CODE-8, C1-SPEC-1, C1-CODE-5, C1-SPEC-2, C1-SEC-7, C1-SPEC-3 | Accepted per remediation report; out of scope for this review's new-surface focus |
| C1-M5-1, C1-M5-2 (deferred M5) | Addressed in Part 2 below |

---

## Part 2 — New-Surface Review

### Finding: LOW-1 — Thought-summary text bypasses the scrubber before stderr emission

**File:** `internal/cli/research.go:234–249`

**Description.**
`bindThoughtDisplay` routes streamed thought summaries onto the run's transport (stderr as NDJSON)
without passing the content through `secret.Scrub()`:

```go
opts.OnThought = func(text string) {
    payload, err := json.Marshal(deltaPayload{Type: "thought_summary", Text: text})
    if err != nil {
        return
    }
    _ = tr.Emit(ctx, transport.Event{Kind: transport.KindDelta, Payload: payload})
}
```

The `text` string is the raw `ev.Delta.Text` field from the Gemini API's SSE `step.delta` event.
`tr.Emit()` writes directly to `cmd.ErrOrStderr()` as NDJSON. The `ScrubHandler` is a slog wrapper;
it does not intercept transport writes. The final error-message scrubbing (C1-SEC-6) also does not
cover this path.

**Attack scenario / risk.**

The Gemini API key itself (`AIza...`) will not appear in thought summaries: the key is sent only in
the `x-goog-api-key` request header, never in the request body, and the API does not echo request
headers in response bodies. The four scrubber patterns (`reGoogleKey`, `reGoogHeader`, `reBearer`,
`reCandidate`) would therefore have nothing to match even if they ran.

The plausible risk is via `--input`: when local files are passed as multimodal grounding, the API
may summarise or quote their content in thought deltas. If such a file contains credentials that do
not match `reGoogleKey` or `reBearer` (e.g., a service-account JSON, a database password in a
config file, or an SSH private key), those credentials could appear verbatim in thought summaries
and be written to stderr without scrubbing. An operator monitoring stderr, piping stderr to a
centralised log collector, or running in a shared shell session would expose that content.

This is the same structural gap identified in INFO-1 (cycle 1) for API error messages; the
difference is that thought summaries can be substantially larger and may contain arbitrary content
drawn from user-supplied inputs.

**Remediation.**
Apply `secret.Scrub()` to the thought text before marshalling:

```go
opts.OnThought = func(text string) {
    payload, err := json.Marshal(deltaPayload{
        Type: "thought_summary",
        Text: secret.Scrub(text),
    })
    if err != nil {
        return
    }
    _ = tr.Emit(ctx, transport.Event{Kind: transport.KindDelta, Payload: payload})
}
```

This adds one call per thought delta on the streaming path — negligible overhead — and extends the
structural scrubbing guarantee to cover all string content written to stderr.

**CWE:** CWE-209 (Generation of Error Message Containing Sensitive Information)

---

### Surfaces checked and found sound (new M4/M5 additions)

**`research-config` JSON output.**
`config.EncodeJSON()` serialises the `ResearchConfig` struct unchanged. The `api_key_ref` field
carries a `secret://` reference (enforced by `Validate()` — any literal key is rejected before
`EncodeJSON` is reached). The resolved API key value is never stored in `ResearchConfig`; it is
resolved later in `withRunSeams` and held only in the `apiKey` local variable. `MCP map[string]string`
emits the configured URLs verbatim — if an operator embeds basic-auth credentials in an MCP URL,
those credentials appear in the JSON output. This is expected serialisation behaviour (the config
reflects what was configured), not a code flaw. There are no separate MCP `Authorization` header
fields in the config struct. `TestResearchConfigEmitsResolvedJSON` asserts `api_key_ref` carries
the reference, not the key.

**Follow-up model override (`--model`).**
The `--model` flag populates `cfg.Model` (a plain string). In `runFollowUp`, this value is passed
to `gemini.NewFollowUp(opts, model)` and ultimately placed in `CreateRequest.Model`. Go's
`encoding/json.Marshal` correctly encodes any string value as a JSON string — all special characters
are escaped — so there is no JSON injection risk. The model ID reaches the Gemini API only as a
JSON string field; it does not influence local file operations, command execution, or any other
privilege-sensitive path. `config.Validate()` does not validate the `Model` field, but the absence
of validation is not a local security concern: the API will reject an unknown model ID. The model
value is also emitted in the transport as part of the cost summary; it is not a secret and does not
need scrubbing.

**Planner stdin handling.**
`reviewPlan()` (`research.go:310–332`) requires either a terminal stdin or `--accept-plan` before
constructing the planner:

```go
if !stdinIsTerminal(cmd.InOrStdin()) && !cfg.AcceptPlan {
    return "", errors.New("research --plan: stdin is not a terminal...")
}
```

`stdinIsTerminal()` (`commands.go:201–215`) requires stdin to be an `*os.File` with the
`ModeCharDevice` bit set — an `io.Reader` of any other type (pipe, `bytes.Buffer`, injected reader)
returns false, correctly preventing a non-terminal from approving spend. User feedback typed at the
prompt is read by `bufio.Reader.ReadString('\n')`. The feedback is sent to the Gemini API as the
content of a new `CreateRequest.Input`; `json.Marshal` encodes it correctly. `DefaultMaxRounds = 5`
bounds the refinement loop; exceeding it withdraws the refine option. `TestResearchPlanNonInteractiveNeedsAcceptPlan`
pins that a non-terminal stdin aborts before any request is sent.

**SSE reconnect URL construction (`last_event_id`).**
In `interactions/stream.go:102–133`, `lastEventID` is stored via `query.Set("last_event_id",
lastEventID)` and encoded by `query.Encode()`. `url.Values.Encode()` calls `url.QueryEscape` on all
values, correctly percent-encoding special characters. The `lastEventID` value comes from `id:` lines
in the SSE stream after the NUL guard (`!strings.ContainsRune(value, 0)` — per the SSE spec, IDs
containing NUL are ignored). The base URL is validated at startup; `lastEventID` cannot redirect
requests to a different host. There is no SSRF risk from `lastEventID` manipulation.

**SSE error events.**
`EventError` events return `fmt.Errorf("interactions: stream error event: %w", ev.Err)` from
`consumeStream`. This error propagates through `awaitStream` → `Await` → `run.Run` → `concludeRun`
→ `root.Execute()`. At `root.Execute()` the final error string is run through `secret.Scrub()` before
being printed to stderr (C1-SEC-6, confirmed remediated). API error messages in `ev.Err.Message`
originate from the Gemini API; they do not contain the API key (which is a request header only).

**SSE accumulation bound (C1-M5-1/C1-M5-2 deferred items).**
`stream.go:191–193` checks total accumulated payload against `s.maxEventBytes` after each `data:`
line is appended. Individual lines are bounded by the scanner buffer (`maxEventBytes`). The combined
bound is correct: if a second 16 MiB line is appended to an already-16 MiB accumulation, the check
triggers on that append and the error is returned before further allocation. This closes the original
MEDIUM-2 (C1-SSE-1) as verified.

**Test fakes.**
`newInteractionsServer`, `sseStatus`, `newPlanFlowServer` in `research_test.go` and `levers_test.go`
are standard `httptest.Server` patterns. The `finalStatus` and `id` values injected into SSE frames
via string concatenation are hardcoded test constants, not user input. No unsafe patterns are
present that would be dangerous if copied to production code.

**go.mod and dependency surface.**
No new direct dependencies have been added since cycle 1. The dependency set remains: stdlib,
`cobra v1.10.2`, `pflag v1.0.9`, `yaml.v3 v3.0.1`, and the OTel SDK (`v1.44.0`). No CVEs found in
the pinned versions. `go.sum` is present.

**CI workflow.**
All three actions are pinned to full commit SHAs:
`actions/checkout@df4cb1c069e1874edd31b4311f1884172cec0e10 # v6.0.3`,
`actions/setup-go@4a3601121dd01d1626a1e23e37211e3254c1c06c # v6.4.0`,
`golangci/golangci-lint-action@82606bf257cbaff209d206a39f5134f0cfbfd2ee # v9.2.1`.
The workflow `permissions` block is minimal (`contents: read`). No secrets are referenced in
environment variables. Sound.

---

## Verdict

The codebase is in a sound state for v1 release. All cycle-1 findings are closed; the single new
finding (LOW-1) is a one-line fix with negligible runtime cost. The new M4/M5 surfaces — the planner
gate, budget enforcement, `research-config` emission, follow-up model wiring, SSE reconnect, and
thought-summary display — are correctly implemented. The credential scrubber's structural guarantee
remains strong across all logging paths; only the transport's thought-summary path sits outside it.

Priority before merging: fix LOW-1 (`secret.Scrub(text)` in `bindThoughtDisplay`).
