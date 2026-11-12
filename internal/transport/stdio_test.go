package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStdio_Emit_WritesNDJSONLines(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	var buf bytes.Buffer
	tr := NewStdio(&buf)
	ctx := context.Background()

	events := []Event{
		{Kind: KindRunStarted, Payload: json.RawMessage(`{"query":"q"}`)},
		{Kind: KindInteractionCreated, Payload: json.RawMessage(`{"interaction_id":"int_1"}`)},
		{Kind: KindStatusChanged, Payload: json.RawMessage(`{"status":"in_progress"}`)},
		{Kind: KindRunCompleted, Payload: json.RawMessage(`{"status":"completed"}`)},
		{Kind: KindCostSummary, Payload: json.RawMessage(`{"total_tokens":42}`)},
	}
	for _, ev := range events {
		must.NoError(tr.Emit(ctx, ev), "Emit(%q)", ev.Kind)
	}
	must.NoError(tr.Close())

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	must.Len(lines, len(events))
	for i, line := range lines {
		var ev Event
		must.NoError(json.Unmarshal([]byte(line), &ev), "line %d is not valid JSON: %s", i, line)
		check.Equal(events[i].Kind, ev.Kind, "line %d kind", i)
		check.False(ev.Time.IsZero(), "line %d has a zero time; Emit should stamp it", i)
	}
}

func TestStdio_Emit_PreservesExplicitTime(t *testing.T) {
	must := require.New(t)

	var buf bytes.Buffer
	tr := NewStdio(&buf)
	want := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)

	must.NoError(tr.Emit(context.Background(), Event{Time: want, Kind: KindDelta}))

	var ev Event
	must.NoError(json.Unmarshal(buf.Bytes(), &ev))
	must.True(ev.Time.Equal(want), "time = %v, want %v", ev.Time, want)
}

func TestStdio_Emit_RespectsCancelledContext(t *testing.T) {
	must := require.New(t)

	var buf bytes.Buffer
	tr := NewStdio(&buf)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	must.Error(tr.Emit(ctx, Event{Kind: KindRunStarted}))
	must.Zero(buf.Len(), "Emit with cancelled context should write nothing")
}

func TestStdio_Emit_ConcurrentWritesDoNotInterleave(t *testing.T) {
	must := require.New(t)

	var buf bytes.Buffer
	tr := NewStdio(&buf)
	ctx := context.Background()

	const n = 50
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			_ = tr.Emit(ctx, Event{Kind: KindDelta, Payload: json.RawMessage(`{"text":"chunk"}`)})
		})
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	must.Len(lines, n)
	for i, line := range lines {
		var ev Event
		must.NoError(json.Unmarshal([]byte(line), &ev), "line %d corrupted by interleaving: %s", i, line)
	}
}

func TestStdio_New_DefaultsNilWriterToStderr(t *testing.T) {
	must := require.New(t)

	// NewStdio(nil) must not panic and must produce a usable transport;
	// the write goes to the test process's stderr, which is harmless.
	tr := NewStdio(nil)
	must.NotNil(tr.w, "NewStdio(nil) should default the writer")
}
