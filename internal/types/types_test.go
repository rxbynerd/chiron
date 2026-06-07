package types

import (
	"encoding/json"
	"testing"
)

// wireExample is the Interaction resource example from
// docs/INTERACTIONS-API.md §4 — the normative wire shape.
const wireExample = `{
  "id": "v1_abc",
  "object": "interaction",
  "agent": "deep-research-preview-04-2026",
  "status": "completed",
  "created": "2026-06-07T12:00:00Z",
  "updated": "2026-06-07T12:30:00Z",
  "steps": [
    { "type": "user_input", "content": [ {"type": "text", "text": "the query"} ] },
    { "type": "model_output", "content": [
        { "type": "thought", "text": "thinking..." },
        { "type": "text", "text": "# Report", "annotations": [
            { "type": "url_citation", "url": "https://example.com", "title": "Example",
              "start_index": 0, "end_index": 8 } ] },
        { "type": "image", "data": "aGVsbG8=", "mime_type": "image/png" }
    ] }
  ],
  "usage": {
    "total_input_tokens": 250000, "total_cached_tokens": 150000,
    "total_output_tokens": 60000, "total_tool_use_tokens": 1000,
    "total_thought_tokens": 9000, "total_tokens": 320000,
    "grounding_tool_count": [ {"type": "google_search", "count": 80} ]
  }
}`

func TestInteractionDecodesWireShape(t *testing.T) {
	var in Interaction
	if err := json.Unmarshal([]byte(wireExample), &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if in.ID != "v1_abc" || in.Status != StatusCompleted {
		t.Errorf("envelope: %+v", in)
	}
	if len(in.Steps) != 2 || in.Steps[0].Type != StepUserInput || in.Steps[1].Type != StepModelOutput {
		t.Fatalf("steps: %+v", in.Steps)
	}

	text := in.Steps[1].Content[1]
	if text.Type != ContentText || text.Text != "# Report" {
		t.Errorf("text part: %+v", text)
	}
	if len(text.Annotations) != 1 || text.Annotations[0].Type != AnnotationURLCitation ||
		text.Annotations[0].URL != "https://example.com" || text.Annotations[0].EndIndex != 8 {
		t.Errorf("annotations: %+v", text.Annotations)
	}

	img := in.Steps[1].Content[2]
	if img.Type != ContentImage || string(img.Data) != "hello" || img.MIMEType != "image/png" {
		t.Errorf("image part (base64 must decode): %+v", img)
	}

	if in.Usage.TotalTokens != 320000 || len(in.Usage.GroundingToolCount) != 1 ||
		in.Usage.GroundingToolCount[0].Count != 80 {
		t.Errorf("usage: %+v", in.Usage)
	}
}

func TestFinalOutput(t *testing.T) {
	var in Interaction
	if err := json.Unmarshal([]byte(wireExample), &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	step, ok := in.FinalOutput()
	if !ok || step.Type != StepModelOutput || len(step.Content) != 3 {
		t.Errorf("FinalOutput: ok=%v step=%+v", ok, step)
	}

	empty := Interaction{Steps: []Step{{Type: StepUserInput}}}
	if _, ok := empty.FinalOutput(); ok {
		t.Error("FinalOutput on input-only interaction must report false")
	}
}

func TestStatusTerminal(t *testing.T) {
	terminal := []Status{
		StatusRequiresAction, StatusCompleted, StatusFailed,
		StatusCancelled, StatusIncomplete, StatusBudgetExceeded,
	}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%s must be terminal", s)
		}
	}
	if StatusInProgress.Terminal() {
		t.Error("in_progress must not be terminal")
	}
	if Status("").Terminal() {
		t.Error("unknown (empty) status must not be terminal")
	}
}
