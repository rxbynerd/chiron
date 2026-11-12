# CLAUDE.md

Read `AGENTS.md` for the per-package map, build commands, money-safety
rules, and test conventions — it is the single orientation document for
agentic sessions in this repository. `docs/PROPOSAL.md` is the v1 design
record; the v2 design is `docs/V2-PLAN.md` and `docs/V2-RESEARCH-AGENT.md`.
Design decisions live in `docs/DECISIONS.md`; for anything touching the
Gemini API, `docs/INTERACTIONS-API.md` is normative and wins over the
proposal.

v2 is in progress: `docs/V2-PLAN.md` is the phased implementation plan and
`docs/V2-RESEARCH-AGENT.md` is the research-agent core design (binding for the
v2 researcher core). Per the prove-first pivot (V2-AMENDS amend 8), the v2
`Researcher` is Chiron's own **in-process** research loop on **one
OpenAI-compatible standard model** plus web search (an MCP tool or Exa) and
fetch (an SSRF-guarded `web_fetch` or Exa). `--agent worker` runs a single bounded loop and
`--agent fleet` runs a lead orchestrating several workers; both are
implemented and opt-in. The evaluation gate (the eval-versus-baseline harness,
issue #25) is outstanding, so `deep-research` stays the default agent.
**Managed deep research (the v1 Gemini Deep Research adapter) is a stopgap +
eval baseline only, never the product.**
**Stirrup** (`github.com/rxbynerd/stirrup`) is the deferred scale-out path,
adopted later as an embedded engine, not a remote K8s-job harness; nothing in
the tree dials a `HarnessService`.

Non-negotiables worth repeating:

- Research-only: Chiron never gains write/execute capabilities.
- The run core depends only on the seam interfaces.
- No vendor AI SDKs; hand-rolled `net/http` adapters only.
- New dependencies require a justification entry in `docs/DECISIONS.md`.
- Wire types live in `internal/interactions`; `internal/types` is the
  stable domain model — keep API churn inside the adapter.
- Spend paths follow AGENTS.md "Money safety": budget gate before any
  create, no auto-retry on creates, emit the interaction id early.
- en-GB spelling in documentation; no emojis.
