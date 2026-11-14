package fleet

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/testutil/golden"
)

// TestActionSchemaIsStrictCompliant pins every action schema to the
// provider's strict structured-output rules; a non-compliant schema is a 400
// on every turn against a real endpoint.
func TestActionSchemaIsStrictCompliant(t *testing.T) {
	for _, recall := range []bool{false, true} {
		for _, tt := range []struct {
			name   string
			schema json.RawMessage
		}{
			{"every action", actionSchemaFor(recall)},
			{"final only", finalOnlySchemaFor(recall)},
		} {
			t.Run(fmt.Sprintf("%s/recall=%v", tt.name, recall), func(t *testing.T) {
				must := require.New(t)
				must.NoError(model.ValidateStrictSchema(tt.schema), "schema violates strict mode")
			})
		}
	}
}

// TestFinalOnlySchema_DiffersOnlyInFinalFields: the final-only schema for
// each variant is its ordinary schema with the action enum narrowed to final
// and answer and citations no longer nullable nor described as null, and
// nothing else changed.
func TestFinalOnlySchema_DiffersOnlyInFinalFields(t *testing.T) {
	for _, tt := range []struct {
		recall bool
		golden string
	}{
		{false, "worker-action-schema-final.json"},
		{true, "worker-action-schema-recall-final.json"},
	} {
		t.Run(fmt.Sprintf("recall=%v", tt.recall), func(t *testing.T) {
			must := require.New(t)
			golden.Assert(t, tt.golden, finalOnlySchemaFor(tt.recall))

			var want, got map[string]any
			must.NoError(json.Unmarshal(actionSchemaFor(tt.recall), &want), "decode ordinary schema")
			must.NoError(json.Unmarshal(finalOnlySchemaFor(tt.recall), &got), "decode final-only schema")
			props := want["properties"].(map[string]any)
			props["action"].(map[string]any)["enum"] = []any{"final"}
			props["answer"].(map[string]any)["type"] = "string"
			props["answer"].(map[string]any)["description"] = "For action=final: the finding, in the required output format."
			props["citations"].(map[string]any)["type"] = "array"
			props["citations"].(map[string]any)["description"] = "For action=final: the sources relied on."
			must.Equal(want, got)
		})
	}
}
