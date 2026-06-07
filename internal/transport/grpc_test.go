package transport

import (
	"context"
	"errors"
	"testing"
)

func TestGRPCEmitReturnsErrNotImplemented(t *testing.T) {
	tr := NewGRPC("control-plane:443")
	err := tr.Emit(context.Background(), Event{Kind: KindRunStarted})
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Emit error = %v, want ErrNotImplemented", err)
	}
}

func TestGRPCCloseIsSafe(t *testing.T) {
	tr := NewGRPC("control-plane:443")
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
