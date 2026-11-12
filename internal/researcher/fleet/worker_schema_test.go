package fleet

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
)

// TestActionSchemaIsStrictCompliant pins the action schema to the provider's
// strict structured-output rules; a non-compliant schema is a 400 on every
// turn against a real endpoint.
func TestActionSchemaIsStrictCompliant(t *testing.T) {
	must := require.New(t)
	err := model.ValidateStrictSchema(actionSchema)
	must.NoError(err, "actionSchema violates strict mode")
}
