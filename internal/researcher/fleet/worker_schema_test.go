package fleet

import (
	"testing"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
)

// TestActionSchemaIsStrictCompliant pins the action schema to the provider's
// strict structured-output rules; a non-compliant schema is a 400 on every
// turn against a real endpoint.
func TestActionSchemaIsStrictCompliant(t *testing.T) {
	if err := model.ValidateStrictSchema(actionSchema); err != nil {
		t.Fatalf("actionSchema violates strict mode: %v", err)
	}
}
