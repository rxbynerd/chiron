# CLAUDE.md

Read `AGENTS.md` for the per-package map, build commands, and ground
rules — it is the single orientation document for agentic sessions in
this repository. The spec is `docs/PROPOSAL.md`; design decisions live in
`docs/DECISIONS.md`.

Non-negotiables worth repeating:

- Research-only: Chiron never gains write/execute capabilities.
- The run core depends only on the seam interfaces.
- No vendor AI SDKs; hand-rolled `net/http` adapters only.
- New dependencies require a justification entry in `docs/DECISIONS.md`.
- en-GB spelling in documentation; no emojis.
