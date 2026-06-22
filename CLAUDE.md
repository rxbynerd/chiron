# CLAUDE.md

Read `AGENTS.md` for the per-package map, build commands, money-safety
rules, and test conventions — it is the single orientation document for
agentic sessions in this repository. The spec is `docs/PROPOSAL.md`;
design decisions live in `docs/DECISIONS.md`; for anything touching the
Gemini API, `docs/INTERACTIONS-API.md` is normative and wins over the
proposal.

v2 is in active planning on the `v2` branch: `docs/V2-PLAN.md` is the phased
implementation plan and `docs/V2-RESEARCH-AGENT.md` is the research-agent core
design (treat it as binding for the v2 researcher core). In v2 the `Researcher`
is Chiron's own multi-agent fleet — a lead orchestrator driving **Stirrup**
(`github.com/rxbynerd/stirrup`) research-mode jobs on **standard frontier
models** (GPT-5.4/5.5, Claude, Gemini-Pro) equipped with a web-search tool.
**Managed deep research (the v1 Gemini Deep Research adapter) is a stopgap +
eval baseline only, never the product.** Stirrup owns the provider adapters;
its wire types are Buf-generated, never `go get`. The Chiron runner is the
`stirrup.harness.v1` `HarnessService` server (Stirrup workers dial in).

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
