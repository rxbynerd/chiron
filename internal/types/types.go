// Package types holds the shared data types that cross Chiron's seams:
// the Interaction returned by a Researcher, the Report produced by a
// Formatter, and the RunResult summarising one research run. Keeping them
// here lets every seam package depend on types without depending on each
// other.
//
// These are seam-level DOMAIN types, deliberately decoupled from the
// Interactions API wire format: the wire schema (steps/content/annotations
// per docs/INTERACTIONS-API.md §4) lives in internal/interactions, and
// researcher/gemini maps wire to domain. The API is beta and drifting, so
// churn stays contained in the adapter while these types stay stable (see
// docs/DECISIONS.md, "Wire types vs domain types").
package types

import "time"

// Status is the lifecycle state of an interaction. The set mirrors the
// API's full enum (docs/INTERACTIONS-API.md §4) so the domain model can
// represent every state the adapter reports.
type Status string

// Interaction statuses.
const (
	StatusInProgress     Status = "in_progress"
	StatusRequiresAction Status = "requires_action"
	StatusCompleted      Status = "completed"
	StatusFailed         Status = "failed"
	StatusCancelled      Status = "cancelled"
	StatusIncomplete     Status = "incomplete"
	StatusBudgetExceeded Status = "budget_exceeded"
)

// Terminal reports whether the status ends a run: everything except
// in_progress and requires_action. An empty status is unknown, not
// terminal.
func (s Status) Terminal() bool {
	return s != "" && s != StatusInProgress && s != StatusRequiresAction
}

// Interaction is the result of one research task. It is the unit a
// Researcher produces and a Formatter consumes. The ID doubles as the
// resume handle: state is held server-side, so a crashed run recovers
// with `chiron get <id>` and Chiron keeps no local state.
type Interaction struct {
	ID    string `json:"id"`
	Agent string `json:"agent,omitempty"`
	Query string `json:"query,omitempty"`
	// Tools is the tool set the research task actually ran with,
	// populated by the adapter from the resolved create request (not
	// echoed by the API), so the report's front matter can record it
	// (PROPOSAL §4.4). Entries are tool type names; MCP servers appear
	// as "mcp_server:<name>".
	Tools  []string `json:"tools,omitempty"`
	Status Status   `json:"status"`
	// StatusDetail is a human-readable reason accompanying failure or
	// cancellation states, when the adapter has one.
	StatusDetail string     `json:"status_detail,omitempty"`
	Outputs      []Output   `json:"outputs,omitempty"`
	Citations    []Citation `json:"citations,omitempty"`
	Usage        Usage      `json:"usage,omitzero"`
	CreatedAt    time.Time  `json:"created_at,omitzero"`
	CompletedAt  time.Time  `json:"completed_at,omitzero"`
}

// OutputType discriminates the entries of Interaction.Outputs.
type OutputType string

// Output types produced by a research agent. The final report is the last
// text output; image outputs carry agent-generated charts.
const (
	OutputText  OutputType = "text"
	OutputImage OutputType = "image"
	// OutputThoughtSummary carries the agent's reasoning summaries,
	// mapped from wire thought content parts by the gemini adapter.
	// The formatter excludes them from the report body; they surface
	// through --output json and the streaming display.
	OutputThoughtSummary OutputType = "thought_summary"
)

// Output is a single agent output: report text, a thought summary, or an
// image (chart) with its decoded bytes.
type Output struct {
	Type OutputType `json:"type"`
	// Text carries text and thought_summary outputs.
	Text string `json:"text,omitempty"`
	// MIMEType and Data carry image outputs (decoded, not base64).
	MIMEType string `json:"mime_type,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

// Citation is one source the agent cited, deduplicated by the adapter,
// kept for the report's sources section and for verification.
type Citation struct {
	URI   string `json:"uri"`
	Title string `json:"title,omitempty"`
}

// Usage records per-run cost signals: the token and search counts the
// API reports (mapped by the adapter) plus Chiron-side run telemetry.
// Chiron tracks the signals and enforces budget caps; pricing tables and
// attribution are deferred to Stint (a suite concern, not Chiron's).
type Usage struct {
	InputTokens      int     `json:"input_tokens,omitempty"`
	CachedTokens     int     `json:"cached_tokens,omitempty"`
	OutputTokens     int     `json:"output_tokens,omitempty"`
	ToolUseTokens    int     `json:"tool_use_tokens,omitempty"`
	ThoughtTokens    int     `json:"thought_tokens,omitempty"`
	SearchCount      int     `json:"search_count,omitempty"`
	PollCount        int     `json:"poll_count,omitempty"`
	ReconnectCount   int     `json:"reconnect_count,omitempty"`
	EstimatedCostGBP float64 `json:"estimated_cost_gbp,omitempty"`
}

// Report is a formatted research report: a single portable Markdown
// document plus any chart assets to be written alongside it.
type Report struct {
	// Markdown is the full document: YAML front matter, body, charts as
	// relative image links, and a numbered sources section.
	Markdown []byte `json:"markdown"`
	// Assets are chart images referenced from the Markdown by Name.
	Assets []Asset `json:"assets,omitempty"`
}

// Asset is a file written next to the report, typically an agent-generated
// chart referenced from the Markdown as a relative link.
type Asset struct {
	Name     string `json:"name"`
	MIMEType string `json:"mime_type,omitempty"`
	Data     []byte `json:"data"`
}

// RunResult summarises one research run for machine consumption
// (--output json) and for the end-of-run cost summary.
type RunResult struct {
	InteractionID string        `json:"interaction_id"`
	Status        Status        `json:"status"`
	Report        *Report       `json:"report,omitempty"`
	Usage         Usage         `json:"usage,omitzero"`
	Duration      time.Duration `json:"duration_ns,omitempty"`
}
