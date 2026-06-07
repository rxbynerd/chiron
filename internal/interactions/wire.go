package interactions

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// Status is the lifecycle state of an interaction: the full enum per
// docs/INTERACTIONS-API.md §4. v1 treats everything other than
// in_progress and completed as a terminal-failure variant.
//
// The seven wire strings are identical to internal/types.Status, so the
// wire-to-domain mapping in researcher/gemini is a plain string
// conversion; Terminal() semantics match too.
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
// in_progress and requires_action, matching types.Status.Terminal.
// requires_action should not occur for deep research (no custom function
// tools, docs/INTERACTIONS-API.md §3) and is not terminal in itself, but
// PollUntilTerminal still returns promptly with ErrRequiresAction rather
// than hanging on it. An empty status is unknown, not terminal.
func (s Status) Terminal() bool {
	return s != "" && s != StatusInProgress && s != StatusRequiresAction
}

// Failed reports whether the status is a terminal-failure variant:
// terminal but not completed (failed, cancelled, incomplete or
// budget_exceeded).
func (s Status) Failed() bool {
	return s.Terminal() && s != StatusCompleted
}

// CreateRequest is the POST /v1beta/interactions body
// (docs/INTERACTIONS-API.md §3).
//
// Deep research requires Background: true, which in turn requires
// Store: true — Client.Create enforces the pair before spending a
// request. Follow-up Q&A against a completed interaction sets Model (not
// Agent) plus PreviousInteractionID.
type CreateRequest struct {
	// Agent is the research agent identifier (AgentDeepResearch or
	// AgentDeepResearchMax). Zero for follow-up Q&A, which uses Model.
	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
	// Input is string | []Content | []Step on the wire; a plain string
	// is the common case. Multimodal grounding passes []Content with
	// typed parts (text, image, document).
	Input any `json:"input"`
	// AgentConfig configures the research agent; nil for follow-up Q&A.
	AgentConfig *AgentConfig `json:"agent_config,omitempty"`
	// Tools overrides the default set — google_search, url_context and
	// code_execution are applied server-side when omitted.
	Tools []Tool `json:"tools,omitempty"`
	// Background, Store and Stream are emitted explicitly, never
	// omitted: their server-side defaults differ, and a silently
	// defaulted field on a paid run is exactly the drift the pinned
	// Api-Revision header guards against.
	Background bool `json:"background"`
	Store      bool `json:"store"`
	Stream     bool `json:"stream"`
	// PreviousInteractionID chains plan-refinement and follow-up
	// requests to a stored interaction.
	PreviousInteractionID string `json:"previous_interaction_id,omitempty"`
}

// Agent config field values (docs/INTERACTIONS-API.md §3).
const (
	// AgentConfigDeepResearch is the required AgentConfig.Type
	// discriminator.
	AgentConfigDeepResearch = "deep-research"

	ThinkingSummariesAuto = "auto"
	ThinkingSummariesNone = "none"

	VisualizationAuto = "auto"
	VisualizationOff  = "off"
)

// AgentConfig configures the research agent (docs/INTERACTIONS-API.md
// §3). Field names follow the wire spelling.
type AgentConfig struct {
	// Type is the required discriminator: AgentConfigDeepResearch.
	Type string `json:"type"`
	// ThinkingSummaries is auto or none (API default none).
	ThinkingSummaries string `json:"thinking_summaries,omitempty"`
	// Visualization is auto or off (API default auto).
	Visualization string `json:"visualization,omitempty"`
	// CollaborativePlanning is emitted explicitly because the planning
	// flow flips it per request: true to plan and refine, false to
	// approve (docs/INTERACTIONS-API.md §7).
	CollaborativePlanning bool `json:"collaborative_planning"`
}

// Tool types (docs/INTERACTIONS-API.md §3). Custom function tools are
// not supported by the deep-research agent.
const (
	ToolGoogleSearch  = "google_search"
	ToolURLContext    = "url_context"
	ToolCodeExecution = "code_execution"
	ToolMCPServer     = "mcp_server"
	ToolFileSearch    = "file_search"
)

// Tool is one tool the agent may use. Only the fields for the given Type
// are set: mcp_server carries Name, URL, Headers and AllowedTools;
// file_search carries FileSearchStoreNames and TopK.
type Tool struct {
	Type                 string            `json:"type"`
	Name                 string            `json:"name,omitempty"`
	URL                  string            `json:"url,omitempty"`
	Headers              map[string]string `json:"headers,omitempty"`
	AllowedTools         *AllowedTools     `json:"allowed_tools,omitempty"`
	FileSearchStoreNames []string          `json:"file_search_store_names,omitempty"`
	TopK                 int               `json:"top_k,omitempty"`
}

// AllowedTools restricts which tools of an MCP server the agent may
// call. Mode is auto, any, none or validated.
type AllowedTools struct {
	Mode  string   `json:"mode,omitempty"`
	Tools []string `json:"tools,omitempty"`
}

// UnmarshalJSON accepts both documented forms: the API reference's
// object ({"mode":...,"tools":[...]}) and the deep-research guide's bare
// array of tool names. docs/INTERACTIONS-API.md §3 says to prefer the
// object form but tolerate both when parsing.
func (a *AllowedTools) UnmarshalJSON(data []byte) error {
	if d := bytes.TrimSpace(data); len(d) > 0 && d[0] == '[' {
		return json.Unmarshal(d, &a.Tools)
	}
	type plain AllowedTools
	return json.Unmarshal(data, (*plain)(a))
}

// Interaction is the Interaction resource (docs/INTERACTIONS-API.md §4).
// Because background interactions are stored server-side (store: true),
// the ID doubles as the resume handle across crashes and reconnects.
// JSON tags mirror the wire names exactly.
type Interaction struct {
	ID     string `json:"id"`
	Object string `json:"object,omitempty"`
	// Agent is set on research interactions; Model is set instead on
	// follow-up Q&A interactions (docs/INTERACTIONS-API.md §3).
	Agent                 string    `json:"agent,omitempty"`
	Model                 string    `json:"model,omitempty"`
	Status                Status    `json:"status,omitempty"`
	Created               time.Time `json:"created,omitzero"`
	Updated               time.Time `json:"updated,omitzero"`
	Steps                 []Step    `json:"steps,omitempty"`
	Usage                 Usage     `json:"usage,omitzero"`
	PreviousInteractionID string    `json:"previous_interaction_id,omitempty"`
}

// FinalOutput returns the last model_output step — the one carrying the
// final report text, chart images and citation annotations
// (docs/INTERACTIONS-API.md §4) — or false if there is none yet.
func (in *Interaction) FinalOutput() (*Step, bool) {
	for i := len(in.Steps) - 1; i >= 0; i-- {
		if in.Steps[i].Type == StepModelOutput {
			return &in.Steps[i], true
		}
	}
	return nil, false
}

// FinalText returns the final report: the text content of the last
// model_output step (docs/INTERACTIONS-API.md §4). Multiple text parts
// are joined with blank lines; thought parts are excluded. Empty until a
// model_output step with text content exists.
func (in *Interaction) FinalText() string {
	step, ok := in.FinalOutput()
	if !ok {
		return ""
	}
	var parts []string
	for _, c := range step.Content {
		if c.Type == ContentText && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Images returns the image content parts — agent-generated charts — of
// every model_output step, in order (docs/INTERACTIONS-API.md §4).
func (in *Interaction) Images() []Content {
	var images []Content
	for _, s := range in.Steps {
		if s.Type != StepModelOutput {
			continue
		}
		for _, c := range s.Content {
			if c.Type == ContentImage {
				images = append(images, c)
			}
		}
	}
	return images
}

// Citation is one deduplicated source reference derived from citation
// annotations, shaped to map directly onto the domain model's
// types.Citation{URI, Title}: url_citation contributes URL/Title,
// file_citation contributes DocumentURI/FileName.
type Citation struct {
	URI   string
	Title string
}

// Citations returns the sources list: every url_citation and
// file_citation annotation across the text parts of every model_output
// step, deduplicated by URI (URL or document_uri) in first-seen order
// (docs/INTERACTIONS-API.md §4). Annotations without a URI are skipped.
func (in *Interaction) Citations() []Citation {
	var citations []Citation
	seen := make(map[string]bool)
	for _, s := range in.Steps {
		if s.Type != StepModelOutput {
			continue
		}
		for _, c := range s.Content {
			if c.Type != ContentText {
				continue
			}
			for _, a := range c.Annotations {
				var cite Citation
				switch a.Type {
				case AnnotationURLCitation:
					cite = Citation{URI: a.URL, Title: a.Title}
				case AnnotationFileCitation:
					cite = Citation{URI: a.DocumentURI, Title: a.FileName}
				default:
					continue
				}
				if cite.URI == "" || seen[cite.URI] {
					continue
				}
				seen[cite.URI] = true
				citations = append(citations, cite)
			}
		}
	}
	return citations
}

// URLCitations returns the raw url_citation annotations — byte offsets
// included — across the text parts of every model_output step,
// deduplicated by URL in first-seen order. Citations is the
// domain-shaped sources list; this keeps the offset detail for callers
// that need to anchor citations within the text.
func (in *Interaction) URLCitations() []Annotation {
	var citations []Annotation
	seen := make(map[string]bool)
	for _, s := range in.Steps {
		if s.Type != StepModelOutput {
			continue
		}
		for _, c := range s.Content {
			if c.Type != ContentText {
				continue
			}
			for _, a := range c.Annotations {
				if a.Type != AnnotationURLCitation || seen[a.URL] {
					continue
				}
				seen[a.URL] = true
				citations = append(citations, a)
			}
		}
	}
	return citations
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
// grounding counts — the per-run cost signals
// (docs/INTERACTIONS-API.md §4).
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
