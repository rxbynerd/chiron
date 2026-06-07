# Decision log

Running log of design and dependency decisions made while building Chiron,
so future sessions (human or agentic) can see *why* the code is shaped the
way it is rather than re-deriving or re-litigating it. Append new entries
at the end with a date and the deciding context; supersede old entries in
place with a note rather than deleting them.

---

## 2026-06-07 — ContextStore is declared locally, not imported from paddockapi

Chiron's `internal/memory` declares its own `ContextStore` interface and
supporting types, structurally matching the contract sketched in
`docs/PADDOCK.md` §4, rather than importing `paddockapi`. This follows the
recommendation in PADDOCK.md §8.4: a direct import would couple the two
projects' versioning, while a local interface keeps Chiron buildable
without Paddock, and Go's structural typing lets Paddock satisfy the seam
without either project depending on the other. v1 binds the `Noop` store
only — defining the seam and *not* implementing it is the deliberate
decision recorded in PROPOSAL.md §5.

## 2026-06-07 — Dependency set: cobra, pflag (transitive), yaml.v3

- `github.com/spf13/cobra` — the sanctioned CLI framework, consistent with
  the rest of the suite.
- `github.com/spf13/pflag` — arrives transitively with cobra; used
  directly in `internal/config` for flag binding (no new module).
- `gopkg.in/yaml.v3` — config files and the YAML half of "JSON/YAML
  serialisable". One strict yaml.v3 decoder handles both forms, since JSON
  is a YAML subset; stdlib `encoding/json` handles emission.

No vendor AI SDKs, ever: the Gemini adapter (M1/M2) is hand-rolled
`net/http` + SSE per PROPOSAL.md §2, so every line is auditable.

## 2026-06-07 — Config merge semantics: explicit-flag overlay over a base

Resolution order is: documented defaults → base config (`--config` path,
`-` for stdin, or auto-detected piped stdin) → only the flags the user
explicitly set (via `pflag.FlagSet.Visit`). Flag *defaults* are never
applied over a base, so a piped `research-config` value survives later
pipeline stages that don't mention it. Decoding starts from the defaults,
so absent keys keep them; unknown keys are rejected (`KnownFields`)
because a silently misspelt key would silently change a paid run.
`--quiet` is sugar for `--stream=false`; the pair is marked mutually
exclusive at the command level.

## 2026-06-07 — Researcher is Start/Await/Result, not one Research call

PROPOSAL.md §6 names the contract explicitly ("the core still calls
start / await / result"). The split keeps the run core a deterministic
state machine with natural span boundaries for the tracer, and because
`Await`/`Result` accept a bare interaction ID, crash-resume
(`chiron get <id>`) and follow-ups fall out of the same surface with no
local state.

## 2026-06-07 — Durations serialise as strings

`config.Duration` wraps `time.Duration` to marshal as `"30m"` in JSON and
YAML rather than a nanosecond integer, since ResearchConfig is a
user-facing, hand-editable document. Both string and integer forms are
accepted on decode.
