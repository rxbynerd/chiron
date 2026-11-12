package interactions

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatusTerminalAndFailed(t *testing.T) {
	tests := []struct {
		name     string
		status   Status
		terminal bool
		failed   bool
	}{
		{"in progress", StatusInProgress, false, false},
		{"requires action", StatusRequiresAction, false, false}, // handled via ErrRequiresAction, matching types.Status
		{"completed", StatusCompleted, true, false},
		{"failed", StatusFailed, true, true},
		{"cancelled", StatusCancelled, true, true},
		{"incomplete", StatusIncomplete, true, true},
		{"budget exceeded", StatusBudgetExceeded, true, true},
		{"empty status", Status(""), false, false},
		{"unknown future status", Status("some_future_status"), true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.terminal, tt.status.Terminal())
			must.Equal(tt.failed, tt.status.Failed())
		})
	}
}

// referenceExample mirrors docs/INTERACTIONS-API.md §4 (with valid
// base64 in place of the doc's placeholder) and pins the wire decode.
const referenceExample = `{
  "id": "v1_abc",
  "object": "interaction",
  "agent": "deep-research-preview-04-2026",
  "status": "completed",
  "created": "2026-06-07T12:00:00Z",
  "updated": "2026-06-07T12:19:00Z",
  "steps": [
    { "type": "user_input", "content": [ {"type": "text", "text": "Research query text"} ] },
    { "type": "model_output", "content": [
        { "type": "thought", "text": "Planning the research..." },
        { "type": "text", "text": "# Report on the topic", "annotations": [
            { "type": "url_citation", "url": "https://example.org/a", "title": "Source A",
              "start_index": 0, "end_index": 20 } ] },
        { "type": "image", "data": "cG5nLWJ5dGVz", "mime_type": "image/png" }
    ] }
  ],
  "usage": {
    "total_input_tokens": 250000, "total_cached_tokens": 150000, "total_output_tokens": 60000,
    "total_tool_use_tokens": 1200, "total_thought_tokens": 9000, "total_tokens": 320200,
    "grounding_tool_count": [ {"type": "google_search", "count": 80} ]
  },
  "previous_interaction_id": "v1_prev"
}`

func TestInteractionDecodesReferenceExample(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var in Interaction
	must.NoError(json.Unmarshal([]byte(referenceExample), &in), "decoding reference example")

	check.Equal("v1_abc", in.ID)
	check.Equal("interaction", in.Object)
	check.Equal(AgentDeepResearch, in.Agent)
	check.Equal(StatusCompleted, in.Status)
	wantCreated := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	check.True(in.Created.Equal(wantCreated), "Created = %v, want %v", in.Created, wantCreated)

	must.Len(in.Steps, 2, "steps = %+v", in.Steps)
	must.Equal(StepUserInput, in.Steps[0].Type)
	must.Equal(StepModelOutput, in.Steps[1].Type)

	check.Equal("v1_prev", in.PreviousInteractionID)
	check.Equal(320200, in.Usage.TotalTokens)
	check.Equal(150000, in.Usage.TotalCachedTokens)
	if counts := in.Usage.GroundingToolCount; check.Len(counts, 1, "grounding counts = %+v", counts) {
		check.Equal(80, counts[0].Count)
	}

	check.Equal("# Report on the topic", in.FinalText())

	if images := in.Images(); check.Len(images, 1, "Images() = %+v", images) {
		check.Equal("image/png", images[0].MIMEType)
		check.Equal("png-bytes", string(images[0].Data))
	}

	if citations := in.URLCitations(); check.Len(citations, 1, "URLCitations() = %+v", citations) {
		check.Equal("https://example.org/a", citations[0].URL)
		check.Equal(20, citations[0].EndIndex)
	}
}

func TestFinalText_JoinsTextAndSkipsThoughts(t *testing.T) {
	check := assert.New(t)

	in := Interaction{Steps: []Step{
		{Type: StepUserInput, Content: []Content{{Type: ContentText, Text: "query"}}},
		{Type: StepModelOutput, Content: []Content{{Type: ContentText, Text: "draft"}}},
		{Type: StepModelOutput, Content: []Content{
			{Type: ContentThought, Text: "thinking"},
			{Type: ContentText, Text: "Part one."},
			{Type: ContentText, Text: "Part two."},
		}},
	}}
	check.Equal("Part one.\n\nPart two.", in.FinalText())

	var empty Interaction
	check.Empty(empty.FinalText(), "FinalText() on empty interaction")
	_, ok := empty.FinalOutput()
	check.False(ok, "FinalOutput() on empty interaction")
}

func TestURLCitations_DedupesByURL(t *testing.T) {
	must := require.New(t)

	cite := func(u string) Annotation { return Annotation{Type: AnnotationURLCitation, URL: u} }
	in := Interaction{Steps: []Step{
		{Type: StepModelOutput, Content: []Content{
			{Type: ContentText, Text: "a", Annotations: []Annotation{cite("https://x"), cite("https://x")}},
			{Type: ContentText, Text: "b", Annotations: []Annotation{
				cite("https://y"),
				{Type: AnnotationFileCitation, FileName: "notes.pdf"},
			}},
		}},
	}}
	got := in.URLCitations()
	must.Len(got, 2, "URLCitations() = %+v", got)
	must.Equal("https://x", got[0].URL)
	must.Equal("https://y", got[1].URL)
}

func TestCitationsMapsBothKindsAndDedupes(t *testing.T) {
	must := require.New(t)

	in := Interaction{Steps: []Step{
		{Type: StepModelOutput, Content: []Content{
			{Type: ContentText, Text: "a", Annotations: []Annotation{
				{Type: AnnotationURLCitation, URL: "https://x", Title: "Source X"},
				{Type: AnnotationFileCitation, DocumentURI: "files/doc1", FileName: "doc1.pdf"},
				{Type: AnnotationURLCitation, URL: "https://x", Title: "Duplicate X"},
				{Type: AnnotationFileCitation}, // no URI: skipped
			}},
			{Type: ContentText, Text: "b", Annotations: []Annotation{
				{Type: AnnotationURLCitation, URL: "https://y", Title: "Source Y"},
			}},
		}},
	}}
	got := in.Citations()
	want := []Citation{
		{URI: "https://x", Title: "Source X"},
		{URI: "files/doc1", Title: "doc1.pdf"},
		{URI: "https://y", Title: "Source Y"},
	}
	must.Equal(want, got)
}

func TestAllowedToolsUnmarshalBothForms(t *testing.T) {
	tests := []struct {
		name string
		json string
		want AllowedTools
	}{
		{"object form", `{"mode":"validated","tools":["search","fetch"]}`,
			AllowedTools{Mode: "validated", Tools: []string{"search", "fetch"}}},
		{"array form", `["search","fetch"]`,
			AllowedTools{Tools: []string{"search", "fetch"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			var got AllowedTools
			must.NoError(json.Unmarshal([]byte(tt.json), &got), "unmarshal")
			must.Equal(tt.want.Mode, got.Mode)
			must.Equal(tt.want.Tools, got.Tools)
		})
	}
}

func TestCreateRequestEmitsExplicitBooleans(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	data, err := json.Marshal(&CreateRequest{
		Agent:       AgentDeepResearch,
		Input:       "q",
		AgentConfig: &AgentConfig{Type: AgentConfigDeepResearch},
	})
	must.NoError(err, "marshal")

	var got map[string]any
	must.NoError(json.Unmarshal(data, &got), "round-trip")

	for _, key := range []string{"background", "store", "stream", "input"} {
		check.Contains(got, key, "create request missing explicit %q: %s", key, data)
	}

	cfg, ok := got["agent_config"].(map[string]any)
	must.True(ok, "agent_config missing: %s", data)
	check.Contains(cfg, "collaborative_planning", "agent_config missing explicit collaborative_planning: %s", data)
}
