package httpx_test

import (
	"bytes"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/rxbynerd/chiron/internal/httpx"
)

func TestReadAllBounded(t *testing.T) {
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
			data, err := httpx.ReadAllBounded(strings.NewReader(tt.body), tt.limit)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ReadAllBounded = %v, want success", err)
				}
				if string(data) != tt.body {
					t.Errorf("ReadAllBounded = %q, want %q", data, tt.body)
				}
				return
			}
			if !errors.Is(err, httpx.ErrBodyTooLarge) {
				t.Fatalf("ReadAllBounded = %v, want ErrBodyTooLarge", err)
			}
			if err.Error() != tt.wantErr {
				t.Errorf("ReadAllBounded error = %q, want %q", err, tt.wantErr)
			}
			if data != nil {
				t.Errorf("ReadAllBounded returned %d bytes alongside its error", len(data))
			}
		})
	}
}

func TestReadAllBoundedConsumesAtMostOneByteOverTheBound(t *testing.T) {
	src := bytes.NewReader(bytes.Repeat([]byte("x"), 1<<20))
	if _, err := httpx.ReadAllBounded(src, 64); !errors.Is(err, httpx.ErrBodyTooLarge) {
		t.Fatalf("ReadAllBounded = %v, want ErrBodyTooLarge", err)
	}
	if consumed := int64(1<<20) - int64(src.Len()); consumed != 65 {
		t.Errorf("consumed %d bytes, want 65", consumed)
	}
}

func TestReadAllBoundedPropagatesReadError(t *testing.T) {
	boom := errors.New("connection reset")
	r := io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(boom))
	data, err := httpx.ReadAllBounded(r, 1024)
	if !errors.Is(err, boom) {
		t.Fatalf("ReadAllBounded = %v, want the reader's error", err)
	}
	if errors.Is(err, httpx.ErrBodyTooLarge) {
		t.Errorf("a read error must not match ErrBodyTooLarge: %v", err)
	}
	if data != nil {
		t.Errorf("ReadAllBounded returned %d bytes alongside its error", len(data))
	}
}
