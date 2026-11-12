package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/interactions"
)

// newPlanServer fakes the planning flow: create returns an in-progress
// interaction, the GET completes it with plan text. Create bodies are
// captured for shape assertions.
func newPlanServer(t *testing.T, bodies *[]map[string]any) *httptest.Server {
	t.Helper()
	check := assert.New(t)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				check.NoError(err, "decoding create body")
				return
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
	must := require.New(t)
	opts := Options{APIKey: testKey, BaseURL: server.URL}
	fastPoll(&opts)
	for _, m := range mutate {
		m(&opts)
	}
	p, err := NewPlanner(opts)
	must.NoError(err, "NewPlanner")
	return p
}

func TestPlanner_Propose_BuildsPlanningCreate(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var bodies []map[string]any
	server := newPlanServer(t, &bodies)
	defer server.Close()

	plan, err := newPlanner(t, server).Propose(context.Background(), "Why is the sky blue?")
	must.NoError(err)
	check.Equal("v1_plan_1", plan.InteractionID)
	check.Contains(plan.Text, "Search vendor sites", "plan text must be the agent's plan")

	must.Len(bodies, 1)
	body := bodies[0]
	check.Equal("deep-research-preview-04-2026", body["agent"], "planning is a research create")
	cfg, _ := body["agent_config"].(map[string]any)
	check.NotNil(cfg, "agent_config")
	check.Equal(true, cfg["collaborative_planning"])
	check.Equal(true, body["background"], "planning steps must run in background")
	check.Equal(true, body["store"], "planning steps must be stored")
	_, hasPrev := body["previous_interaction_id"]
	check.False(hasPrev, "a proposal chains to nothing")
	// The proposal renders the research template, so the agent plans
	// the report it will actually write.
	input, _ := body["input"].(string)
	check.Contains(input, "Why is the sky blue?", "input must carry the query")
	check.Contains(input, "## Conclusion", "input must be the templated research prompt")
}

func TestPlanner_Refine_ChainsWithFeedbackVerbatim(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var bodies []map[string]any
	server := newPlanServer(t, &bodies)
	defer server.Close()

	plan, err := newPlanner(t, server).Refine(context.Background(), "v1_plan_prev", "skip consumer products")
	must.NoError(err)
	check.NotEqual("v1_plan_prev", plan.InteractionID, "a refinement is a new interaction, not the one it revises")

	body := bodies[0]
	check.Equal("v1_plan_prev", body["previous_interaction_id"])
	check.Equal("skip consumer products", body["input"], "feedback must be sent verbatim")
	cfg, _ := body["agent_config"].(map[string]any)
	check.NotNil(cfg, "agent_config")
	check.Equal(true, cfg["collaborative_planning"], "a refinement is still planning")
}

func TestPlanner_Refine_ValidatesInputs(t *testing.T) {
	server := newPlanServer(t, &[]map[string]any{})
	defer server.Close()
	p := newPlanner(t, server)

	tests := []struct {
		name       string
		previousID string
		feedback   string
	}{
		{name: "without previous id", previousID: "", feedback: "feedback"},
		{name: "without feedback", previousID: "v1_plan_1", feedback: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			_, err := p.Refine(context.Background(), tt.previousID, tt.feedback)
			must.Error(err, "must fail before spending")
		})
	}
}

func TestPlannerCreateIsNeverRetried(t *testing.T) {
	must := require.New(t)

	// A create is paid, so an ambiguous 5xx on a plan round must surface
	// rather than retry.
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := newPlanner(t, server).Propose(context.Background(), "q")
	must.Error(err, "a 502 on the plan create must fail the round")
	must.EqualValues(1, posts.Load())
}

func TestPlannerFailedPlanRoundSurfaces(t *testing.T) {
	must := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			w.Write([]byte(`{"id":"v1_plan_bad","status":"in_progress"}`))
			return
		}
		w.Write([]byte(`{"id":"v1_plan_bad","status":"failed"}`))
	}))
	defer server.Close()

	_, err := newPlanner(t, server).Propose(context.Background(), "q")
	must.ErrorContains(err, "ended failed")
}

// A plan round can complete without a text part. That must surface as an
// error, because an empty plan spends a round for nothing. The paid round's
// id still travels back as the recovery handle.
func TestPlannerRoundCompletesWithoutText(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			w.Write([]byte(`{"id":"v1_plan_empty","status":"in_progress"}`))
			return
		}
		w.Write([]byte(`{"id":"v1_plan_empty","status":"completed","steps":[{"type":"model_output","content":[]}]}`))
	}))
	defer server.Close()

	plan, err := newPlanner(t, server).Propose(context.Background(), "q")
	must.ErrorContains(err, "without plan text")
	must.NotNil(plan, "the paid round must be returned")
	check.Equal("v1_plan_empty", plan.InteractionID, "the paid round's id is the recovery handle")
}

// A requires_action plan outcome must stay matchable with errors.Is through
// the planner's wrap, so concludeRun maps it to the research-outcome exit
// code rather than an infrastructure error.
func TestPlannerRoundRequiresAction(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			w.Write([]byte(`{"id":"v1_plan_ra","status":"in_progress"}`))
			return
		}
		w.Write([]byte(`{"id":"v1_plan_ra","status":"requires_action"}`))
	}))
	defer server.Close()

	plan, err := newPlanner(t, server).Propose(context.Background(), "q")
	must.ErrorIs(err, interactions.ErrRequiresAction)
	must.NotNil(plan, "the paid round must be returned")
	check.Equal("v1_plan_ra", plan.InteractionID, "the paid round's id is the recovery handle")
}
