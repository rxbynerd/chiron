// Package interactions is Chiron's hand-rolled net/http client for the
// Gemini Interactions API. It owns the wire-format types — create
// request, Interaction resource, SSE event envelopes — per
// docs/INTERACTIONS-API.md §3–§6, which is normative for this package:
// the live API has drifted from docs/PROPOSAL.md §3 (see the reference's
// §8) and where they disagree the reference wins.
//
// The package depends on the standard library only — no vendor AI SDKs
// (AGENTS.md ground rules) — so every request built and byte parsed is
// auditable. internal/types holds the separate, stable domain model;
// mapping wire to domain is the Gemini researcher adapter's concern, and
// the Interaction accessors (FinalText, Images, URLCitations) exist to
// make that mapping straightforward.
package interactions

const (
	// DefaultBaseURL is the production API host
	// (docs/INTERACTIONS-API.md §1).
	DefaultBaseURL = "https://generativelanguage.googleapis.com"

	// APIRevision pins the schema revision via the Api-Revision header
	// on every request, protecting against silent drift in a beta API
	// (docs/INTERACTIONS-API.md §1).
	APIRevision = "2026-05-20"

	// LastVerified records when this package's wire shapes were last
	// verified against docs/INTERACTIONS-API.md (whose own last_verified
	// carries the same date against Google's published docs). The API is
	// beta and actively drifting: if this date is stale, re-verify the
	// reference before trusting these shapes.
	LastVerified = "2026-06-07"
)

// Agent identifiers (docs/INTERACTIONS-API.md §2). Mapping Chiron's tier
// flag values (deep-research, deep-research-max) to these wire IDs is the
// researcher adapter's concern.
const (
	AgentDeepResearch    = "deep-research-preview-04-2026"
	AgentDeepResearchMax = "deep-research-max-preview-04-2026"
)
