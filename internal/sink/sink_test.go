package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rxbynerd/chiron/internal/types"
)

func sampleResult() *types.RunResult {
	return &types.RunResult{
		InteractionID: "v1_abc",
		Status:        types.StatusCompleted,
		Report: &types.Report{
			Markdown: []byte("---\nquery: q\n---\n\n# Report\n\n![Chart 1](chart-1.png)\n"),
			Assets: []types.Asset{
				{Name: "chart-1.png", MIMEType: "image/png", Data: []byte("png-bytes")},
				{Name: "chart-2.svg", MIMEType: "image/svg+xml", Data: []byte("svg-bytes")},
			},
		},
		Usage: types.Usage{SearchCount: 80, EstimatedCostGBP: 1.58},
	}
}

func TestStdoutMarkdownWritesDocument(t *testing.T) {
	var buf bytes.Buffer
	res := sampleResult()
	if err := NewStdoutMarkdown(&buf).Write(context.Background(), res); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), res.Report.Markdown) {
		t.Errorf("wrote %q, want the report markdown verbatim", buf.String())
	}
}

func TestStdoutMarkdownNilReport(t *testing.T) {
	err := NewStdoutMarkdown(&bytes.Buffer{}).Write(context.Background(), &types.RunResult{})
	if err == nil {
		t.Error("Write with nil report must error, not write nothing silently")
	}
}

func TestFileWritesReportAndAssets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.md")
	res := sampleResult()
	if err := NewFile(path).Write(context.Background(), res); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading report: %v", err)
	}
	if !bytes.Equal(got, res.Report.Markdown) {
		t.Errorf("report = %q, want the markdown verbatim", got)
	}
	for _, a := range res.Report.Assets {
		data, err := os.ReadFile(filepath.Join(dir, a.Name))
		if err != nil {
			t.Fatalf("asset %s not written next to the report: %v", a.Name, err)
		}
		if !bytes.Equal(data, a.Data) {
			t.Errorf("asset %s = %q, want %q", a.Name, data, a.Data)
		}
	}

	// C1-SEC-4: reports carry query text, citations and cost signals —
	// owner-only, like the JSONL trace.
	names := []string{path}
	for _, a := range res.Report.Assets {
		names = append(names, filepath.Join(dir, a.Name))
	}
	for _, name := range names {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %v, want 0600", name, perm)
		}
	}
}

func TestFileRejectsPathShapedAssetNames(t *testing.T) {
	dir := t.TempDir()
	res := sampleResult()
	res.Report.Assets = []types.Asset{{Name: "../escape.png", Data: []byte("x")}}
	err := NewFile(filepath.Join(dir, "report.md")).Write(context.Background(), res)
	if err == nil {
		t.Fatal("a path-shaped asset name must be rejected, not resolved outside the report directory")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "..", "escape.png")); statErr == nil {
		t.Error("asset escaped the report directory")
	}
}

func TestFileMissingDirectoryFails(t *testing.T) {
	res := sampleResult()
	res.Report.Assets = nil
	err := NewFile(filepath.Join(t.TempDir(), "absent", "report.md")).Write(context.Background(), res)
	if err == nil {
		t.Error("writing into a missing directory must surface the error")
	}
}

func TestStdoutJSONRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	res := sampleResult()
	if err := NewStdoutJSON(&buf).Write(context.Background(), res); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var got types.RunResult
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not one JSON document: %v", err)
	}
	if got.InteractionID != res.InteractionID || got.Status != res.Status {
		t.Errorf("round-trip = %+v, want id/status preserved", got)
	}
	if !bytes.Equal(got.Report.Markdown, res.Report.Markdown) {
		t.Error("report markdown did not survive the JSON round trip")
	}
	if len(got.Report.Assets) != 2 || !bytes.Equal(got.Report.Assets[0].Data, []byte("png-bytes")) {
		t.Error("assets must travel inline in the JSON envelope")
	}
}

func TestMultiWritesAllAndStopsOnError(t *testing.T) {
	var first, second bytes.Buffer
	res := sampleResult()
	multi := Multi(NewStdoutMarkdown(&first), NewStdoutMarkdown(&second))
	if err := multi.Write(context.Background(), res); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if first.Len() == 0 || second.Len() == 0 {
		t.Error("both sinks must receive the report")
	}

	boom := errors.New("boom")
	var after bytes.Buffer
	multi = Multi(failingSink{err: boom}, NewStdoutMarkdown(&after))
	if err := multi.Write(context.Background(), res); !errors.Is(err, boom) {
		t.Errorf("error = %v, want the first sink's failure", err)
	}
	if after.Len() != 0 {
		t.Error("sinks after a failure must not run: the emit already failed")
	}
}

func TestMultiEmptyDiscards(t *testing.T) {
	if err := Multi().Write(context.Background(), sampleResult()); err != nil {
		t.Errorf("empty multi sink must discard, got %v", err)
	}
}

func TestSinksRespectCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := sampleResult()
	sinks := []ReportSink{
		NewStdoutMarkdown(&bytes.Buffer{}),
		NewFile(filepath.Join(t.TempDir(), "report.md")),
		NewStdoutJSON(&bytes.Buffer{}),
	}
	for _, s := range sinks {
		if err := s.Write(ctx, res); !errors.Is(err, context.Canceled) {
			t.Errorf("%T.Write with cancelled context = %v, want context.Canceled", s, err)
		}
	}
}

type failingSink struct{ err error }

func (f failingSink) Write(context.Context, *types.RunResult) error { return f.err }

// Guard against the markdown sink quietly mangling multi-byte content.
func TestStdoutMarkdownPreservesBytes(t *testing.T) {
	var buf bytes.Buffer
	res := sampleResult()
	res.Report.Markdown = []byte("# Œuvre — 研究\n")
	if err := NewStdoutMarkdown(&buf).Write(context.Background(), res); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), res.Report.Markdown) {
		t.Error("multi-byte content must pass through untouched")
	}
}
