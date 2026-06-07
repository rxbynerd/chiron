# Remediation Report — Chiron Cycle 1

**Brief:** `docs/reviews/cycle-1-brief.md` (HEAD `dea3bf3` at review time)
**Remediated on:** `main`, starting from `f7ae65b` (post-M4)
**Date:** 2026-06-07

Every finding in the brief has a disposition below. The brief predates
M4 (task #7); each finding was re-verified against the current tree
before fixing, and two were partially or wholly resolved by M4 landing
— noted where so.

## Dispositions

| Finding | Severity | Disposition |
| --- | --- | --- |
| C1-SEC-1 | High | **Fixed** @ `775e509` (validation + tests) and `36685c3` (AGENTS.md). Deviation: http:// is admitted for loopback hosts, not refused outright — see note 1. |
| C1-CODE-1 | High | **Fixed** @ `df941a6`. Bounded 50 MiB read via `io.LimitReader`; boundary-inclusive and over-bound tests added; no `os.ReadFile` remains in `input.go`. |
| C1-CODE-2 | High | **Fixed** @ `df941a6`. `inFlight` CAS guard plus pollCount reset moved under the mutex; concurrent-Start and fresh-Start-reset tests added; package passes `-race`. |
| C1-SSE-1 | Medium | **Fixed** @ `845a852`. Accumulation bound across `data:` lines, checked before dispatch; test uses two 80-byte lines against a 128-byte bound. |
| C1-SEC-2 | Medium | **Fixed** @ `845a852`. Same-host redirect policy (three-hop cap) on a shallow copy of any supplied client; cross-host, same-host and loop tests added. |
| C1-CODE-3 | Medium | **Fixed** @ `ceb56b1`. `]` escaped in titles, `)` percent-encoded in URIs, non-http(s) schemes dropped at `collectSources`; three golden fixtures pin the cases. |
| C1-CODE-4 | Medium | **Fixed** @ `775e509`. Non-`*os.File` readers are not piped config; separate `stdinIsTerminal` keeps the spend-approval guard — see note 2. |
| C1-SEC-4 | Low | **Fixed** @ `ac7dafc`. Report and asset writes are `0o600`; modes asserted in the sink test. |
| C1-SEC-3 | Low | **Fixed** @ `0e54832`. Actions pinned to release-commit SHAs with version comments — see note 3. |
| C1-SEC-5 | Low | **Fixed** @ `ac7dafc`. `slog.Warn` when `mode.Perm()&0o177 != 0`; resolution is not blocked; warn/no-warn both tested. |
| C1-SEC-6 | Low | **Fixed** @ `775e509`. `root.Execute`'s stderr line routes through `secret.Scrub`. |
| C1-CODE-6 | Low | **Fixed** @ `a2c1272`. Counting tracer asserts `MetricFailures` emits exactly once on a sink failure. |
| C1-CODE-7 | Low | **Already satisfied** — no change. `go.mod` declares `go 1.26.4` and `newID` already carries the comment citing the Go 1.24+ never-fails guarantee for `crypto/rand.Read`, which is the documented origin of the guarantee (the brief's 1.20/1.21 figures predate the documentation). |
| C1-CODE-8 | Low | **Resolved by M4** (task #7, commit `9be3a2b`) — `toDomain` now maps `in.Model` when `in.Agent` is empty, and `TestFollowUpChainsModelInteraction` exercises the path. The proposed `TODO(M4)` comment is moot: the code is live, not pending. |
| C1-SPEC-1 | Low | **Fixed** @ `df941a6`. `TODO(M5)` marker at the unreachable `OutputThoughtSummary` constant. |
| C1-CODE-5 | Low | **Fixed** @ `845a852`. Comment records the defined-wrapping overflow mechanism; no logic change. |
| C1-SPEC-2 | Low | **Fixed** @ `36685c3`. `otel/trace` listed in the DECISIONS.md OTel entry with its use sites. |
| C1-SEC-7 | Info | **Documented** @ `36685c3`. OTLP endpoint variables recorded as security-sensitive in AGENTS.md alongside `CHIRON_GEMINI_BASE_URL`. |
| C1-SPEC-3 | Info | **Overtaken by events** — the M4 stubs the finding describes (`research-config`, `get`, `follow-up`, `--plan`, `--budget`) are now implemented (task #7). No action was required by the brief. |
| C1-M5-1 | Medium (deferred) | **Delegated-M5** per the brief's explicit instruction; tracked on task #8. Not touched here. |
| C1-M5-2 | Medium (deferred) | **Delegated-M5** per the brief's explicit instruction; tracked on task #8. Not touched here. |

## Notes

1. **C1-SEC-1 loopback deviation.** The brief's fix requires https://
   unconditionally. The CLI smoke tests drive the compiled command tree
   end to end against plain-HTTP `httptest` servers, and the adapter
   deliberately exposes no TLS-trust injection point, so https-only
   would have destroyed the end-to-end suite or forced a trust seam
   that exists only for tests. http:// restricted to loopback
   (`localhost`, `127.0.0.0/8`, `::1`) preserves every threat the
   finding names — cleartext on a network path, key exfiltration to a
   remote host, SSRF to link-local/internal addresses — while keeping
   the tests honest. All of the brief's named rejection cases
   (`http://evil.example.com`, missing scheme, `ftp://`, bare string)
   are covered in `internal/cli/baseurl_test.go`. Rationale recorded in
   DECISIONS.md.
2. **C1-CODE-4 terminal split.** Inverting `stdinIsPiped` alone would
   have let a non-terminal reader pass the `--plan` review gate
   (`reviewPlan` tested "piped" as a proxy for "not a terminal").
   The fix introduces `stdinIsTerminal` — not the negation of
   `stdinIsPiped` — so an unknown reader is neither an implicit config
   source nor a spend-approving terminal. The recorded decision that a
   non-terminal stdin can never approve spend is preserved, and
   `TestResearchPlanNonInteractiveNeedsAcceptPlan` still passes
   unchanged.
3. **C1-SEC-3 version note.** The brief recorded that `@v6` did not
   exist for `actions/checkout`/`actions/setup-go` at review time and
   suggested pinning v4/v5. By remediation time both v6 majors exist
   (checkout v6.0.3, setup-go v6.4.0; golangci-lint-action v9.2.1), so
   the pins are those releases' commit SHAs, resolved via the GitHub
   API, each with a `# vX.Y.Z` comment.

## Verification

- `go build ./...`, `go vet ./...`, `go test -count=1 ./...` pass at
  the final commit.
- `go test -race -count=1 ./internal/researcher/gemini/...` passes
  (C1-CODE-2 acceptance).
- `grep -rn 'os\.ReadFile' internal/researcher/gemini/input.go` is
  empty (C1-CODE-1 acceptance).
- Golden files regenerate cleanly with
  `go test ./internal/formatter/ -update` and pass pinned
  (C1-CODE-3 acceptance).
