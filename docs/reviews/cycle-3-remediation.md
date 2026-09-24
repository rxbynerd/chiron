# Remediation Report — Chiron Cycle 3 (v2 research agent, PR #1)

**Brief:** `docs/reviews/cycle-3-brief.md` (reviewer reports in `docs/reviews/cycle-3/`, committed at `cb95817`)
**Remediated on:** `feat/v2-research-agent`, starting from `cb95817`
**Date:** 2026-09-23

Every finding in the brief has a disposition below. The remediation ran as
three file-disjoint streams: the `fetch` package (`a4ba527`..`00c0f3f`),
the `model` and `search` packages (`654b6de`..`62efe6d`), and the worker
loop, config, CLI and documents (`7a0c2cb`, `4c7f3b0`, `b74f62f`,
`9c046ad`, `ad267b0`, `740c0d1`). `go build`, `go vet`,
`go test -race ./...` and `golangci-lint run ./...` are clean at the
commit introducing this report. Decisions are recorded in
`docs/DECISIONS.md` under the four 2026-09-23 entries.

## Dispositions

| Finding | Severity | Disposition |
| --- | --- | --- |
| C3-01 | Critical | **Fixed** @ `b74f62f`. CLAUDE.md describes the in-process worker as shipped; AGENTS.md gains rows for `fleet`, `fleet/search`, `fleet/fetch` and the fleet config and loopback switch; README documents `--agent worker`, the fleet flags, the money rules and the `wkr_` refusal. |
| C3-02 | High | **Fixed** @ `7a0c2cb` + `4c7f3b0`. Page text is reduced and bounded by `fleet.max_page_bytes` (64 KiB default); `fleet.max_tokens` defaults to 400 000 and `worker_timeout` to 5m. The ceiling is enforced through new `price_input_gbp_per_mtok`/`price_output_gbp_per_mtok` fields rather than rejected (a ceiling without prices is rejected). The Gemini-only levers are refused for `worker`/`fleet` at validation. |
| C3-03 | High | **Fixed** @ `7a0c2cb`. Start detaches cancellation but keeps the deadline; Await cancels the run when its context ends; deadline, cancellation and `length` stops map to `incomplete`. Tests cover a timeout mid-turn, `length`, and Await cancellation stopping further paid turns. |
| C3-04 | High | **Fixed** @ `a4ba527`..`00c0f3f`. Dial-time IP pinning with no proxy, TLS carried through the guarded dialer, CGNAT/NAT64/6to4/link-local/metadata/`fec0::/10` refused, context-aware lookups, redirect re-validation, a scripted resolver in tests. `New` refuses transports that would bypass the guard. |
| C3-05 | High | **Fixed** @ `7a0c2cb` + `ad267b0`. Span attributes are set before `span.End`. The double count is resolved by moving per-worker spend onto the worker span as attributes; only the run core emits the run-level metrics. A JSONL trace test asserts the attributes, that no metric originates from the worker, and that a provider error echoing the key never reaches the trace. |
| C3-06 | High | **Fixed** @ `7a0c2cb`, `654b6de`, `20177c5`, `9c046ad`. The action schema lists every property in `required` with nullable non-discriminator fields; the wire field is `max_completion_tokens`; the model fake rejects a non-strict schema with 400; `TestActionSchemaIsStrictCompliant` pins the schema to `model.ValidateStrictSchema`. |
| C3-07 | High | **Fixed** @ `b74f62f`. Dead `§0a`/`§5.3`/`§5.4`/`§5.5`/`§10`/`§2–§11` references in V2-PLAN and STIRRUP-ENGINE-PROPOSAL repointed at the 2026-07-01 sections or at V2-AMENDS amend 8; Wave 5 no longer says workers complete over the stirrup stream. |
| C3-08 | High | **Fixed** @ `7a0c2cb`. Fetch and search failures are fed back as user turns; three consecutive failures end the run `failed`; a success resets the count. Confirmed as the intended behaviour and recorded in DECISIONS. |
| C3-09 | High | **Fixed** @ `4c7f3b0`. `CHIRON_FETCH_ALLOW_LOOPBACK=1` (exactly) lets the worker's fetch reach loopback for the CLI e2e test; any other value is a startup error. `TestWorkerSearchFetchFinalThroughCLI` completes search, fetch and final through the shipped binary against httptest doubles. |
| C3-10 | Medium | **Fixed** @ `7a0c2cb`, `9c046ad`, `740c0d1`. Tool results are fenced and the system prompt declares them data; retrieved strings are defanged so an embedded delimiter cannot close the fence (a fixed delimiter plus defanging, not a per-run nonce, for deterministic goldens); final citations are restricted to seen URLs with the dropped count on the span; the answer has images rewritten to links, HTML removed and `secret.Scrub` applied. |
| C3-11 | Medium | **Fixed** @ `7a0c2cb`. A citation is recorded when a page is fetched, so `incomplete` and `failed` Findings carry the sources read so far. |
| C3-12 | Medium | **Fixed** @ `4c7f3b0`, `9c046ad`, `b74f62f`. Model-name help says required; `--agent` and README describe `worker` and mark `fleet` not yet available; the `research` long help describes the agents; `memory: inmemory` is rejected until a store exists; the CLI guard returns `fleet.ErrNotImplemented` (`errors.Is` holds). |
| C3-13 | Medium | **Fixed** @ `4c7f3b0`. `get` and `follow-up` refuse `wkr_` ids locally with an explanation; help text qualified. |
| C3-14 | Medium | **Fixed** @ `7a0c2cb`. `parseAction` rejects trailing content via the decoder, disallows unknown fields, surfaces refusals; a 14-case malformed-output table. |
| C3-15 | Medium | **Fixed** @ `7a0c2cb` + `4c7f3b0`. No `Wave N` narrative remains in non-test Go; the user-facing error names `--agent worker`. |
| C3-16 | Medium | **Fixed** in the commit introducing this report. The DECISIONS entry records the wave-order exception and the previously missing Wave 3 decisions. |
| C3-17 | Medium | **Follow-up issue.** Choosing between provenance, binding and namespacing changes the config UX and is the owner's call. Mitigated in-PR @ `9c046ad` (endpoints must not carry userinfo, query or fragment; errors never echo the raw value) and by the existing https-only rule. |
| C3-18 | Medium | **Follow-up issue.** `Options.ToolName`/`QueryArgKey` exist with documented defaults; exposing them in config waits on the SP-A backend choice. |
| C3-19 | Medium | **Fixed** @ `7a0c2cb`. `worker_tools_test.go` asserts the transcript the model receives: fed-back errors, fenced page text, defanged delimiters, reduced HTML with the citation title. |
| C3-20 | Medium | **Fixed** @ `4c7f3b0`. `internal/cli/worker_test.go` covers the failure exit code, lever rejection, `wkr_` refusal, missing-field failure before any request, and the loopback switch. |
| C3-21 | Medium | **Fixed** @ `64270a8` + `c845b57`. SSE framing (multi-line data, server messages before the reply, bounded stream) and JSON-RPC error and mismatched-id tests in the `search` package. |
| C3-22 | Medium | **Fixed** @ `1faca20`, `821bcba`, `ad267b0`. Downgrade and cross-host redirect tests in both clients; `TestGenerateMalformedBodyIsError` covers non-JSON, no-choices and wrong-shape bodies. |
| C3-23 | Medium | **Fixed** @ `ad267b0`. Unknown ids error from Await and Result; Result on a live run reports `in_progress` without completion state. |
| C3-24 | Medium | **Fixed** @ `b74f62f`. Amends 1 and 3 marked superseded by amend 8. |
| C3-25 | Low | **Fixed (in-PR part)** @ `9c046ad` + `64270a8`. Server helpers are handler factories and each test owns its server; the SSE fake flushes; AGENTS.md records the `FakeServer` carve-out. Moving the fakes out of the binary is a follow-up issue. |
| C3-26 | Low | **Fixed** @ `1faca20` + `821bcba`. https-to-http redirects refused by both credential-bearing clients, with tests that count accepted connections. |
| C3-27 | Low | **Fixed (in-PR part)** @ `c845b57`. `MCP-Protocol-Version` sent after initialize, reply ids matched, server-request frames skipped, sessions ended with a best-effort DELETE. Version enforcement and session reuse are a follow-up issue. |
| C3-28 | Low | **Fixed** @ `b74f62f`, `62efe6d`, `4c7f3b0`, and the DECISIONS entry in this commit, which supersedes the stale 2026-07-01 statements about `EstimatedCostGBP` and the untyped fleet guard. |
| C3-29 | Low | **Fixed** @ `35e5b1a`, `4c7f3b0`, `7a0c2cb`. Deadline tests observe cancellation (0.1s instead of 2s); the flag-isolation test resets the whole `Fleet` block; the vacuous citation test became `TestRunWorkerFinalCitationsRestrictedToSeenURLs`. |
| C3-30 | Low | **Fixed** @ `72fc173`, `821bcba`, `9c046ad`. Clients reject userinfo without echoing it; config additionally rejects query and fragment and describes endpoints by scheme and host. |
| C3-31 | Low | **Fixed** @ `7a0c2cb` + `ad267b0`. `Detail` and the `objective` span attribute are bounded at 2 KiB on a rune boundary. |
| C3-32 | Low | **Fixed** @ `9c046ad`. `CompletedAt` is the loop's end. A cap stop exits 2 (`incomplete`), as the exit-code help already states; recorded in DECISIONS. |
| C3-33 | Low | **Fixed** @ `7a0c2cb`. All `parseAction` errors carry the `fleet:` prefix, asserted in the table test. |
| C3-34 | Low | **Follow-up issue.** Per-turn progress events need a transport event or a reuse of `delta`; deferred with the observability work. |
| C3-35 | Low | **Fixed** @ `b74f62f`. The V2-RESEARCH-AGENT §2 fleet row records that Wave 3 landed and what remains. |

## Notes

1. **C3-02 ceiling.** The brief proposed rejecting a non-zero
   `ceiling_gbp` until a rate table existed. Price fields make the ceiling
   enforceable now without a table in the binary, so the follow-up item
   for a per-model rate table is dropped; a curated table can layer on
   later as defaults for the two fields.
2. **C3-10 fence.** A per-run nonce was considered. Defanging `<<<` in
   every retrieved string gives the same guarantee (no retrieved content
   can produce a delimiter) while keeping prompts deterministic for the
   golden report.
3. **C3-17.** Left to the owner because the three options differ in what
   a pipeline may put in a base config. The issue carries a
   recommendation for option (a).
4. **Worktree bases.** Both implementer worktrees were cut from `main`
   rather than the PR branch; each was reset to `cb95817` before work and
   cherry-picked onto the branch afterwards, with one append-only
   conflict in DECISIONS.md resolved by keeping both entries.
