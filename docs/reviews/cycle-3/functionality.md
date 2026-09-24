# Cycle 3 — Functionality review: v2 research-agent (`--agent worker`/`fleet`)

Reviewer: functionality-reviewer. Scope: `feat/v2-research-agent` (HEAD
`d4ac9f2`) vs `origin/main` (`926d7bf`). Method: built `./cmd/chiron` from
the branch, drove `chiron research` / `chiron research-config` directly,
and ran a scratch CLI-level test (`internal/cli`, removed before finishing;
`git status` confirmed clean) against the exported `model.FakeServer` /
`search.FakeServer` fakes to exercise the worker path end to end with no
real network traffic.

## Findings

### C3-FUNC-1 — The entire `--agent worker`/`fleet` surface is undocumented outside `--help` and `docs/`
**Severity:** High
**File:** README.md (whole file); `examples/researchconfig/` (no fleet example)
`README.md` is the project's own stated primary entry point ("Documents"
section points elsewhere only for deeper detail) and it has zero mentions
of `worker`, `fleet`, any `fleet-*` flag, or the `fleet` config block —
`grep -n "worker\|fleet\|OpenAI-compatible\|MCP" README.md` matches only
the pre-existing `--mcp name=url` row. The "Research flags" table, the
"Cost levers" table, and the "Environment variables" table all predate
this branch and were not extended. `examples/researchconfig/` still ships
only `comprehensive.json` and `default.yaml`, neither touching `fleet`.
The only documentation of the feature lives in `docs/V2-RESEARCH-AGENT.md`
(binding design doc, not operator-facing) and `AGENTS.md`'s
security-sensitive-config section (which documents the two endpoint/key
fields, not the full flag set or a runnable example). An operator who
reads only the README — the document the repo itself designates as the
starting point — cannot discover that `--agent worker` exists, let alone
how to point it at a real OpenAI-compatible endpoint and an MCP search
server.
**Fix:** Add a README section (or a linked `docs/WORKER-AGENT.md`)
covering: the `--agent worker` flags, a worked example pointing
`--fleet-model-endpoint`/`--fleet-search-endpoint` at real services, and
one `examples/researchconfig/fleet.yaml` sample. `--agent fleet` should be
called out as not yet available (see C3-FUNC-4).
**Acceptance:** A new operator can construct a working `--agent worker`
invocation using only README.md plus a real OpenAI-compatible endpoint
and MCP search server, without reading `docs/V2-RESEARCH-AGENT.md`.

### C3-FUNC-2 — `--fleet-model-name` help text contradicts the actual (hard) requirement
**Severity:** Medium
**File:** `internal/config/flags.go:41`; `internal/cli/research.go:162-166`
The flag is registered as:
```go
fs.String("fleet-model-name", "", "standard frontier model name (adapter default if unset)")
```
implying an adapter-side default applies when the flag is omitted. But
`buildWorker` treats it as mandatory, and the code's own comment concedes
there is no default:
```go
if fc.ModelName == "" {
    // The model client mandates an explicit model identifier — it carries
    // no default of its own — so require it here rather than sending an
    // empty model field and failing deeper in the adapter.
    return nil, errors.New("research --agent worker: fleet.model_name is required")
}
```
Verified live: `chiron research --agent worker --query "x" --fleet-model-endpoint https://example.com` (no `--fleet-model-name`) fails with
`fleet.model_name is required`, not a fallback model.
**Fix:** Change the flag's help string to state the field is required for
`worker`/`fleet` (e.g. `"standard frontier model name (required for --agent worker/fleet)"`), matching the pattern already used for
`--fleet-model-endpoint`.
**Acceptance:** `chiron research --help` no longer implies an unset
`--fleet-model-name` will fall back to an adapter default.

### C3-FUNC-3 — Several documented top-level research flags silently no-op under `--agent worker`
**Severity:** Medium
**File:** `internal/cli/research.go:87-89` (dispatch), `:355-375` (`geminiOptions`, the only consumer of these fields), `:109` (`cfg.Plan` check)
`chiron research --help` lists `--budget`, `--plan`, `--accept-plan`,
`--stream`, `--quiet`, `--tools`, `--visualise`, `--file-search`,
`--input`, `--mcp`, and `--template` as generic flags of `research`, with
no per-agent scoping note. All of them are read only inside the
Gemini-specific branch of `runResearch` (via `geminiOptions` and the
`cfg.Plan` check at line 109); `runWorkerResearch`/`buildWorker` never
reference any of them. Verified live:
```
$ chiron research --agent worker --query test --budget 0.01 \
    --fleet-model-endpoint https://example.com --fleet-model-name gpt-5.4 \
    --fleet-model-key-ref secret://FOO --fleet-search-endpoint https://search.example.com \
    --fleet-search-key-ref secret://BAR
chiron: secret: environment variable FOO is not set
```
— the run proceeds straight past the budget gate with no warning that
`--budget` was ignored. This is most notable for `--budget`, given
AGENTS.md's money-safety framing of that flag as blocking "the run...
before any interaction is created." The worker path does have its own
enforced spend cap (`--fleet-ceiling`, verified wired at
`internal/researcher/fleet/worker.go:200`), so the risk is mitigated, but
`--budget`'s pre-run estimate gate and `--fleet-ceiling`'s in-loop
after-the-fact cap are different guarantees and a user reaching for the
one they already know (`--budget`) gets neither an effect nor a nudge
toward the one that works. None of this scoping is recorded in
`docs/DECISIONS.md` — only in one inline code comment at
`internal/cli/research.go:129-130`.
**Fix:** Either (a) reject the incompatible flag/agent combination with a
clear error at the composition root (preferred for `--budget`/`--plan`,
which change money-safety semantics), or (b) add a one-line note to each
affected flag's help text and to the README table scoping it to
`deep-research`/`deep-research-max`, and record the scoping decision in
`docs/DECISIONS.md`.
**Acceptance:** `chiron research --agent worker --budget <n> ...` either
errors clearly or the help text/README no longer implies `--budget`
applies uniformly across agents.

### C3-FUNC-4 — `chiron get`/`chiron follow-up` are hardwired to the Gemini adapter regardless of the id's origin
**Severity:** Medium
**File:** `internal/cli/research.go:233-247` (`runGet`), `:249-` (`runFollowUp`)
`runGet` and `runFollowUp` unconditionally call `gemini.New(opts)` and
send whatever id they were given to it — there is no check of the `wkr_`
prefix that `newInteractionID` mints specifically to "mark its origin,
distinct from the Gemini adapter's server-issued ids"
(`internal/researcher/fleet/worker_researcher.go:200-206`). `docs/V2-RESEARCH-AGENT.md` §3 explicitly says not to claim `chiron get <id>` can
recover an in-process run, but the CLI does not enforce or explain that
limitation locally: if `GEMINI_API_KEY` happens to be set (a likely state
for any Chiron user who has also run deep-research tasks), `chiron get
wkr_...` or `chiron follow-up wkr_...` will make a real network call to
the Gemini Interactions API with a nonexistent id and surface whatever
generic error Gemini returns, rather than a clear local message like
"in-process worker runs cannot be resumed after the process exits."
Verified: with `GEMINI_API_KEY` unset, the failure is a generic secret
error that gives no hint about the real cause either
(`chiron: secret: environment variable GEMINI_API_KEY is not set`).
**Fix:** In `runGet`/`runFollowUp`, check the id prefix before building the
Gemini researcher and fail fast with a message naming the actual
limitation when it is a `wkr_`-prefixed id.
**Acceptance:** `chiron get wkr_<anything>` fails locally with a message
naming the in-process-resume limitation, without depending on whether
`GEMINI_API_KEY` happens to be set.

### C3-FUNC-5 — `--agent fleet` is listed as a first-class choice in `--help` but always fails at runtime
**Severity:** Low
**File:** `internal/config/flags.go` (`--agent` help string), `internal/cli/research.go:343` (`checkResearcherWired`)
`chiron research --help` and `chiron research-config --help` both print
`--agent string   agent: "deep-research", "deep-research-max", "worker" or "fleet"`, listing `fleet` on equal footing with the other three. Every
invocation of `--agent fleet` currently fails with `agent "fleet": not yet
wired (v2 Wave 4 in progress)`. This is a deliberate, documented
mid-wave state (`docs/V2-PLAN.md`, `docs/V2-RESEARCH-AGENT.md` §6), and
the runtime error is clear and typed, so this is not a defect in the
error path itself — but a user only learns `fleet` is unavailable by
invoking it, since the flag's own help text gives no signal.
**Fix:** Either omit `fleet` from the `--agent` help string until Wave 4
lands, or annotate it inline, e.g. `... "fleet" (not yet implemented)`.
**Acceptance:** `chiron research --help` does not present `fleet` as an
equally-available choice.

### C3-FUNC-6 — No progress signal on stderr for the duration of a worker run
**Severity:** Low
**File:** `internal/researcher/fleet/worker_researcher.go:105-118` (`Await`); `internal/run/run.go:127-146` (events emitted before `Await`)
`run.Run` emits `run_started` and `interaction_created` before calling
`Await`, then nothing until `Await` returns, at which point
`status_changed`/`run_completed`/`cost_summary` fire together. `Worker.Await`
(`worker_researcher.go:112-118`) is a bare channel-wait with no interim
transport emission, and the worker loop itself
(`internal/researcher/fleet/worker.go`) only touches `Tracer` spans/metrics,
never `Transport`. Verified live via the scratch harness: for a
multi-turn run the NDJSON stderr stream contains exactly `run_started`,
`interaction_created`, `status_changed`, `run_completed`, `cost_summary`
— no `delta` events — for the whole run, which can last up to
`--fleet-worker-timeout` (default 5m) across up to `--fleet-max-turns`
(default 8) turns of model/search/fetch calls. The Gemini path has an
equivalent gap closed by `--stream` thought-summary deltas
(`bindThoughtDisplay`); the worker path has no analogous mechanism, and
(per C3-FUNC-3) `--stream`/`--quiet` are silently no-ops there too. Not a
blocker — the run does complete and is bounded by structural caps — but an
operator watching a live worker run has no way to tell it is progressing
versus hung until the caps or the answer arrive.
**Fix:** Emit a lightweight per-turn transport event (e.g. reusing
`KindStatusChanged` with a turn/action detail, or a new `delta`-equivalent)
from the worker loop, gated the same "best effort, never fails the run"
way `docs/V2-RESEARCH-AGENT.md` §6 already specifies for the fleet lead's
spans.
**Acceptance:** A multi-turn `--agent worker` run emits at least one
transport event between `interaction_created` and `run_completed`.

## Verified OK

- **CLI/config surface completeness.** Every `FleetConfig` field
  (`model_endpoint`, `model_name`, `model_key_ref`, `search_endpoint`,
  `search_key_ref`, `max_turns`, `max_tokens`, `ceiling_gbp`,
  `worker_timeout`, `max_workers`, `concurrency`, `memory`) has both a
  `--fleet-*` flag (`internal/config/flags.go`) and a JSON/YAML config-file
  path (`internal/config/config.go`), confirmed via `chiron research
  --help` and round-trip `research-config` runs.
- **`research-config` round-trips the fleet block correctly** for both
  JSON and YAML bases, including base+overlay merge (overlay flags replace
  only the targeted field; unset fields keep the base's value; defaults
  fill in fields absent from both). Verified with a piped JSON base
  (`--config -`) and a YAML `--config <path>` file, overlaying
  `--fleet-max-workers`/`--fleet-concurrency` in each case.
- **Validation error sequencing is one-clear-error-at-a-time** and each
  message names the exact missing field (`fleet.model_endpoint` →
  `fleet.model_name` → `fleet.model_key_ref` → `fleet.search_endpoint` →
  secret resolution), which is unambiguous even though it takes several
  round-trips to reach a runnable command — a UX-ergonomics observation
  deferred to the UX reviewer, not a functionality defect.
- **`fleet.max_workers`/`fleet.concurrency` are correctly scoped to
  `--agent fleet` only** (`internal/config/config.go:296-311`); a single
  worker is not made to satisfy fan-out validation it doesn't need.
- **Structural spend/runaway caps are genuinely wired**, not just
  accepted and discarded: `MaxTurns`, `MaxTokens`, and `CeilingGBP` are all
  checked inside the loop (`internal/researcher/fleet/worker.go:193-201`)
  and independently verified to stop the run with an `incomplete` status
  and a diagnostic detail naming the cap that fired.
- **The worker path produces a correct report through all three sinks**
  exercised (`stdout-markdown` via `-o text`, `stdout-json` via `-o json`,
  file via `--out`), verified live against `model.FakeServer` +
  `search.FakeServer`: front matter, body, and the `## Sources` citation
  list all render correctly in every sink, and no separate reporting path
  exists — confirming the "map onto one `types.Interaction`, reuse the
  normal formatter/sinks" design in `docs/V2-RESEARCH-AGENT.md` §5.
- **`--fleet-search-key-ref` is genuinely optional** (a keyless search MCP
  is supported): omitting it does not block a run, and no `Authorization`
  header is sent, per `internal/cli/research.go` (search key resolved only
  when the ref is non-empty).
- **No auto-retry on paid model turns**: `internal/researcher/fleet/model`
  has no retry logic at all (`grep -n retry` finds nothing outside test
  files), consistent with the money-safety non-negotiable; a deeper audit
  of this path is the code reviewer's remit.
- **Secret references resolve at the CLI composition root, not inside the
  worker loop** (`buildWorker`, `internal/cli/research.go:150-167`),
  matching the pattern documented in `AGENTS.md`'s security-sensitive
  configuration section.

## Open questions

- I did not attempt to reach a real OpenAI-compatible endpoint or a real
  MCP search server (per the brief, fakes only / no real network), so I
  cannot confirm end-to-end interop with an actual provider beyond what
  the fake-server-backed request/response shape implies. If the
  synthesiser wants that confirmed, it needs a follow-up review with
  network access explicitly authorised.
- `--agent fleet`'s own accessibility (config/CLI surface, defaults,
  sinks) could not be exercised beyond `research-config`, since
  `checkResearcherWired` rejects it before any researcher is built — this
  is expected per the Wave 4 status, not a gap in this review.
