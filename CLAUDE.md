# CLAUDE.md

Read `AGENTS.md` for the per-package map, build commands, money-safety
rules, and test conventions — it is the single orientation document for
agentic sessions in this repository. The spec is `docs/PROPOSAL.md`;
design decisions live in `docs/DECISIONS.md`; for anything touching the
Gemini API, `docs/INTERACTIONS-API.md` is normative and wins over the
proposal.

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
