# Cycle 3 runtime verification — `chiron research --agent worker`

Method: an in-process Go test harness (`internal/cli/zz_e2e_scratch_test.go`,
package `cli`, deleted before finishing — `git status` is clean apart from
`docs/reviews/cycle-3/`) driving the real command tree via `NewRootCommand()`
+ `cmd.Execute()` (the same helper the package's own `execute()` uses),
against the exported `model.FakeServer` and `search.FakeServer` fakes plus
plain `httptest.Server`s. No real network was touched. Each scenario below
was actually run; transcripts are the literal `go test -v` output.

## Summary verdict

**Partially verified.** Everything reachable through local fakes behaves as
documented — closed action vocabulary, cap enforcement, exit codes, event
stream, budget non-interference — with one exception (C3-VERIFY-2, unknown
action leaking into a documented-safe message check, which turned out to be
a test-assertion mistake on my part, not a product bug, see below). The one
behaviour I could **not** verify is the specific happy path the brief asked
for first — "search -> fetch -> finish" — because the shipped composition
root makes a successful `web_fetch` impossible to reach with any local test
double (C3-VERIFY-1, High).

## Checklist of observable behaviours

### C3-VERIFY-1 — happy path "search -> fetch -> finish" cannot be exercised through the real binary against any local double

**Severity:** High
**File:** internal/cli/research.go:208-214

**Status: unverified as end-to-end CLI behaviour (High-confidence discrepancy); verified instead at the package level with a deliberately non-production configuration.**

`buildWorker` hardcodes the fetch client's SSRF allowance:

```go
fetchClient, err := fetch.New(fetch.Options{
    RequestTimeout: callTimeout,
    AllowLoopback:  false,
})
```

`AllowLoopback` has no flag, config field, or environment override anywhere
in the codebase (confirmed by `grep -rn AllowLoopback internal cmd`, only
this call site and the fetch package's own definition/tests exist). Every
`httptest.Server` binds to `127.0.0.1`, which `fetch.guardHost` always
refuses when `AllowLoopback` is false — the same refusal a production
deployment gives for a real internal address. Concretely, scripting a
search result that points at a local `httptest.Server` page and asking the
model to fetch it:

```
=== RUN   TestVerifyWorkerFetchAgainstLoopbackIsRefused
    status_detail: 'fetch failed: fetch: refusing to fetch internal address 127.0.0.1'
    ...
    err=research failed: interaction ... ended failed
--- PASS
```

The run always ends `failed`, never `completed`. This means:

- I cannot verify the report/citation/formatter path for a *real* fetched
  page through `chiron research --agent worker` in this (or any offline
  CI/eval) environment — only through the fleet package's own tests, which
  construct `fetch.Client` directly with `AllowLoopback: true`
  (`internal/researcher/fleet/worker_testservers_test.go`, not the CLI
  composition root) and are therefore not proof that the *shipped* wiring
  can ever complete a fetch without live internet.
- Every eval harness or CI environment without outbound internet access
  will see `--agent worker` fail on the first real research question that
  needs a source, unlike `deep-research`/`deep-research-max` and the
  model/search legs of the worker (which both have `CHIRON_GEMINI_BASE_URL`-
  style loopback escape hatches via `fleet.model_endpoint`/
  `fleet.search_endpoint`).
- I fell back to verifying the loop's fetch *mechanics* are otherwise
  correct by running the package's own `TestWorkerGoldenReportThroughRunCore`
  (`internal/researcher/fleet/worker_golden_test.go`), which passes and does
  produce search→fetch→finish with citations — but that test uses
  `AllowLoopback: true`, a configuration the CLI never produces.

**Fix:** either accept this as a documented, deliberate limitation (fetch
truly can only ever reach the live internet, by design, and the golden test
is the intended proof), or add a narrowly-scoped, clearly-labelled
loopback-allowance path for the worker's fetch client analogous to
`CHIRON_GEMINI_BASE_URL` (env-gated, not a flag, so it can't land in a
committed config), so `--agent worker` is smoke-testable end-to-end without
live network.

**Acceptance:** state explicitly in AGENTS.md / V2-RESEARCH-AGENT.md that
`--agent worker`'s fetch step cannot be run against a local double through
the compiled CLI, and that `TestWorkerGoldenReportThroughRunCore` is the
closest available substitute — or land the override and add a genuine CLI
e2e test that completes a fetch through a loopback page.

---

### Verified checklist (pass unless noted)

1. **Search → fetch → finish produces a numbered-source Markdown report.**
   Verified at the fleet-package level only (`TestWorkerGoldenReportThroughRunCore`,
   passing); **not verified** through the CLI composition root — see
   C3-VERIFY-1.

2. **JSON sink output (`-o json`).** PASS. `chiron research --agent worker
   -o json` emits the `RunResult` envelope; the citation is present once the
   base64 `report.markdown` field is decoded (the base64 encoding is
   pre-existing, documented `sink/json.go` behaviour — `[]byte` via
   `encoding/json` — not something this PR changed, so not flagged as a
   finding).

   ```
   stdout={"interaction_id":"wkr_...","status":"completed","report":{"markdown":"<base64>"},"duration_ns":608583}
   ```
   Decoded markdown contains `https://example.org/a` and the `## Sources`
   section.

3. **Garbage / non-JSON model output.** PASS. A model reply of
   `this is not json at all` ends the run `failed` on the first turn, with
   no retry (`modelSrv.CallCount() == 1`), and the parse error surfaces in
   `status_detail`:
   ```
   status_detail: 'invalid model action: model action is not valid JSON for the
   action schema: invalid character ''h'' in literal true (expecting ''r'')'
   ```

4. **Unknown/side-effecting action (`"action":"shell"`).** PASS. Refused
   before dispatch, run ends `failed`, `status_detail` names the rejection:
   ```
   status_detail: 'invalid model action: model requested unknown action "shell":
   only search, fetch and final exist'
   ```
   No execution occurred (there is no dispatch branch for it — confirmed by
   reading `worker.go`'s closed `switch`). Note: the *process-level* error
   string returned to `main` (`research failed: interaction ... ended failed`)
   deliberately does **not** repeat this detail — it lives in the report and
   the `status_changed`/NDJSON event instead. My first draft of this test
   asserted on the wrong string (process error vs. report body) and failed;
   that was my test bug, not a product defect, and is fixed in the final
   harness. Flagging here only so the synthesiser doesn't mistake it for a
   finding if it shows up in scratch history.

5. **Loop past `--fleet-max-turns`.** PASS. With `--fleet-max-turns 1` and a
   model that always replies with a `search` action, exactly one model call
   is made (`modelSrv.CallCount() == 1`) and the run ends `incomplete`:
   ```
   status_detail: reached the 1-turn cap before a final answer
   ```
   Exit code is `2` (`ExitResearchFailed` — `incomplete` and `failed` share
   the same exit code per `exitForStatus`'s `default` branch). This matches
   the documented contract in `internal/cli/commands.go`'s help text ("2 the
   research task ended failed or incomplete...").

6. **Search endpoint returns an MCP tool-level error (`isError:true`).**
   PASS. `search.NewFakeServer(nil, search.WithRawToolResult(...isError:true...))`
   causes `doSearch` to fail immediately; run ends `failed` with the tool's
   text folded in (and available for scrubbing, though this case had nothing
   credential-shaped to scrub):
   ```
   status_detail: 'search failed: search: tool reported an error: rate limited,
   try again later'
   ```

7. **Fetch target resolves to a private IP.** PASS. A search result of
   `http://10.1.2.3/secret`, with the model then requesting `fetch` on that
   URL, is refused synchronously (no dial attempted, confirmed by
   `guardHost`'s literal-IP branch) and the run ends `failed`:
   ```
   status_detail: 'fetch failed: fetch: refusing to fetch internal address 10.1.2.3'
   ```

8. **Non-loopback `http://` fleet endpoint rejected at validate.** PASS.
   `--fleet-model-endpoint http://example.com` fails at `cfg.Validate()`
   before any secret is resolved or request is made:
   ```
   err=invalid research config: fleet.model_endpoint: "http://example.com" must
   be an absolute https:// URL (http:// only for loopback test servers)
   ```
   Exit code `1` (`ExitUsage`), consistent with the Gemini-tier
   `CHIRON_GEMINI_BASE_URL` rule it mirrors.

9. **stderr progress events.** PASS. NDJSON lines for `run_started`,
   `interaction_created`, `status_changed`, `run_completed` and
   `cost_summary` all appear on stderr for a worker run, exactly mirroring
   the Gemini-tier event vocabulary; the `wkr_`-prefixed local interaction id
   is emitted immediately (before any model/search call), matching the
   money-safety "id emitted early" convention even though the worker path
   itself spends no metered Chiron-side money on `Start`.

10. **`--budget` for worker runs.** PASS (confirmed **ignored**, as
    documented in code comments but not in user-facing `--help`). Running
    with `--budget 0.0001` — an amount that would block every deep-research
    tier — against a worker script that completes normally still exits `0`
    with the full report on stdout. Nothing in `runWorkerResearch` /
    `buildWorker` reads `cfg.BudgetGBP`; the only spend cap available to a
    worker run is `--fleet-ceiling` (`fleet.ceiling_gbp`), a separate flag.
    See C3-VERIFY-2 below — this silent divergence is not documented in
    `--help`.

11. **`--agent fleet`.** PASS. Fails with `agent "fleet": not yet wired (v2
    Wave 4 in progress)` and exit code `1` (`ExitUsage`) — a plain error, not
    an `ExitError`, so it is correctly bucketed as infrastructure/usage
    rather than a research outcome, matching `checkResearcherWired`'s intent
    and the existing `TestFleetAgentNotYetWired` pin.

---

### C3-VERIFY-2 — `--budget` silently does nothing for `--agent worker`, and the command help does not say so

**Severity:** Medium
**File:** internal/cli/commands.go:32 (research command `Long` help text); internal/cli/research.go:132-145

The `research` command's help text states unconditionally:

> "With `--budget`, the run is blocked up front when the tier's estimated
> cost exceeds the cap — before any interaction is created."

This is only true for the Gemini-bound tiers. For `--agent worker`,
`runWorkerResearch` never calls `gateBudget`, and `--budget` has no effect
at all — confirmed above (item 10): a worker run with `--budget 0.0001`
still completes and spends. A user who reads `chiron research --help` and
sets `--budget` expecting it to protect a worker run (the natural reading,
since the flag is listed globally, not per-tier) gets no protection and no
warning. The actual spend-bounding knob for a worker run is
`--fleet-ceiling` / `fleet.ceiling_gbp`, an entirely different flag with a
different name and unit convention, discoverable only by reading
`AGENTS.md`/source.

**Fix:** either (a) document in the `research` command's `--help` that
`--budget` applies to the `deep-research*` tiers only and `--fleet-ceiling`
is the worker/fleet equivalent, or (b) make `--budget` also populate
`fleet.ceiling_gbp` when unset (least-surprise: one cost-cap flag that means
"cap the spend" regardless of agent), or (c) reject `--budget` with a clear
error when `--agent` is `worker`/`fleet` so silence is never mistaken for
protection.

**Acceptance:** running `chiron research --agent worker --budget X ...`
either caps the run at `X`, or fails fast with a message naming
`--fleet-ceiling` as the correct flag — it must not silently proceed
unbounded while the help text implies it is bounded.

---

## What I did not check

- **`--agent fleet` full behaviour** beyond the not-yet-wired guard — out of
  scope (Wave 4 is explicitly unimplemented).
- **Real internet fetch** — deliberately never attempted, per instructions.
- **OTel tracing output** (`OTEL_EXPORTER_OTLP_ENDPOINT` binding) — not
  exercised; only the no-op tracer path was driven.
- **`fleet.memory: inmemory`** binding — not exercised; only the default
  `noop` path was driven (the worker loop does not appear to touch
  `ContextStore` at all in this PR, consistent with AGENTS.md's "Wave 4/6"
  framing, but I did not specifically re-verify that by reading the wiring
  path beyond `buildWorker`, which never references `cfg.Fleet.Memory`).
- **`--fleet-max-tokens` / `--fleet-ceiling` enforcement** — not
  independently re-verified at the CLI layer (time-boxed out); these are
  covered by the fleet package's own unit tests, which I only ran, not
  re-derived.

## Commands run

```
go build ./...
go test ./internal/researcher/fleet/... ./internal/cli/... ./internal/config/...
go vet ./internal/cli/...
go test ./internal/cli/... -run 'TestVerify' -v
```

All green except the two self-inflicted assertion mistakes described in
item 4 above, which were fixed before the final run (final run: 12/12 pass).
