# Security

## Posture

Chiron is research-only by design: it never mutates a workspace, runs no
shell, and applies no edits. Its only capabilities are read-only research
tools. This bounds the blast radius of any compromise to data egress, and
is why Chiron carries none of Stirrup's executor/permission/safety-ring
machinery.

## Secrets

- Configuration carries `secret://` references (for example
  `secret://GEMINI_API_KEY`), never literal keys; `ResearchConfig`
  validation rejects anything else, and the rejection error never echoes
  the offending value — it may itself be the credential.
- Secrets are resolved at the `internal/secret` seam (env and file
  backends) at the last moment before use. File-backed secrets whose
  permissions admit group or world access trigger a warning.
- A credential-pattern scrubber (`secret.Scrub`: header forms, bearer
  tokens, Google API key shapes, a high-entropy backstop) wraps every
  output path — the slog handler, both tracer bindings, the CLI's stderr
  error line, and streamed thought-summary deltas, which can quote
  credentials found in `--input` documents back out.

## Network

- The API endpoint is fixed unless `CHIRON_GEMINI_BASE_URL` is set; the
  override is validated before any client exists — absolute `https://`
  anywhere, `http://` for loopback hosts only — because the API key
  travels in a header to whatever the variable names. It exists for the
  httptest smoke tests and must never be set in production.
- The Interactions client refuses cross-host redirects (Go strips only
  its own sensitive headers on redirect, not custom ones like
  `x-goog-api-key`), enforced even over a caller-supplied `http.Client`.
- Every body read is bounded via `io.LimitReader` — responses, SSE
  events, error bodies, and local `--input` files — so a misbehaving
  endpoint or oversized document cannot exhaust memory; over-bound reads
  fail loudly rather than truncating silently.

## Filesystem

- Reports and chart assets are written owner-only (`0600`).
- Chiron keeps no local state: resume handles live server-side.

## Supply chain

- The dependency surface is deliberately minimal and auditable: stdlib,
  cobra (+pflag), yaml.v3, and the OpenTelemetry SDK (justified in
  `docs/DECISIONS.md`). Provider adapters are hand-rolled `net/http`
  against documented REST APIs — no vendor AI SDKs — so every line that
  touches a credential is in this repository.
- CI actions are pinned to full commit SHAs, not mutable tags.

## Reporting a vulnerability

Open a private security advisory on GitHub
(https://github.com/rxbynerd/chiron/security/advisories) rather than a
public issue. Reports are acknowledged on a best-effort basis.
