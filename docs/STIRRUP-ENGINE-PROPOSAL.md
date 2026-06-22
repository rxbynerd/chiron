---
title: "Stirrup as a harness engine — a proposal from a non-coding consumer"
audience: Stirrup maintainers (github.com/rxbynerd/stirrup)
author: "@rubynerd (via Chiron)"
date: 2026-06-22
status: proposal / discussion — NOT a commitment by either project
locale: en-GB
grounding: read from Stirrup `main` at commit f12bedb, 2026-06-22
---

# Stirrup as a harness engine — a proposal from a non-coding consumer

## 0. Framing (please read first)

This document is a **proposal taken to Stirrup**, written from the perspective of a
prospective external consumer (Chiron, a read-only web-research service). It is a **by-product
of a Chiron planning exercise**, captured so the idea is not lost — it is **not** a decision,
and **Chiron does not depend on any of it**. Chiron's v2 plan ships a lean, in-process,
homegrown research loop and treats Stirrup adoption as a *later, optional* scale-out step
(`chiron/docs/V2-RESEARCH-AGENT.md` §0a, `chiron/docs/V2-PLAN.md` D9 + scale-out track). If
Stirrup decides this is out of scope, misaligned with its coding-agent focus, or simply not
worth the maintenance surface, **that is a perfectly acceptable outcome** — Chiron proceeds
regardless.

What follows is one consumer's grounded read of "what would it take to embed Stirrup in-process
as an engine", offered as input to Stirrup's own roadmap.

## 1. The ask, in one line

Make Stirrup's **model-loop core** (provider adapters + credential federation + the agentic
loop + context/budget machinery) consumable **in-process as a dependency-light library** by a
consumer that does **not** want the coding/sandbox/Kubernetes machinery — i.e. promote the
already-existing `harnessapi` embedding seam into a first-class, lean "engine".

## 2. Who is asking, and why

Chiron is a **read-only external web-research** service. Its worker is, in essence, an LLM loop
with two network tools (a web-search MCP and `web_fetch`). It executes no code, edits no files,
needs no workspace, and needs no per-task sandbox. What it would love to borrow from Stirrup is
exactly the part that is **hard to build well and tedious to maintain**:

- the **hand-rolled, SDK-free provider adapters** (`anthropic`, `openai-responses`, `gemini`,
  …) normalised behind one interface, including their per-provider quirks; and
- the **credential-federation layer** (keyless WIF to Azure OpenAI / Anthropic / Gemini-Vertex
  / Bedrock) — security-sensitive, multi-cloud, and easy to get subtly wrong.

It does **not** want, and would prefer not to carry, the parts that exist *because Stirrup is a
coding agent*: the sandbox/workspace executors, the Kubernetes-job runtime, git strategies, the
edit/`run_command` tools, and the policy/approval engine.

The mismatch Chiron hit is that Stirrup's **only blessed consumption model today is "a
short-lived K8s job that dials a control plane"** — a shape that exists to give *coding* tasks
disposable, isolated workspaces. A read-only research consumer pays the full operational price
of that isolation architecture to reach assets (adapters + auth) that have nothing to do with
isolation.

## 3. What Stirrup already gives us toward this (grounded)

Encouragingly, the embedding seam **already exists** — this proposal is mostly about hardening
and slimming it, not inventing it.

- **A public in-process embedding API.** `harness/harnessapi/harnessapi.go` is explicitly
  *"the public API for embedding the stirrup harness in-process … for external consumers (e.g.
  the stable control plane's local provisioner)"*. It exposes
  `BuildLoopWithTransport(ctx, *types.RunConfig, Transport) (*Loop, error)` and `Loop.Run` /
  `Loop.Close`. An external module can already construct and run the loop in-process from a
  `RunConfig`, without dialling anything.
- **The loop runs fine without a control plane or streaming.** `harness/internal/core/
  subagent.go` runs a child `AgenticLoop` in-process with a capture transport wrapping
  `NullTransport`, a `NoneVerifier`, and a `NoneGitStrategy` — proof the loop does not require
  the remote wire or the git/verify machinery.
- **Provider/credential are wired by the factory from the `RunConfig`.** A consumer therefore
  does not need to import the provider/credential packages directly — setting `provider.type`
  and the credential source in the `RunConfig` is enough.
- **`types` is already a clean, zero-dependency module.** `types/go.mod` has no `require` block;
  the repo is already a four-module workspace (`types`, `gen`, `harness`, `eval`) per `go.work`,
  so module boundaries are an established tool here.

## 4. The blockers (grounded)

What stops a research-only consumer from using the above *cleanly* today:

1. **The embedding API lives in the heavy `harness` module.** `harnessapi` imports
   `harness/internal/core` and `harness/internal/transport`, so importing it pulls in all of
   `harness/go.mod`'s direct dependencies — including **`k8s.io/api`, `k8s.io/apimachinery`,
   `k8s.io/client-go`, `k8s.io/streaming`, `k8s.io/utils`** (lines 35–39), the **AWS SDK v2**
   family (`aws-sdk-go-v2`, `…/config`, `…/credentials`, `…/service/bedrockruntime`, `…/ssm`,
   `…/sts`, lines 6–11), and **`cedar-policy/cedar-go`** (line 12). A consumer that runs no
   Kubernetes, no containers, no Bedrock, and no policy engine still inherits all of it.
2. **The factory constructs an executor unconditionally.** Per `harness/internal/core/
   factory.go` (`buildExecutor`), the supported executor types are `local` / `container` /
   `k8s` / `k8s-sandbox` / `api` — there is **no `none`**. A read-only, no-workspace consumer
   must pass `executor:"api"` (a VCS-backed read path it doesn't want) or accept a local
   filesystem executor, when conceptually it wants *no executor at all*.
3. **There is no one-shot / no-loop entrypoint.** The loop is always agentic, and the result
   contract is free-text only: `types/result.go:55-59` notes that *"callers that framed their
   prompt for JSON output parse this field"* (`RunResult.FinalAssistantText`) — there is no
   `response_format` / output-schema in `RunConfig`, and no "single completion, return text"
   helper. A consumer that wants one tool-less judgement call (decompose/synthesise/cite) must
   spin a whole loop, or build its own one-shot path.
4. **The high-value packages are `internal/`.** `provider`, `credential`, and `core` are under
   `harness/internal/`, so they cannot be imported piecemeal by another module even if a
   consumer wanted just the adapters or just the credential layer. (Today this is mitigated by
   the config-driven factory + `harnessapi`, so it is the *least* pressing blocker — but it
   constrains finer-grained reuse.)

## 5. Proposed shape — a lean "engine", adoptable in phases

None of this requires changing Stirrup's behaviour; it is **packaging and surface** work. Listed
smallest-first so Stirrup can stop at whatever depth is worth it.

- **Phase 1 — an executor-optional factory.** Add a real `none` executor (an `Executor` that
  errors on every I/O call) and let `BuildLoop*` accept the absence of an executor the way it
  already accepts a `NullTransport`. This alone makes a read-only, no-workspace run a
  first-class configuration. Low risk; no dependency change.
- **Phase 2 — a one-shot helper on `harnessapi`.** A `Complete(ctx, *RunConfig) (text, usage,
  error)` (or `RunOnce`) that performs a single tool-less model turn and returns the text +
  token usage, bypassing the agentic loop. Serves "judgement call" consumers (Chiron's lead)
  and is a natural place to later add optional provider-native structured output. Small,
  additive.
- **Phase 3 — a dependency-light `engine` module.** Split the model-loop core (provider
  adapters + credential federation + the loop + context strategies + budgets + the OTel
  tracing they emit) into a module whose `go.mod` does **not** force `k8s.io/*`, the AWS SDK, or
  `cedar-go` on importers. Options, in rough order of effort: (a) move the K8s executor,
  container runtime, and Cedar policy engine behind **build tags** so they are not compiled
  for an engine-only consumer; or (b) extract a separate module (e.g.
  `github.com/rxbynerd/stirrup/engine`, alongside the existing `types`/`gen`/`harness`/`eval`)
  that the full `harness` then composes. The existing zero-dep `types` module shows the seam is
  feasible. This is the phase that actually removes the dependency-weight blocker (§4.1).
- **Phase 4 — document `harnessapi` as a supported library contract.** Today its doc comment
  scopes it to *"the stable control plane's local provisioner"* (an internal use). If embedding
  is to be a real consumption mode, give it a stability statement and a short "embed Stirrup
  in-process" guide, so external consumers can rely on it across releases.

A consumer like Chiron could then: import the `engine` module; build a `RunConfig` with
`mode:"research"`, `executor:"none"`, `tools:{built_in:["web_fetch"], mcp_servers:[search]}`,
one `provider` + a credential source; and run the loop in-process, one goroutine per worker —
getting the adapters, credential federation, budgets, and tracing, with **none** of the
K8s/sandbox/coding surface.

## 6. What this deliberately does NOT change

- **No behaviour change.** Modes, validation (`ValidateRunConfig`), the loop semantics, and the
  provider adapters are untouched; this is packaging + two additive entrypoints.
- **The remote K8s-job model stays the flagship.** `stirrup job` + the `HarnessService` wire
  contract remain Stirrup's primary, coding-oriented deployment shape. The engine is an
  *additional* consumption mode for read-only / non-sandbox consumers, not a replacement.
- **`spawn_agent`, executors, git, edit tools, the policy engine** all remain — they are simply
  not *forced onto* an engine-only importer.

## 7. Trade-offs, and good reasons Stirrup might decline

- **Maintenance surface.** A documented, stability-guaranteed library API is a real ongoing
  commitment; Stirrup may prefer to keep `harnessapi` internal and evolve it freely.
- **Module-split cost.** Carving a lean `engine` module (or threading build tags through the
  executor/policy wiring) is non-trivial and touches the factory — a core file.
- **Single-consumer risk.** If Chiron is the only consumer, the work may not pay for itself;
  it is most justified if Stirrup wants to be a **multi-consumer platform** with embedding as a
  first-class story. (Chiron is happy to be the forcing function and first adopter, but cannot
  on its own justify the investment for Stirrup.)
- **Isolation philosophy.** Stirrup may *intend* the K8s-job boundary as a universal safety
  property (every task isolated), in which case "embed in-process" is deliberately not offered.

If any of these outweigh the benefit for Stirrup, declining is the right call. Chiron's v2 ships
on its own in-process loop either way, and would adopt the engine later **only if** it lands and
multi-model scale justifies the swap (which, by design, is a binding change behind Chiron's
`Researcher` seam — not a re-architecture).

## 8. Grounding (read from Stirrup `main` @ f12bedb, 2026-06-22)

- `harness/harnessapi/harnessapi.go:1-5,29-45` — the public in-process embedding API and
  `BuildLoopWithTransport` / `Loop.Run` / `Loop.Close`.
- `harness/internal/core/subagent.go` — the loop run in-process with capture/`NullTransport`,
  `NoneVerifier`, `NoneGitStrategy` (in-process feasibility); child reuses `parent.Executor`.
- `harness/internal/core/factory.go` (`buildExecutor`) — executor types `local`/`container`/
  `k8s`/`k8s-sandbox`/`api`; **no `none`** path.
- `harness/go.mod:6-12,35-39` (and `:12`) — `aws-sdk-go-v2` family, `k8s.io/*`, and
  `cedar-policy/cedar-go` as **direct** dependencies inherited by any importer of `harness`.
- `types/go.mod` — zero-dependency module; `go.work` — the `types`/`gen`/`harness`/`eval`
  multi-module layout that makes a lean `engine` module a natural fit.
- `types/result.go:25-71` — `RunResult` is free-text (`FinalAssistantText`), no output-schema;
  motivates the optional one-shot/structured helper (Phase 2).
- `types/runconfig.go` — read-only modes (`planning`/`review`/`research`/`toil`) and
  `ToolsConfig` (optional `built_in` / `mcp_servers`), the basis for an `executor:"none"`,
  web-only `RunConfig`.

## 9. References

- `chiron/docs/V2-RESEARCH-AGENT.md` §0a — the Chiron prove-first pivot that produced this
  proposal (the in-process loop now; Stirrup-as-embedded-engine later).
- `chiron/docs/V2-PLAN.md` D9 + "Scale-out track (deferred)" — where Chiron would adopt this,
  if it lands.
- Stirrup `README.md`, `docs/deployment.md`, `docs/architecture.md`, `docs/providers.md`,
  `docs/credential-federation.md` — the current (coding-agent, remote-K8s-job) model.
