package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newPlanServer fakes the planning flow: create returns an in-progress
// interaction, the GET completes it with plan text. Create bodies are
// captured for shape assertions.
func newPlanServer(t *testing.T, bodies *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decoding create body: %v", err)
			}
			*bodies = append(*bodies, body)
			w.Write([]byte(`{"id":"v1_plan_` + string(rune('0'+len(*bodies))) + `","status":"in_progress"}`))
			return
		}
		id := strings.TrimPrefix(req.URL.Path, "/v1beta/interactions/")
		w.Write([]byte(`{
			"id": "` + id + `",
			"status": "completed",
			"steps": [{"type": "model_output", "content": [
				{"type": "text", "text": "1. Search vendor sites.\n2. Compare datasheets."}
			]}]
		}`))
	}))
}

func newPlanner(t *testing.T, server *httptest.Server, mutate ...func(*Options)) *Planner {
	t.Helper()
	opts := Options{APIKey: testKey, BaseURL: server.URL}
	fastPoll(&opts)
	for _, m := range mutate {
		m(&opts)
	}
	p, err := NewPlanner(opts)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	return p
}

func TestPlannerProposeBuildsPlanningCreate(t *testing.T) {
	var bodies []map[string]any
	server := newPlanServer(t, &bodies)
	defer server.Close()

	plan, err := newPlanner(t, server).Propose(context.Background(), "Why is the sky blue?")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if plan.InteractionID != "v1_plan_1" {
		t.Errorf("plan id = %q", plan.InteractionID)
	}
	if !strings.Contains(plan.Text, "Search vendor sites") {
		t.Errorf("plan text = %q, want the agent's plan", plan.Text)
	}

	if len(bodies) != 1 {
		t.Fatalf("creates = %d, want 1", len(bodies))
	}
	body := bodies[0]
	if body["agent"] != "deep-research-preview-04-2026" {
		t.Errorf("agent = %v, want the research agent — planning is a research create", body["agent"])
	}
	cfg, _ := body["agent_config"].(map[string]any)
	if cfg == nil || cfg["collaborative_planning"] != true {
		t.Errorf("agent_config = %v, want collaborative_planning true", cfg)
	}
	if body["background"] != true || body["store"] != true {
		t.Errorf("background/store = %v/%v, want true/true (§7: all steps in background)", body["background"], body["store"])
	}
	if _, hasPrev := body["previous_interaction_id"]; hasPrev {
		t.Error("a proposal chains to nothing")
	}
	// The proposal renders the research template, so the agent plans
	// the report it will actually write.
	input, _ := body["input"].(string)
	if !strings.Contains(input, "Why is the sky blue?") || !strings.Contains(input, "## Conclusion") {
		t.Errorf("input must be the templated research prompt: %q", input)
	}
}

func TestPlannerRefineChainsWithFeedbackVerbatim(t *testing.T) {
	var bodies []map[string]any
	server := newPlanServer(t, &bodies)
	defer server.Close()

	plan, err := newPlanner(t, server).Refine(context.Background(), "v1_plan_prev", "skip consumer products")
	if err != nil {
		t.Fatalf("Refine: %v", err)
	}
	if plan.InteractionID == "v1_plan_prev" {
		t.Error("a refinement is a new interaction, not the one it revises")
	}

	body := bodies[0]
	if body["previous_interaction_id"] != "v1_plan_prev" {
		t.Errorf("previous_interaction_id = %v", body["previous_interaction_id"])
	}
	if body["input"] != "skip consumer products" {
		t.Errorf("input = %v, want the feedback verbatim", body["input"])
	}
	cfg, _ := body["agent_config"].(map[string]any)
	if cfg == nil || cfg["collaborative_planning"] != true {
		t.Errorf("agent_config = %v, refinement is still planning", cfg)
	}
}

func TestPlannerRefineValidatesInputs(t *testing.T) {
	server := newPlanServer(t, &[]map[string]any{})
	defer server.Close()
	p := newPlanner(t, server)

	if _, err := p.Refine(context.Background(), "", "feedback"); err == nil {
		t.Error("refine without a previous id must fail before spending")
	}
	if _, err := p.Refine(context.Background(), "v1_plan_1", ""); err == nil {
		t.Error("refine without feedback must fail before spending")
	}
}

func TestPlannerCreateIsNeverRetried(t *testing.T) {
	// Same duplicate-spend rationale as the research create: an
	// ambiguous 5xx on a plan round must surface, not retry.
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	if _, err := newPlanner(t, server).Propose(context.Background(), "q"); err == nil {
		t.Fatal("a 502 on the plan create must fail the round")
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("create attempts = %d, want exactly 1", got)
	}
}

func TestPlannerFailedPlanRoundSurfaces(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			w.Write([]byte(`{"id":"v1_plan_bad","status":"in_progress"}`))
			return
		}
		w.Write([]byte(`{"id":"v1_plan_bad","status":"failed"}`))
	}))
	defer server.Close()

	_, err := newPlanner(t, server).Propose(context.Background(), "q")
	if err == nil || !strings.Contains(err.Error(), "ended failed") {
		t.Errorf("err = %v, want the failed plan round surfaced", err)
	}
}
