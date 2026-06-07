// Package secret defines the Resolver seam for secret:// references.
// Secrets never live in configuration: ResearchConfig carries references
// such as secret://GEMINI_API_KEY, never literal keys, and a
// credential-pattern scrubber wraps the logger so leakage is structurally
// impossible. v1 binds env and file resolvers; v2 adds GCP Secret Manager.
package secret

import "context"

// Resolver dereferences a secret:// reference to its value. Resolved
// values must never be written to configuration, logs, or traces.
type Resolver interface {
	// Resolve returns the secret value for a reference such as
	// secret://GEMINI_API_KEY. It fails if the reference is malformed or
	// the secret is absent — never by returning an empty value.
	Resolve(ctx context.Context, ref string) (string, error)
}
