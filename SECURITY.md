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
  validation rejects anything else.
- Secrets are resolved at the `internal/secret.Resolver` seam at the last
  moment before use.
- A credential-pattern scrubber wraps the logger and trace payloads so a
  resolved key cannot leak through observability output.

## Dependencies

The dependency surface is deliberately minimal and auditable: stdlib,
cobra (+pflag), yaml.v3. Provider adapters are hand-rolled `net/http`
against documented REST APIs — no vendor SDKs — so every line that
touches a credential is in this repository.

## Reporting a vulnerability

Open a private security advisory on GitHub
(https://github.com/rxbynerd/chiron/security/advisories) rather than a
public issue. Reports are acknowledged on a best-effort basis.
