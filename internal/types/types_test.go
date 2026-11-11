package types

import (
	"encoding/json"
	"reflect"
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

// TestUsageAddSumsEveryField sets every field of Usage to one and checks
// that Add doubles each.
func TestUsageAddSumsEveryField(t *testing.T) {
	var u Usage
	v := reflect.ValueOf(&u).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Int:
			f.SetInt(1)
		case reflect.Float64:
			f.SetFloat(1)
		default:
			t.Fatalf("Usage.%s has kind %s; teach Add and this test to sum it", v.Type().Field(i).Name, f.Kind())
		}
	}
	sum := reflect.ValueOf(u.Add(u))
	for i := range sum.NumField() {
		f := sum.Field(i)
		var got float64
		if f.Kind() == reflect.Int {
			got = float64(f.Int())
		} else {
			got = f.Float()
		}
		if got != 2 {
			t.Errorf("Add: Usage.%s = %v, want 2", sum.Type().Field(i).Name, got)
		}
	}
}

// TestUsageAddRoundsWebCost: summed micro-dollar amounts stay exact.
func TestUsageAddRoundsWebCost(t *testing.T) {
	got := Usage{WebCostUSD: 0.007}.Add(Usage{WebCostUSD: 0.014}).Add(Usage{WebCostUSD: 0.001})
	if got.WebCostUSD != 0.022 {
		t.Errorf("WebCostUSD = %v, want 0.022", got.WebCostUSD)
	}
}
