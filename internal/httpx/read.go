package httpx

import (
	"errors"
	"fmt"
	"io"
	"math"
)

// ErrBodyTooLarge matches, via errors.Is, the error ReadAllBounded returns
// when the body exceeds its bound.
var ErrBodyTooLarge = errors.New("body exceeds bound")

type bodyTooLargeError struct{ limit int64 }

func (e bodyTooLargeError) Error() string {
	return fmt.Sprintf("body exceeds %d-byte bound", e.limit)
}

func (bodyTooLargeError) Is(target error) bool { return target == ErrBodyTooLarge }

// ReadAllBounded reads r to EOF, consuming at most limit+1 bytes. A body
// larger than limit fails with an error matching ErrBodyTooLarge rather than
// being truncated, so a misbehaving server can neither exhaust memory nor
// pass a clipped document off as complete. A read error is returned as is.
func ReadAllBounded(r io.Reader, limit int64) ([]byte, error) {
	n := limit
	if n < math.MaxInt64 {
		n++
	}
	data, err := io.ReadAll(io.LimitReader(r, n))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, bodyTooLargeError{limit}
	}
	return data, nil
}
