package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rxbynerd/chiron/internal/researcher"
)

func TestFollowUpBuildsModelCreate(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("decoding create body: %v", err)
		}
		w.Write([]byte(`{"id":"v1_followup","status":"in_progress"}`))
	}))
	defer server.Close()

	opts := Options{APIKey: testKey, BaseURL: server.URL}
	fastPoll(&opts)
	r, err := NewFollowUp(opts, "")
	if err != nil {
		t.Fatalf("NewFollowUp: %v", err)
	}

	id, err := r.Start(context.Background(), researcher.Task{
		Query:                 "Which vendor leads on power efficiency?",
		PreviousInteractionID: "v1_research",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id != "v1_followup" {
		t.Errorf("id = %q", id)
	}

	// Follow-up Q&A uses model, not agent (docs/INTERACTIONS-API.md §3).
	if body["model"] != DefaultFollowUpModel {
		t.Errorf("model = %v, want the documented default %q", body["model"], DefaultFollowUpModel)
	}
	if _, hasAgent := body["agent"]; hasAgent {
		t.Error("a follow-up create must not carry agent")
	}
	if _, hasCfg := body["agent_config"]; hasCfg {
		t.Error("a follow-up create must not carry agent_config")
	}
	if _, hasTools := body["tools"]; hasTools {
		t.Error("a follow-up create must not carry tools")
	}
	if body["previous_interaction_id"] != "v1_research" {
		t.Errorf("previous_interaction_id = %v", body["previous_interaction_id"])
	}
	// The query travels verbatim: a follow-up is a question about an
	// existing report, not a templated research task.
	if body["input"] != "Which vendor leads on power efficiency?" {
		t.Errorf("input = %v, want the query verbatim", body["input"])
	}
	if body["background"] != true || body["store"] != true {
		t.Errorf("background/store = %v/%v, want true/true", body["background"], body["store"])
	}
}

func TestFollowUpModelOverride(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		json.NewDecoder(req.Body).Decode(&body)
		w.Write([]byte(`{"id":"v1_followup","status":"in_progress"}`))
	}))
	defer server.Close()

	opts := Options{APIKey: testKey, BaseURL: server.URL}
	fastPoll(&opts)
	r, err := NewFollowUp(opts, "gemini-3.1-flash-preview")
	if err != nil {
		t.Fatalf("NewFollowUp: %v", err)
	}
	if _, err := r.Start(context.Background(), researcher.Task{Query: "q", PreviousInteractionID: "v1_x"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if body["model"] != "gemini-3.1-flash-preview" {
		t.Errorf("model = %v, want the override", body["model"])
	}
}

func TestFollowUpRequiresPreviousInteraction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t.Error("no request must be sent for an invalid follow-up")
	}))
	defer server.Close()

	opts := Options{APIKey: testKey, BaseURL: server.URL}
	r, err := NewFollowUp(opts, "")
	if err != nil {
		t.Fatalf("NewFollowUp: %v", err)
	}
	if _, err := r.Start(context.Background(), researcher.Task{Query: "q"}); err == nil {
		t.Error("a follow-up without a previous interaction id must fail before spending")
	}
}

func TestFollowUpEstimatesZero(t *testing.T) {
	// Model-priced Q&A is outside the tier table; inventing a figure
	// would put a fabricated cost in the front matter and the budget
	// gate.
	r, err := NewFollowUp(Options{APIKey: testKey}, "")
	if err != nil {
		t.Fatalf("NewFollowUp: %v", err)
	}
	if got := r.EstimatedCostGBP(); got != 0 {
		t.Errorf("estimate = %v, want 0 for follow-up mode", got)
	}
}
