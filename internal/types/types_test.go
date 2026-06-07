package types

import (
	"encoding/json"
	"testing"
)

func TestStatusTerminal(t *testing.T) {
	terminal := []Status{
		StatusCompleted, StatusFailed, StatusCancelled,
		StatusIncomplete, StatusBudgetExceeded,
	}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%s must be terminal", s)
		}
	}

	nonTerminal := []Status{StatusInProgress, StatusRequiresAction, Status("")}
	for _, s := range nonTerminal {
		if s.Terminal() {
			t.Errorf("%q must not be terminal", s)
		}
	}
}

func TestInteractionJSONRoundTrip(t *testing.T) {
	in := Interaction{
		ID:           "v1_abc",
		Agent:        "deep-research",
		Query:        "the query",
		Status:       StatusFailed,
		StatusDetail: "budget exhausted upstream",
		Outputs: []Output{
			{Type: OutputThoughtSummary, Text: "thinking..."},
			{Type: OutputText, Text: "# Report"},
			{Type: OutputImage, MIMEType: "image/png", Data: []byte("hello")},
		},
		Citations: []Citation{{URI: "https://example.com", Title: "Example"}},
		Usage: Usage{
			InputTokens:   250000,
			CachedTokens:  150000,
			OutputTokens:  60000,
			ToolUseTokens: 1000,
			ThoughtTokens: 9000,
			SearchCount:   80,
		},
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Interaction
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if out.StatusDetail != in.StatusDetail {
		t.Errorf("status_detail lost: %+v", out)
	}
	if out.Usage != in.Usage {
		t.Errorf("usage lost: %+v", out.Usage)
	}
	if len(out.Outputs) != 3 || string(out.Outputs[2].Data) != "hello" {
		t.Errorf("outputs lost: %+v", out.Outputs)
	}
}
