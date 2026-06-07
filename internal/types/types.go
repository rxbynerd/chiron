// Package types holds the shared data types that cross Chiron's seams:
// the Interaction returned by a Researcher, the Report produced by a
// Formatter, and the RunResult summarising one research run. Keeping them
// here lets every seam package depend on types without depending on each
// other.
package types

import "time"

// Status is the lifecycle state of an interaction.
type Status string

// Interaction statuses, mirroring the Interactions API lifecycle
// (in_progress until the agent finishes, then completed or failed).
const (
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
)

// Interaction is the result of one research task. It is the unit a
// Researcher produces and a Formatter consumes. The ID doubles as the
// resume handle: state is held server-side, so a crashed run recovers
// with `chiron get <id>` and Chiron keeps no local state.
type Interaction struct {
	ID          string     `json:"id"`
	Agent       string     `json:"agent,omitempty"`
	Query       string     `json:"query,omitempty"`
	Status      Status     `json:"status"`
	Outputs     []Output   `json:"outputs,omitempty"`
	Citations   []Citation `json:"citations,omitempty"`
	Usage       Usage      `json:"usage,omitzero"`
	CreatedAt   time.Time  `json:"created_at,omitzero"`
	CompletedAt time.Time  `json:"completed_at,omitzero"`
}

// OutputType discriminates the entries of Interaction.Outputs.
type OutputType string

// Output types produced by a research agent. The final report is the last
// text output; image outputs carry agent-generated charts.
const (
	OutputText           OutputType = "text"
	OutputImage          OutputType = "image"
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

// Citation is one source the agent cited, kept for the report's sources
// section and for verification.
type Citation struct {
	URI   string `json:"uri"`
	Title string `json:"title,omitempty"`
}

// Usage records per-run cost signals. Chiron tracks the signals and
// enforces budget caps; pricing tables and attribution are deferred to
// Stint (a suite concern, not Chiron's).
type Usage struct {
	InputTokens      int     `json:"input_tokens,omitempty"`
	OutputTokens     int     `json:"output_tokens,omitempty"`
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
