// Package types holds the shared data types that cross Chiron's seams:
// the Interaction returned by a Researcher, the Report produced by a
// Formatter, and the RunResult summarising one research run. Keeping them
// here lets every seam package depend on types without depending on each
// other.
//
// The Interaction family models the wire shapes in
// docs/INTERACTIONS-API.md §4 (normative for this repository — the live
// API has drifted from PROPOSAL.md §3; see the reference's §8). JSON tags
// mirror the wire names exactly.
package types

import "time"

// Status is the lifecycle state of an interaction. The full enum per
// docs/INTERACTIONS-API.md §4; v1 treats everything other than
// in_progress and completed as a terminal-failure variant.
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

// Terminal reports whether the status ends a run. Everything other than
// in_progress is terminal — including requires_action, which should not
// occur for deep research but must not hang a poller if it does
// (docs/INTERACTIONS-API.md §4). An empty status is unknown, not terminal.
func (s Status) Terminal() bool {
	return s != "" && s != StatusInProgress
}

// Interaction is the result of one research task, mirroring the
// Interaction resource (docs/INTERACTIONS-API.md §4). The ID doubles as
// the resume handle: state is held server-side (store: true), so a
// crashed run recovers with `chiron get <id>` and Chiron keeps no local
// state.
type Interaction struct {
	ID     string `json:"id"`
	Object string `json:"object,omitempty"`
	// Agent is the agent identifier for research tasks; Model is set
	// instead on follow-up Q&A interactions (reference §3).
	Agent                 string    `json:"agent,omitempty"`
	Model                 string    `json:"model,omitempty"`
	Status                Status    `json:"status"`
	Created               time.Time `json:"created,omitzero"`
	Updated               time.Time `json:"updated,omitzero"`
	Steps                 []Step    `json:"steps,omitempty"`
	Usage                 Usage     `json:"usage,omitzero"`
	PreviousInteractionID string    `json:"previous_interaction_id,omitempty"`
}

// FinalOutput returns the last model_output step — the one carrying the
// final report text, chart images, and citation annotations
// (docs/INTERACTIONS-API.md §4) — or false if there is none yet.
func (in *Interaction) FinalOutput() (*Step, bool) {
	for i := len(in.Steps) - 1; i >= 0; i-- {
		if in.Steps[i].Type == StepModelOutput {
			return &in.Steps[i], true
		}
	}
	return nil, false
}

// StepType discriminates the entries of Interaction.Steps.
type StepType string

// Step types. The API also emits tool-call/result step types; consumers
// must tolerate values beyond these.
const (
	StepUserInput   StepType = "user_input"
	StepModelOutput StepType = "model_output"
)

// Step is one entry in an interaction: user input, model output, or a
// tool call/result.
type Step struct {
	Type    StepType  `json:"type"`
	Content []Content `json:"content,omitempty"`
}

// ContentType discriminates content parts.
type ContentType string

// Content part types. text and thought carry Text; image carries
// MIMEType plus Data or URI; document (request-side grounding) carries
// URI and MIMEType.
const (
	ContentText     ContentType = "text"
	ContentThought  ContentType = "thought"
	ContentImage    ContentType = "image"
	ContentDocument ContentType = "document"
)

// Content is one typed part of a step. Data is base64 on the wire, which
// encoding/json maps to []byte automatically.
type Content struct {
	Type        ContentType  `json:"type"`
	Text        string       `json:"text,omitempty"`
	Annotations []Annotation `json:"annotations,omitempty"`
	MIMEType    string       `json:"mime_type,omitempty"`
	Data        []byte       `json:"data,omitempty"`
	URI         string       `json:"uri,omitempty"`
}

// AnnotationType discriminates citation annotations.
type AnnotationType string

// Annotation types.
const (
	AnnotationURLCitation  AnnotationType = "url_citation"
	AnnotationFileCitation AnnotationType = "file_citation"
)

// Annotation is a citation attached to a text content part, spanning
// [StartIndex, EndIndex) bytes of its Text. url_citation carries URL and
// Title; file_citation carries DocumentURI, FileName and PageNumber.
// Deduplicate by URL for a sources list.
type Annotation struct {
	Type        AnnotationType `json:"type"`
	URL         string         `json:"url,omitempty"`
	Title       string         `json:"title,omitempty"`
	DocumentURI string         `json:"document_uri,omitempty"`
	FileName    string         `json:"file_name,omitempty"`
	PageNumber  int            `json:"page_number,omitempty"`
	StartIndex  int            `json:"start_index,omitempty"`
	EndIndex    int            `json:"end_index,omitempty"`
}

// Usage is the interaction's usage block: token totals plus per-tool
// grounding counts. These are the per-run cost signals Chiron tracks;
// pricing tables and attribution are deferred to Stint.
type Usage struct {
	TotalInputTokens   int                  `json:"total_input_tokens,omitempty"`
	TotalCachedTokens  int                  `json:"total_cached_tokens,omitempty"`
	TotalOutputTokens  int                  `json:"total_output_tokens,omitempty"`
	TotalToolUseTokens int                  `json:"total_tool_use_tokens,omitempty"`
	TotalThoughtTokens int                  `json:"total_thought_tokens,omitempty"`
	TotalTokens        int                  `json:"total_tokens,omitempty"`
	GroundingToolCount []GroundingToolCount `json:"grounding_tool_count,omitempty"`
}

// GroundingToolCount is the number of times one grounding tool ran — the
// search-count cost signal.
type GroundingToolCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
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
// (--output json) and for the end-of-run cost summary. Usage carries the
// API's signals; the remaining fields are Chiron-side run telemetry.
type RunResult struct {
	InteractionID    string        `json:"interaction_id"`
	Status           Status        `json:"status"`
	Report           *Report       `json:"report,omitempty"`
	Usage            Usage         `json:"usage,omitzero"`
	EstimatedCostGBP float64       `json:"estimated_cost_gbp,omitempty"`
	PollCount        int           `json:"poll_count,omitempty"`
	ReconnectCount   int           `json:"reconnect_count,omitempty"`
	Duration         time.Duration `json:"duration_ns,omitempty"`
}
