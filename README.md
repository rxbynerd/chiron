# Chiron

Chiron is the **researcher** of the Equestrianism suite — the wise centaur
to Stirrup's tack. It investigates a topic with a long-running research
agent and returns a cited, structured Markdown report. It is written in
Go and ships as a single static binary.

Chiron is **research-only**: it never mutates a workspace, runs no shell,
and applies no edits. Where Stirrup *changes* code under a harness,
Chiron *investigates*.

> Status: v1 under construction. The CLI surface and configuration exist;
> the research behaviour is landing milestone by milestone
> (see `docs/PROPOSAL.md` §9).

## Usage (v1 surface)

```sh
# The main path: start, await, format, emit.
chiron research --query "Competitive landscape of 10BASE-T1L PHY vendors" --out report.md

# Emit the resolved config without running; composable in a pipeline.
chiron research-config --agent deep-research-max \
  | chiron research-config --visualise \
  | chiron research --query "..." --out report.md

# Resume a crashed or in-progress run by interaction ID.
chiron get <interaction-id>

# Ask a follow-up against a completed interaction.
chiron follow-up <interaction-id> --query "..."
```

Configuration is a single declarative `ResearchConfig` (JSON or YAML),
resolved as: defaults → base config (`--config` or piped stdin) →
explicitly set flags. See `examples/researchconfig/`. The API key is a
`secret://` reference — never a literal in config.

## Building

```sh
just build   # or: go build ./cmd/chiron
just test
```

## Documents

- `docs/PROPOSAL.md` — the project proposal and architecture (the spec).
- `docs/DECISIONS.md` — running decision log.
- `AGENTS.md` / `CLAUDE.md` — orientation for agentic sessions.
- `SECURITY.md` — security posture and reporting.
