package httpx_test

import (
	"bytes"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/httpx"
)

func TestReadAllBounded_ReturnsWholeBodyOrBoundError(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		limit   int64
		wantErr string // "" means the whole body is returned
	}{
		{"empty body", "", 16, ""},
		{"under the bound", "hello", 16, ""},
		{"exactly the bound", strings.Repeat("x", 16), 16, ""},
		{"one byte over the bound", strings.Repeat("x", 17), 16, "body exceeds 16-byte bound"},
		{"far over the bound", strings.Repeat("x", 4096), 16, "body exceeds 16-byte bound"},
		{"zero bound with an empty body", "", 0, ""},
		{"zero bound with one byte", "x", 0, "body exceeds 0-byte bound"},
		{"largest bound", "hello", math.MaxInt64, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			data, err := httpx.ReadAllBounded(strings.NewReader(tt.body), tt.limit)
			if tt.wantErr == "" {
				must.NoError(err)
				must.Equal(tt.body, string(data))
				return
			}
			must.ErrorIs(err, httpx.ErrBodyTooLarge)
			must.EqualError(err, tt.wantErr)
			must.Nil(data)
		})
	}
}

func TestReadAllBounded_ConsumesAtMostOneByteOverBound(t *testing.T) {
	must := require.New(t)
	const bound = 64
	const total = 1 << 20
	src := bytes.NewReader(bytes.Repeat([]byte("x"), total))
	_, err := httpx.ReadAllBounded(src, bound)
	must.ErrorIs(err, httpx.ErrBodyTooLarge)
	consumed := int64(total) - int64(src.Len())
	must.Equal(int64(bound+1), consumed)
}

func TestReadAllBounded_PropagatesReadError(t *testing.T) {
	must := require.New(t)
	boom := errors.New("connection reset")
	r := io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(boom))
	data, err := httpx.ReadAllBounded(r, 1024)
	must.ErrorIs(err, boom)
	must.NotErrorIs(err, httpx.ErrBodyTooLarge)
	must.Nil(data)
}
