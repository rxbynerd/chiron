package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/researcher"
)

func TestFollowUp_Start_BuildsModelCreate(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// The handler runs on the server's goroutine, so it reports through check.
		if !check.NoError(json.NewDecoder(req.Body).Decode(&body), "decoding create body") {
			return
		}
		w.Write([]byte(`{"id":"v1_followup","status":"in_progress"}`))
	}))
	defer server.Close()

	opts := Options{APIKey: testKey, BaseURL: server.URL}
	fastPoll(&opts)
	r, err := NewFollowUp(opts, "")
	must.NoError(err)

	id, err := r.Start(context.Background(), researcher.Task{
		Query:                 "Which vendor leads on power efficiency?",
		PreviousInteractionID: "v1_research",
	})
	must.NoError(err)
	check.Equal("v1_followup", id)

	// Follow-up Q&A uses model, not agent (docs/INTERACTIONS-API.md §3).
	check.Equal(DefaultFollowUpModel, body["model"])
	check.NotContains(body, "agent", "a follow-up create must not carry agent")
	check.NotContains(body, "agent_config", "a follow-up create must not carry agent_config")
	check.NotContains(body, "tools", "a follow-up create must not carry tools")
	check.Equal("v1_research", body["previous_interaction_id"])
	// The query travels verbatim: a follow-up is a question about an
	// existing report, not a templated research task.
	check.Equal("Which vendor leads on power efficiency?", body["input"])
	check.Equal(true, body["background"])
	check.Equal(true, body["store"])
}

func TestFollowUp_Start_ModelOverride(t *testing.T) {
	must := require.New(t)

	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		json.NewDecoder(req.Body).Decode(&body)
		w.Write([]byte(`{"id":"v1_followup","status":"in_progress"}`))
	}))
	defer server.Close()

	opts := Options{APIKey: testKey, BaseURL: server.URL}
	fastPoll(&opts)
	r, err := NewFollowUp(opts, "gemini-3.1-flash-preview")
	must.NoError(err)

	_, err = r.Start(context.Background(), researcher.Task{Query: "q", PreviousInteractionID: "v1_x"})
	must.NoError(err)
	must.Equal("gemini-3.1-flash-preview", body["model"])
}

func TestFollowUp_Start_RequiresPreviousInteraction(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// The handler runs on the server's goroutine, so it reports through check.
		check.Fail("no request must be sent for an invalid follow-up")
	}))
	defer server.Close()

	opts := Options{APIKey: testKey, BaseURL: server.URL}
	r, err := NewFollowUp(opts, "")
	must.NoError(err)

	_, err = r.Start(context.Background(), researcher.Task{Query: "q"})
	must.Error(err, "a follow-up without a previous interaction id must fail before spending")
}

func TestFollowUpEstimatesZero(t *testing.T) {
	// Model-priced Q&A is outside the tier table; inventing a figure
	// would put a fabricated cost in the front matter and the budget
	// gate.
	must := require.New(t)

	r, err := NewFollowUp(Options{APIKey: testKey}, "")
	must.NoError(err)
	must.Zero(r.EstimatedCostGBP(), "estimate in follow-up mode")
}
