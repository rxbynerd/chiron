package interactions

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStatusTerminalAndFailed(t *testing.T) {
	tests := []struct {
		status   Status
		terminal bool
		failed   bool
	}{
		{StatusInProgress, false, false},
		{StatusRequiresAction, true, true},
		{StatusCompleted, true, false},
		{StatusFailed, true, true},
		{StatusCancelled, true, true},
		{StatusIncomplete, true, true},
		{StatusBudgetExceeded, true, true},
		{Status(""), false, false},
		{Status("some_future_status"), true, true},
	}
	for _, tt := range tests {
		if got := tt.status.Terminal(); got != tt.terminal {
			t.Errorf("Status(%q).Terminal() = %v, want %v", tt.status, got, tt.terminal)
		}
		if got := tt.status.Failed(); got != tt.failed {
			t.Errorf("Status(%q).Failed() = %v, want %v", tt.status, got, tt.failed)
		}
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
	var in Interaction
	if err := json.Unmarshal([]byte(referenceExample), &in); err != nil {
		t.Fatalf("decoding reference example: %v", err)
	}
	if in.ID != "v1_abc" || in.Object != "interaction" || in.Agent != AgentDeepResearch {
		t.Errorf("identity fields = %q/%q/%q", in.ID, in.Object, in.Agent)
	}
	if in.Status != StatusCompleted {
		t.Errorf("Status = %q, want completed", in.Status)
	}
	if want := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC); !in.Created.Equal(want) {
		t.Errorf("Created = %v, want %v", in.Created, want)
	}
	if len(in.Steps) != 2 || in.Steps[0].Type != StepUserInput || in.Steps[1].Type != StepModelOutput {
		t.Fatalf("steps = %+v", in.Steps)
	}
	if in.PreviousInteractionID != "v1_prev" {
		t.Errorf("PreviousInteractionID = %q", in.PreviousInteractionID)
	}
	if in.Usage.TotalTokens != 320200 || in.Usage.TotalCachedTokens != 150000 {
		t.Errorf("usage totals = %+v", in.Usage)
	}
	if len(in.Usage.GroundingToolCount) != 1 || in.Usage.GroundingToolCount[0].Count != 80 {
		t.Errorf("grounding counts = %+v", in.Usage.GroundingToolCount)
	}

	if got := in.FinalText(); got != "# Report on the topic" {
		t.Errorf("FinalText() = %q", got)
	}
	images := in.Images()
	if len(images) != 1 || images[0].MIMEType != "image/png" || string(images[0].Data) != "png-bytes" {
		t.Errorf("Images() = %+v", images)
	}
	citations := in.URLCitations()
	if len(citations) != 1 || citations[0].URL != "https://example.org/a" || citations[0].EndIndex != 20 {
		t.Errorf("URLCitations() = %+v", citations)
	}
}

func TestFinalTextJoinsTextPartsAndSkipsThoughts(t *testing.T) {
	in := Interaction{Steps: []Step{
		{Type: StepUserInput, Content: []Content{{Type: ContentText, Text: "query"}}},
		{Type: StepModelOutput, Content: []Content{{Type: ContentText, Text: "draft"}}},
		{Type: StepModelOutput, Content: []Content{
			{Type: ContentThought, Text: "thinking"},
			{Type: ContentText, Text: "Part one."},
			{Type: ContentText, Text: "Part two."},
		}},
	}}
	if got, want := in.FinalText(), "Part one.\n\nPart two."; got != want {
		t.Errorf("FinalText() = %q, want %q", got, want)
	}

	var empty Interaction
	if got := empty.FinalText(); got != "" {
		t.Errorf("FinalText() on empty interaction = %q, want empty", got)
	}
	if _, ok := empty.FinalOutput(); ok {
		t.Error("FinalOutput() on empty interaction reported ok")
	}
}

func TestURLCitationsDedupesByURL(t *testing.T) {
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
	if len(got) != 2 || got[0].URL != "https://x" || got[1].URL != "https://y" {
		t.Errorf("URLCitations() = %+v, want x then y", got)
	}
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
			var got AllowedTools
			if err := json.Unmarshal([]byte(tt.json), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Mode != tt.want.Mode || strings.Join(got.Tools, ",") != strings.Join(tt.want.Tools, ",") {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCreateRequestEmitsExplicitBooleans(t *testing.T) {
	data, err := json.Marshal(&CreateRequest{
		Agent:       AgentDeepResearch,
		Input:       "q",
		AgentConfig: &AgentConfig{Type: AgentConfigDeepResearch},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	for _, key := range []string{"background", "store", "stream", "input"} {
		if _, ok := got[key]; !ok {
			t.Errorf("create request missing explicit %q: %s", key, data)
		}
	}
	cfg, ok := got["agent_config"].(map[string]any)
	if !ok {
		t.Fatalf("agent_config missing: %s", data)
	}
	if _, ok := cfg["collaborative_planning"]; !ok {
		t.Errorf("agent_config missing explicit collaborative_planning: %s", data)
	}
}
