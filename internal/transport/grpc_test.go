package transport

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGRPC_Emit_ReturnsErrNotImplemented(t *testing.T) {
	must := require.New(t)
	tr := NewGRPC("control-plane:443")
	err := tr.Emit(context.Background(), Event{Kind: KindRunStarted})
	must.ErrorIs(err, ErrNotImplemented)
}

func TestGRPC_Close_IsSafe(t *testing.T) {
	must := require.New(t)
	tr := NewGRPC("control-plane:443")
	must.NoError(tr.Close())
}
