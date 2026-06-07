# Chiron — agent orientation

Chiron is the Equestrianism suite's researcher: a Go CLI that drives a
long-running research agent end-to-end (start → await → retrieve → format
→ emit) and produces a cited Markdown report. It is **research-only**: it
never mutates a workspace, runs no shell, applies no edits.

Read `docs/PROPOSAL.md` before changing anything structural; record
notable decisions in `docs/DECISIONS.md`. For anything touching the
Gemini API, `docs/INTERACTIONS-API.md` is normative and wins over the
proposal. Documentation uses en-GB spelling and no emojis.

## Build and verify

```sh
just build   # go build -o bin/chiron ./cmd/chiron
just test    # go test ./...
just vet     # go vet ./...
just lint    # golangci-lint if installed, else go vet
```

CI (`.github/workflows/ci.yml`) runs build, vet, test, and golangci-lint.

## Per-package map

| Package | Role |
| --- | --- |
| `cmd/chiron` | Entrypoint; delegates to `internal/cli`. |
| `internal/cli` | Cobra command tree (`research`, `research-config`, `get`, `follow-up`) and flag→config resolution. Commands stay thin. |
| `internal/config` | `ResearchConfig`: the single declarative config. JSON/YAML, flag binding, base+overlay merge semantics for pipelines. |
| `internal/types` | Seam-level domain types: `Interaction`, `Output`, `Citation`, `Usage`, `Report`, `RunResult`. Wire schema lives in `internal/interactions`; `researcher/gemini` maps wire→domain (see DECISIONS.md). |
| `internal/researcher` | `Researcher` seam (Start/Await/Result) — the only model-bearing component. Gemini adapter lands in `researcher/gemini`; v2 fleet orchestrator in `researcher/fleet`. |
| `internal/planner` | `Planner` seam (Propose/Refine) + the interactive plan-review `Session` for `--plan`; the gemini binding lives in `researcher/gemini`. |
| `internal/formatter` | `Formatter` seam: `Interaction` → Markdown `Report`. |
| `internal/sink` | `ReportSink` seam: where the final report goes (stdout-markdown, file, stdout-json). |
| `internal/transport` | `Transport` seam: run events out of the core. stdio NDJSON in v1; gRPC in v2. |
| `internal/trace` | `Tracer` seam: spans + run metrics (OTel + local jsonl bindings to come). |
| `internal/secret` | `Resolver` seam for `secret://` references. Literal keys never appear in config, logs, or traces. |
| `internal/memory` | `ContextStore` seam + `Noop` only — deliberately unimplemented (PROPOSAL §5); fulfilled externally by Paddock in v2. Declared locally, never imported from paddockapi (see DECISIONS.md). |

## Ground rules

- The run core must depend only on the seam interfaces; concrete types
  are injected from `ResearchConfig`.
- Dependency surface is minimal and auditable: stdlib, cobra (+pflag),
  yaml.v3. **No vendor AI SDKs** — adapters are hand-rolled `net/http`.
  Justify any new dependency in `docs/DECISIONS.md` before adding it.
- Secrets are `secret://` references end to end.
- Keep commits in logical units; explain rationale in the message body.
