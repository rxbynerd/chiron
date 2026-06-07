# Remediation Report — Chiron Cycle 2 (Final v1)

**Brief:** `docs/reviews/cycle-2-brief.md` (HEAD `90fe191` at review time; brief committed at `a2526c2`)
**Remediated on:** `main`, starting from `a2526c2`
**Date:** 2026-06-07

Every finding in the brief has a disposition below. This cycle was
predominantly test-writing: production code arrived with no
Critical/High/Medium code findings, and the priority spine was
coverage of stream termination, failure details and config surfaces.

## Dispositions

| Finding | Severity | Disposition |
| --- | --- | --- |
| C2-TEST-1 | Critical | **Fixed** @ `9d04305`. Three named tests pin the `interaction.completed` (embedded and bare payload), requires_action-via-completed, and already-terminal `interaction.created` paths; all assert the poll fallback is unreached and pass `-race`. `eventStatus` 0% → 40%, `consumeStream` → 92.9%. |
| C2-TEST-2 | High | **Fixed** @ `9d04305`. `TestFailedInteractionCarriesDetail` is a five-case table pinning status-specific wording (and absence of the `failed` wording elsewhere); `statusDetail` → 100%. |
| C2-TEST-3 | High | **Fixed** @ `9d04305`. Both named tests added. The suspected non-transparent wrap **does not exist**: `planner.go` already wraps with `%w`, and `TestPlannerRoundRequiresAction` proves `errors.Is(err, ErrRequiresAction)` through the wrap — no code fix was required. `round` 80% → 93.3%. |
| C2-TEST-4 | High | **Fixed** @ `2171b15`. `TestLoadBaseExplicitFile` asserts the file's value reaches the resolved JSON; missing-file fails before any seam with zero requests; a malformed-YAML case added beyond the brief. |
| C2-TEST-5 | High | **Fixed** @ `c03c081`. Native `json.Unmarshal` round-trip of `EncodeJSON` output (Timeout preserved); a forms test covers string/nanosecond/rejection branches. `UnmarshalJSON` 0% → 75%, `set` 45.5% → 81.8%. No bugs were exposed — the existing code parses both forms correctly. |
| C2-CONS-1 | High | **Fixed** @ `92550b1`. Both prefixes now `"gemini:"`; grep over `internal/researcher/` finds no `"interactions:"` prefix. |
| C2-TEST-6 | Medium | **Fixed** @ `9d04305`. `TestAwaitStreamCancelledDuringBackoff`: cancellation mid-backoff propagates `context.Canceled` (asserted with `errors.Is`), zero poll-fallback GETs. |
| C2-TEST-7 | Medium | **Fixed** @ `9d04305`. `TestInputPartMIMETypes` table: `.jpg`/`.jpeg` → `image/jpeg`, `.gif`, `.txt`, and a binary `.bin` sniffed to `application/octet-stream`, all asserted in the decoded create body. `mimeFromName` 33.3% → 53.3%. |
| C2-TEST-8 | Medium | **Fixed** @ `991fe71`. `TestExtensionFor` covers JPEG aliases, GIF, WebP, normalisation, and the `.img` fallback; `extensionFor` → 100%. |
| C2-TEST-9 | Medium | **Fixed** @ `c03c081`. Seven flags exercised in isolation with a defaults-equality check against unrelated-field drift; `ApplyFlags` 58.1% → 83.9%. Note: the brief's `OutPath` field is actually named `Out`. |
| C2-TEST-10 | Medium | **Fixed** @ `6eb0582`. `TestRunNilInteractionFromResearcher` pins the `nil, nil` seam guard via the established fake; no `recover()`. |
| C2-CONS-2 | Medium | **Fixed** @ `9d04305`/`2171b15`. All `tc` loops renamed `tt` — the three the brief named plus `baseurl_test.go` (cycle-1 vintage), so the acceptance grep over both packages is clean. |
| C2-CONS-3 | Medium | **Fixed** in the commit introducing this report. AGENTS.md gains a test-infrastructure section recording the caller-managed `httptest.Server` lifecycle convention. |
| C2-SEC-1 | Low | **Fixed** @ `2171b15`. Thought deltas pass through `secret.Scrub`; a test plants a Google-API-key-shaped string in a thought delta and asserts the redaction marker on stderr. |
| C2-SPEC-1 | Low | **Fixed** @ `2171b15`. `reviewPlan` wraps `Session.Run` in a `SpanPlan` with the accepted plan's `interaction_id` attribute. Deviation note: the span is its own trace root, not a child of the `research` span — planning necessarily precedes `run.Run`, which opens that root; making it a child would mean restructuring the run core for an observability nicety. |
| C2-CODE-4 | Low | **Fixed** @ `92550b1` + `2171b15`. `Options.OnStreamDegraded` fires once when the failure budget is spent (adapter test pins exactly-once); the CLI binds it to a best-effort `delta` event with `"type":"stream_degraded"` — reusing `KindDelta` keeps the transport kinds mirroring `proto/chiron/v1` one-to-one (DECISIONS.md) without a proto change. |
| C2-CODE-3 | Low | **Fixed** @ `c03c081`. `config.Validate` rejects non-http(s) MCP URLs with the server name in the message; three rejection cases added to `TestValidate`. Per the brief's action, the adapter-side `assembleTools` check is unchanged. |
| C2-CODE-1 | Low | **Fixed** @ `92550b1`. `n > 30` pre-shift cap plus the wrapping comment, matching `client.go`. |
| C2-CODE-2 | Low | **Fixed** @ `92550b1` + `9d04305`. `round()` returns a partial `Plan` carrying the paid round's id on every post-create error path (contract documented on the `Planner` seam); `Session.Run` prints the `chiron get` recovery hint. Pinned at both adapter and session level. |
| C2-CONS-4 | Low | **Fixed** @ `6eb0582`. `SetAttr("resumed", true)`. |
| C2-TEST-14 | Low | **Fixed** @ `2171b15`. A shared `flush` helper after every SSE write in `newInteractionsServer` and `sseStatus`, matching gemini's `sseWrite`. |
| C2-CONS-6 | Low | **Fixed** in the commit introducing this report. AGENTS.md note: new SSE tests use an `sseWrite`-style helper; existing interactions tests left as-is per the brief. |
| C2-TEST-11 | Low | **Fixed** @ `6be65d1`. Oversize non-JSON error body clipped at the 256-byte snippet bound (the brief said 200; the code's bound is 256), head-anchored, truncation-marked. |
| C2-TEST-12 | Low | **Fixed** @ `c03c081`. `MarshalYAML` pinned to the string form. |
| C2-TEST-13 | Low | **Fixed** @ `6be65d1`. `WithHTTPClient` proven via a tagging `RoundTripper`; `WithRequestTimeout` pinned to `context.DeadlineExceeded` against a stalled server. |
| C2-CONS-5 | Low | **Fixed** @ `9d04305`. Four inline create-body decodes in `gemini_test.go` routed through `decodeJSONBody`; the now-unused `encoding/json` import dropped. |
| C2-CONS-7 | Low | **Fixed** @ `991fe71`. The duplicate package doc now lives on the `Markdown` type; `go doc` shows one package doc. |

## Notes

1. **C2-TEST-3 verdict on the suspected code bug.** The synthesizer
   flagged a possible non-transparent error wrap at `planner.go:133`.
   The wrap was already `%w` at remediation HEAD; the new test confirms
   the typed-error contract holds end to end. Disposition is
   test-added, no code change — the reviewer's assertion is recorded
   as refuted by the test.
2. **Small factual corrections to the brief**, verified against
   current code: the snippet bound is 256 bytes, not 200 (C2-TEST-11);
   the config field is `Out`, not `OutPath` (C2-TEST-9);
   `mimeFromName` returns `""` for unknown extensions and the
   `application/octet-stream` arrives from `http.DetectContentType`'s
   sniff of local-file content (C2-TEST-7) — the tests assert the
   user-visible behaviour the brief intended.
3. **C2-SPEC-1 span topology.** `SpanPlan` is emitted as its own trace
   root rather than a child of `research`, because the plan phase
   precedes `run.Run`, which opens the research root. The phase is now
   visible to backends, which is the finding's intent.

## Verification

- `go build ./...`, `go vet ./...`, `go test -count=1 ./...` pass at
  the final commit; `go test -race -count=1` passes on
  `internal/researcher/gemini` and `internal/cli` (the packages with
  concurrent test additions).
- Coverage acceptance: every named threshold met (figures in the
  table above).
- `grep -rn '"interactions:' internal/researcher/` and
  `grep -rn '\bfor _, tc :=' internal/researcher/gemini/ internal/cli/`
  both return nothing.
