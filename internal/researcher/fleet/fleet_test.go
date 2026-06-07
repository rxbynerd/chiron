package fleet

import (
	"context"
	"errors"
	"testing"

	"github.com/rxbynerd/chiron/internal/researcher"
)

func TestFleetReturnsErrNotImplemented(t *testing.T) {
	ctx := context.Background()
	f := New()

	if _, err := f.Start(ctx, researcher.Task{Query: "q"}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Start error = %v, want ErrNotImplemented", err)
	}
	if err := f.Await(ctx, "int_1"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Await error = %v, want ErrNotImplemented", err)
	}
	if _, err := f.Result(ctx, "int_1"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Result error = %v, want ErrNotImplemented", err)
	}
}
