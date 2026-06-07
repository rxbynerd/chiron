package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStdioEmitsNDJSONLines(t *testing.T) {
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
		if err := tr.Emit(ctx, ev); err != nil {
			t.Fatalf("Emit(%q): %v", ev.Kind, err)
		}
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if got, want := len(lines), len(events); got != want {
		t.Fatalf("got %d lines, want %d", got, want)
	}
	for i, line := range lines {
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\n%s", i, err, line)
		}
		if ev.Kind != events[i].Kind {
			t.Errorf("line %d kind = %q, want %q", i, ev.Kind, events[i].Kind)
		}
		if ev.Time.IsZero() {
			t.Errorf("line %d has a zero time; Emit should stamp it", i)
		}
	}
}

func TestStdioPreservesExplicitTime(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdio(&buf)
	want := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)

	if err := tr.Emit(context.Background(), Event{Time: want, Kind: KindDelta}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	var ev Event
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !ev.Time.Equal(want) {
		t.Errorf("time = %v, want %v", ev.Time, want)
	}
}

func TestStdioRespectsCancelledContext(t *testing.T) {
	var buf bytes.Buffer
	tr := NewStdio(&buf)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := tr.Emit(ctx, Event{Kind: KindRunStarted}); err == nil {
		t.Fatal("Emit with cancelled context: got nil error")
	}
	if buf.Len() != 0 {
		t.Errorf("Emit with cancelled context wrote %d bytes", buf.Len())
	}
}

func TestStdioConcurrentEmitsDoNotInterleave(t *testing.T) {
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
	if got := len(lines); got != n {
		t.Fatalf("got %d lines, want %d", got, n)
	}
	for i, line := range lines {
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d corrupted by interleaving: %v\n%s", i, err, line)
		}
	}
}

func TestStdioDefaultsNilWriterToStderr(t *testing.T) {
	// NewStdio(nil) must not panic and must produce a usable transport;
	// the write goes to the test process's stderr, which is harmless.
	tr := NewStdio(nil)
	if tr.w == nil {
		t.Fatal("NewStdio(nil) left writer nil")
	}
}
