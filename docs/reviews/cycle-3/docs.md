# Cycle 3 documentation review — Chiron v2 research-agent PR (#1)

Scope: docs added/changed on `feat/v2-research-agent` (HEAD `d4ac9f2`, base `origin/main`
`926d7bf`) — `docs/V2-PLAN.md`, `docs/V2-RESEARCH-AGENT.md`, `docs/STIRRUP-ENGINE-PROPOSAL.md`,
`docs/V2-AMENDS.md`, the new `docs/DECISIONS.md` entries, and the `AGENTS.md`/`CLAUDE.md` edits.
Reviewed for factual accuracy against the code on this branch, internal consistency across the
four planning docs, and en-GB/formatting hygiene. No web research performed, per the brief.

## Findings

### C3-DOC-1 — CLAUDE.md still tells every future agent session to build the deferred Stirrup-remote harness

**Severity:** Critical
**File:** `CLAUDE.md:10-19`

This paragraph was *added* by this PR (`git diff origin/main...HEAD -- CLAUDE.md`, hunk
`@@ -7,6 +7,17 @@`) and reads:

> "In v2 the `Researcher` is Chiron's own multi-agent fleet — a lead orchestrator driving
> **Stirrup** (`github.com/rxbynerd/stirrup`) research-mode jobs on **standard frontier
> models**... Stirrup owns the provider adapters; its wire types are Buf-generated, never
> `go get`. The Chiron runner is the `stirrup.harness.v1` `HarnessService` server (Stirrup
> workers dial in)."

This is the exact Stirrup-remote design that `docs/V2-PLAN.md`'s own §0 banner says was
abandoned in the 2026-06-22 "prove-first pivot": *"the former Stirrup-remote content
(`stirrup.harness.v1` / `HarnessService` server / K8s-Job provisioning...) now lives in the
Scale-out track (deferred)"* (`docs/V2-PLAN.md:36-40`). `docs/V2-RESEARCH-AGENT.md:38,51`
(the doc CLAUDE.md itself calls "binding for the v2 researcher core") is explicit: *"Worker
substrate: Chiron-owned in-process goroutines, not Stirrup K8s Jobs"* and *"Do not build
`stirrup.harness.v1`, a runner-as-harness server, or K8s worker Jobs for v2."* The code this
same PR ships confirms the pivot, not the paragraph: `internal/researcher/fleet/worker.go`
is a Chiron in-process goroutine loop calling `internal/researcher/fleet/model` (a hand-rolled
OpenAI-compatible client), and `go.mod` has no `stirrup` entry at all — there is no
`HarnessService`, no Stirrup import, and no Buf target for `stirrup.harness.v1` anywhere on
this branch. CLAUDE.md is the first orientation file loaded into every agentic session in this
repo, so a future session following it verbatim would try to build the exact plumbing
`V2-PLAN.md`/`V2-RESEARCH-AGENT.md` say not to build.

**Fix:** Replace the paragraph with the actual prove-first shape: a Chiron lead orchestrator
that fans out bounded **in-process** worker goroutines on **one** standard model via a
hand-rolled `net/http` adapter (`internal/researcher/fleet/model`), with Stirrup adoption
deferred to a later, optional embedded-engine swap behind the `Researcher` seam (cite
`docs/V2-PLAN.md` D9 and `docs/V2-RESEARCH-AGENT.md` §9). Drop the `HarnessService`/"Stirrup
workers dial in" sentence entirely — it describes the deferred scale-out track, not v2's
critical path.

**Acceptance:** CLAUDE.md's v2 paragraph names the in-process worker/fleet, the shared model
adapter, and the stopgap/eval-baseline role of Gemini Deep Research, and contains no reference
to `stirrup.harness.v1`, `HarnessService`, or Stirrup as something v2 imports or dials.

---

### C3-DOC-2 — AGENTS.md's per-package map misdescribes `internal/researcher/fleet` and omits two new packages entirely

**Severity:** High
**File:** `AGENTS.md:40`

The unchanged table row still reads:

> `internal/researcher/fleet` | v2 stirrup-fleet orchestrator seam — a stub that returns
> not-implemented, pinned by tests.

That is accurate only for the `Fleet{}` type in `internal/researcher/fleet/fleet.go` (still a
literal `ErrNotImplemented` stub, confirmed by reading the file). It is no longer an accurate
description of the *package*: this PR adds `worker.go`, `worker_action.go`, `worker_prompt.go`
and `worker_researcher.go` to that same package, implementing a fully wired, non-stub
`Worker` `Researcher` binding (`NewWorker`, `Start`/`Await`/`Result`) that `internal/cli/research.go`
constructs for real on `--agent worker`. A reader using this table to orient themselves — which
is exactly what `AGENTS.md`'s own framing ("the per-package map") promises — will conclude the
whole package is inert.

Separately, this PR adds two more new packages that never got a row at all:
`internal/researcher/fleet/search` (the MCP search client) and `internal/researcher/fleet/fetch`
(the SSRF-guarded `web_fetch` client). Only `internal/researcher/fleet/model` was added
(`AGENTS.md:41`, diff hunk `@@ -38,6 +38,7 @@`). `docs/V2-PLAN.md`'s own Wave 3 deliverables
list promises this update explicitly: *"`AGENTS.md` updated: the per-package map gains
`internal/researcher/fleet/{model,worker}`..."* (`docs/V2-PLAN.md:479-481`) — the `worker`
half of that promise, and the search/fetch packages, were not delivered.

**Fix:** Split the existing row into (a) `internal/researcher/fleet` — the lead
orchestrator seam, still `Fleet{}`/`ErrNotImplemented` pending Wave 4, but now also the home of
the wired `Worker` Researcher (`--agent worker`); and add rows for
`internal/researcher/fleet/search` (Streamable-HTTP MCP search client) and
`internal/researcher/fleet/fetch` (SSRF-guarded `web_fetch` client), matching the style of the
existing `.../model` row.

**Acceptance:** Every package under `internal/researcher/fleet/` that ships code on this
branch (`fleet`, `fleet/model`, `fleet/search`, `fleet/fetch`) has a table row, and the
`fleet` row itself does not claim the whole package is a stub.

---

### C3-DOC-3 — V2-PLAN.md and STIRRUP-ENGINE-PROPOSAL.md cite ten `V2-RESEARCH-AGENT.md` subsections that no longer exist

**Severity:** High
**File:** `docs/V2-PLAN.md:4,14,35,49,107,114,126,139,207,262,263,818,833,846`; `docs/STIRRUP-ENGINE-PROPOSAL.md:20,189`

`docs/V2-PLAN.md` and `docs/STIRRUP-ENGINE-PROPOSAL.md` repeatedly cite section numbers in
`docs/V2-RESEARCH-AGENT.md` that its current heading structure does not have:

- `§0a` — cited at `V2-PLAN.md:4,14,35,49,207` and `STIRRUP-ENGINE-PROPOSAL.md:20,189` as
  "the prove-first pivot" / "the decision and grounded rationale."
- `§5.3` — cited at `V2-PLAN.md:126,846` (research-only-by-construction) and `§5.4` — cited at
  `V2-PLAN.md:262-263,833` (Stirrup dispatch design) and `§5.5` — cited at `V2-PLAN.md:107,114,139`
  (the shared model substrate).
- `§2–§11` — cited at `V2-PLAN.md:833` as where "the design [is] preserved" for the
  Stirrup-remote `HarnessService`/K8s-Job path.

`grep -n '§[0-9]\+[a-z.]'` against the actual `docs/V2-RESEARCH-AGENT.md` returns zero matches:
its current headings are plain top-level integers, `## 0.` through `## 11.`, with no `0a` and
no decimal subsections at all (confirmed by reading the file in full). This is not a
typo — `V2-RESEARCH-AGENT.md`'s own frontmatter explains why: `supersedes: replaces the
2026-06-22 research-agent design/history note with an implementation-session brief`
(`docs/V2-RESEARCH-AGENT.md:9`). The earlier, richer version of that document (with a §0a pivot
banner and detailed §2–§11 Stirrup-remote design) was deliberately deleted and replaced with the
current lean brief on 2026-07-01, but `V2-PLAN.md` (dated `2026-06-22`, never re-dated) and
`STIRRUP-ENGINE-PROPOSAL.md` were not updated to match, so every one of these ~10 cross-references
now points at content that has been removed. The `§2–§11` case is worse than a dead anchor: the
"design preserved" it claims to point to (a Stirrup-remote `HarnessService` server + K8s Jobs) is
the opposite of what §2–§11 of the *current* document actually say — §9 of the current document is
titled "Deferred explicitly" and lists exactly that HarnessService/K8s-Job material as something
**not** to build.

**Fix:** Either restore the specific subsections these citations depend on (if the detail is
still wanted) or — preferable, given the file's own stated intent to be a lean brief — replace
every `§0a`/`§5.x`/`§2–§11` citation in `V2-PLAN.md` and `STIRRUP-ENGINE-PROPOSAL.md` with the
correct current top-level section number (e.g. `§0` for the pivot framing, `§1` for
non-negotiables, `§5` for the Wave 3 worker design, `§9` for what's deferred). Bump
`V2-PLAN.md`'s `date:`/`supersedes:` frontmatter to acknowledge the 2026-07-01 rewrite of its
companion document.

**Acceptance:** `grep -rn 'V2-RESEARCH-AGENT.*§' docs/` produces only section numbers that
exist as `##` headings in the current `docs/V2-RESEARCH-AGENT.md`.

---

### C3-DOC-4 — V2-AMENDS.md contains two amendments the shipped design has since reversed, with nothing marking them superseded

**Severity:** Medium
**File:** `docs/V2-AMENDS.md:3,5`

Amend 1: *"We can't use Vertex AI with Workload Identity: let's use OpenAI's Responses API via
Workload Identity Federation authentication instead."* Amend 3: *"...If we're leveraging OpenAI's
Responses or Anthropic's Messages API, we can revert to long requests..."* Both describe a
design — a full OpenAI Responses API adapter, keyless WIF for OpenAI, Anthropic Messages API —
that amend 8 (in the same file) and every later doc explicitly reverse: `docs/V2-PLAN.md`'s D2
row states *"The drifted `o3`/`o4-mini-deep-research` bindings are **dropped**"* and *"it builds
**no** full OpenAI Responses adapter and **no** `docs/RESPONSES-API.md`"*
(`docs/V2-PLAN.md:107`); `docs/V2-RESEARCH-AGENT.md:69-71` repeats *"No full OpenAI Responses
adapter in Chiron."* The shipped `internal/researcher/fleet/model` client is a minimal
OpenAI-**compatible Chat Completions** client (per `docs/DECISIONS.md`'s 2026-07-01 "Standard-model
adapter" entry), not a Responses API client, and there is no Anthropic Messages API code or
Vertex/WIF binding anywhere on this branch. `V2-PLAN.md` says it *"applies `docs/V2-AMENDS.md` in
full"* (`docs/V2-PLAN.md:57`) without ever flagging that amends 1 and 3 are themselves now moot —
a reader who opens `V2-AMENDS.md` on its own (it is referenced by name from three other docs) has
no way to tell items 1 and 3 are stale.

Compounding this, `V2-AMENDS.md` is the only doc in this set with no YAML frontmatter (no
`status`, `date`, or `author`, unlike `V2-PLAN.md`, `V2-RESEARCH-AGENT.md`, and
`STIRRUP-ENGINE-PROPOSAL.md`, which all carry one), and it is the only file in the new docs that
uses curly quotes/apostrophes (`can't`, U+2019) rather than the plain ASCII used everywhere else
in this PR — both point to it being an unedited raw note rather than a maintained document.

**Fix:** Add a one-line status marker per amendment (or a header note) flagging that amends 1 and
3 were superseded by amend 8 and `V2-PLAN.md` D2/D9, or fold the still-live content (amend 8's
rationale) into `V2-PLAN.md` §1's decision table — which already restates all eight amendments
with clearer "Source" provenance — and delete this file. If it is kept as a historical record,
say so explicitly at the top so it is not read as current guidance.

**Acceptance:** Either `V2-AMENDS.md` carries a status header stating which items are superseded
and by what, or the file is removed and `V2-PLAN.md` §1 is confirmed to carry the same
provenance information on its own.

---

### C3-DOC-5 — V2-PLAN.md's Wave 3 gate on Wave 2 (Langfuse observability) is not shown satisfied anywhere in this PR

**Severity:** Medium
**File:** `docs/V2-PLAN.md:449-451`

Wave 3 (the worker/model/search/fetch code this PR ships) opens with an explicit gate:

> "**Gate (from Wave 2).** Do not begin this wave — which makes real, paid model calls in
> integration tests — until Wave 2's acceptance holds: a run forwards spans and complete cost
> signals to a Langfuse collector. Spend must be visible before it scales."

Wave 2's deliverables are the `--otlp-endpoint`/`--langfuse-endpoint`/`--langfuse-public-key`/
`--langfuse-secret-key` CLI flags and the `SpanDecompose`/`SpanDelegate`/`SpanSynthesise`/
`SpanCite` vocabulary (`docs/V2-PLAN.md:386-397`). None of this exists on the branch: `grep -ri
langfuse` across the repo matches only prose in the docs themselves, not `internal/cli` or
`internal/trace`; `internal/trace/names.go` gains only `SpanWorker`, added ad hoc by the Wave 3
worker-loop change rather than by a Wave 2 change; and there is no Wave 2 entry in the five new
`docs/DECISIONS.md` sections (all dated 2026-07-01, all Wave-3-scoped: config surface, model
adapter, search client, fetch client, worker loop). The plan itself treats this gate as a hard
precondition ("do not begin"), so either Wave 2 landed elsewhere and this PR's docs should say so,
or the gate was knowingly skipped and that should be recorded as a decision — right now neither
is true, and a reader relying on `V2-PLAN.md`'s own gating language would believe Wave 3 code
cannot exist yet.

**Fix:** Add a `docs/DECISIONS.md` entry (or a note in `V2-PLAN.md` itself) explaining the actual
sequencing taken — e.g. "Wave 2's CLI flags are deferred to land alongside Wave 4; Wave 3 used
fakes only, so the real-spend gate did not yet apply" — or reorder the waves in the plan if Wave 2
is no longer meant to precede Wave 3 in practice.

**Acceptance:** `docs/V2-PLAN.md`'s Wave 3 gate language matches what `docs/DECISIONS.md` records
as actually having happened before Wave 3's model/search/fetch/worker code was merged.

---

### C3-DOC-6 — V2-RESEARCH-AGENT.md's "existing state" table will read as current fact long after it stops being true

**Severity:** Low
**File:** `docs/V2-RESEARCH-AGENT.md:93`

§2's table, "Existing repository state to preserve," states: *"`internal/researcher/fleet` |
Stub returns `ErrNotImplemented`."* This was true when the document was drafted (2026-07-01,
"implementation-session brief," per its own frontmatter) but this same PR replaces that
statement's premise for the `worker` path (see C3-DOC-2). Because `CLAUDE.md` and `AGENTS.md`
both call this document "binding" for the researcher core with no separate changelog, a reader
opening it after this PR merges has no signal that §2 describes a starting point already
overtaken by §5/§6 of the very same document.

**Fix:** Either date-stamp the table explicitly ("state as of 2026-07-01, before Wave 3") or
add a one-line running note at the top of §2 once Wave 3/4 land, pointing at `AGENTS.md`'s
per-package map as the source of truth for current state.

**Acceptance:** §2 is unambiguous about whether it describes the state before or after Wave 3/4,
without requiring the reader to cross-check git history.

---

## Verified OK

- `docs/STIRRUP-ENGINE-PROPOSAL.md` is unambiguously marked as a non-binding proposal: frontmatter
  `status: proposal / discussion — NOT a commitment by either project`, plus an explicit §0
  framing paragraph repeating that Chiron does not depend on it. No changes needed.
- The Wave 3 `docs/DECISIONS.md` entries (config surface, model adapter, search client, fetch
  client, worker loop/action schema) are internally consistent with each other and with the
  code: config defaults (`MaxTurns=8`, `WorkerTimeout=5m`, `MaxWorkers=5`, `Concurrency=3`,
  `Memory=noop`) match `internal/config/config.go`'s `defaultFleet()` exactly; the three-verb
  action schema (`search`/`fetch`/`final`) described in the "action schema" entry matches
  `internal/researcher/fleet/worker_action.go` field-for-field; `SpanWorker` matches
  `internal/trace/names.go`.
- `AGENTS.md`'s new "Security-sensitive configuration (in-process research agents)" section
  (lines 116-136) is accurate against `internal/config`'s `FleetConfig` validation and against
  `internal/cli/research.go`'s `buildWorker`.
- `internal/cli/research.go`'s `checkResearcherWired` gate matches every doc's description:
  `--agent worker` constructs a real researcher, `--agent fleet` still returns a "not yet wired
  (v2 Wave 4 in progress)" error.
- No emoji found in any of the reviewed docs (`grep` for the common emoji Unicode blocks across
  `docs/` matched only an unrelated pre-existing cycle-2 review file, not this PR's docs).
- No American-spelling regressions found (`optimize`/`behavior`/`color`/`analyze`/`modeling`/
  `labeled`/`canceled`/`fulfill`/`judgment` etc. all absent from the new/changed docs); `behaviour`,
  `judgement`, `synthesise`, `organise`-family spellings are used consistently.
- No leftover `TODO`/`FIXME`/`XXX` markers, and no session-narrative language ("re-thread",
  "de-drift") in the new docs.
- `docs/V2-PLAN.md`'s D1-D9 decision table and its wave sections agree with each other on wave
  scope and gating internally (Waves 1, 5, 6, 7 and the scale-out track are consistent throughout
  the file); the only cross-document breakage found is the `V2-RESEARCH-AGENT.md` subsection
  citations (C3-DOC-3).

## Not checked

- `docs/PROPOSAL.md`, `docs/INTERACTIONS-API.md`, `docs/PADDOCK.md` were read only where cited
  by the changed docs, not re-reviewed in full (out of this PR's diff).
- Prose-level tone/voice pass (register, sentence-level phrasing) was not performed in depth —
  flag to `doc-tone-reviewer` if a dedicated pass is wanted before this merges.
