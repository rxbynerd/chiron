package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestStdoutMarkdown_Write_WritesReportVerbatim(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	res := sampleResult()
	must.NoError(NewStdoutMarkdown(&buf).Write(context.Background(), res))
	must.Equal(string(res.Report.Markdown), buf.String())
}

func TestStdoutMarkdown_Write_NilReportErrors(t *testing.T) {
	must := require.New(t)
	err := NewStdoutMarkdown(&bytes.Buffer{}).Write(context.Background(), &types.RunResult{})
	must.Error(err, "Write with nil report must error, not write nothing silently")
}

func TestFile_Write_WritesReportAndAssets(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "report.md")
	res := sampleResult()
	must.NoError(NewFile(path).Write(context.Background(), res))

	got, err := os.ReadFile(path)
	must.NoError(err, "reading report")
	must.Equal(string(res.Report.Markdown), string(got))
	for _, a := range res.Report.Assets {
		data, err := os.ReadFile(filepath.Join(dir, a.Name))
		must.NoError(err, "asset %s not written next to the report", a.Name)
		must.Equal(string(a.Data), string(data), "asset %s", a.Name)
	}

	// Reports carry query text, citations and cost signals, so the files
	// are owner-only, like the JSONL trace.
	names := []string{path}
	for _, a := range res.Report.Assets {
		names = append(names, filepath.Join(dir, a.Name))
	}
	for _, name := range names {
		info, err := os.Stat(name)
		must.NoError(err, "stat %s", name)
		must.Equal(os.FileMode(0o600), info.Mode().Perm(), "mode of %s", name)
	}
}

func TestFile_Write_RejectsPathShapedAssetNames(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	res := sampleResult()
	res.Report.Assets = []types.Asset{{Name: "../escape.png", Data: []byte("x")}}
	err := NewFile(filepath.Join(dir, "report.md")).Write(context.Background(), res)
	must.Error(err, "a path-shaped asset name must be rejected, not resolved outside the report directory")

	_, statErr := os.Stat(filepath.Join(dir, "..", "escape.png"))
	must.Error(statErr, "asset escaped the report directory")
}

func TestFile_Write_MissingDirectoryFails(t *testing.T) {
	must := require.New(t)
	res := sampleResult()
	res.Report.Assets = nil
	err := NewFile(filepath.Join(t.TempDir(), "absent", "report.md")).Write(context.Background(), res)
	must.Error(err, "writing into a missing directory must surface the error")
}

func TestStdoutJSON_Write_RoundTrips(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	res := sampleResult()
	must.NoError(NewStdoutJSON(&buf).Write(context.Background(), res))

	var got types.RunResult
	must.NoError(json.Unmarshal(buf.Bytes(), &got), "output is not one JSON document")
	check.Equal(res.InteractionID, got.InteractionID, "interaction id after round trip")
	check.Equal(res.Status, got.Status, "status after round trip")
	check.Equal(string(res.Report.Markdown), string(got.Report.Markdown), "report markdown did not survive the JSON round trip")
	must.Len(got.Report.Assets, 2, "assets must travel inline in the JSON envelope")
	check.Equal("png-bytes", string(got.Report.Assets[0].Data), "first asset data after round trip")
}

func TestMulti_Write_WritesAllAndStopsOnError(t *testing.T) {
	t.Run("writes every sink", func(t *testing.T) {
		must := require.New(t)
		var first, second bytes.Buffer
		res := sampleResult()
		multi := Multi(NewStdoutMarkdown(&first), NewStdoutMarkdown(&second))
		must.NoError(multi.Write(context.Background(), res))
		must.NotEmpty(first.String(), "first sink must receive the report")
		must.NotEmpty(second.String(), "second sink must receive the report")
	})

	t.Run("stops after a failing sink", func(t *testing.T) {
		must := require.New(t)
		res := sampleResult()
		boom := errors.New("boom")
		var after bytes.Buffer
		multi := Multi(failingSink{err: boom}, NewStdoutMarkdown(&after))
		must.ErrorIs(multi.Write(context.Background(), res), boom, "error must be the first sink's failure")
		must.Empty(after.String(), "sinks after a failure must not run: the emit already failed")
	})
}

func TestMulti_Write_EmptyDiscards(t *testing.T) {
	must := require.New(t)
	must.NoError(Multi().Write(context.Background(), sampleResult()), "empty multi sink must discard")
}

func TestSinks_Write_RespectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := sampleResult()
	tests := []struct {
		name string
		sink ReportSink
	}{
		{name: "stdout markdown", sink: NewStdoutMarkdown(&bytes.Buffer{})},
		{name: "file", sink: NewFile(filepath.Join(t.TempDir(), "report.md"))},
		{name: "stdout json", sink: NewStdoutJSON(&bytes.Buffer{})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.ErrorIs(tt.sink.Write(ctx, res), context.Canceled)
		})
	}
}

type failingSink struct{ err error }

func (f failingSink) Write(context.Context, *types.RunResult) error { return f.err }

// Guard against the markdown sink quietly mangling multi-byte content.
func TestStdoutMarkdown_Write_PreservesMultiByteContent(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	res := sampleResult()
	res.Report.Markdown = []byte("# Œuvre — 研究\n")
	must.NoError(NewStdoutMarkdown(&buf).Write(context.Background(), res))
	must.Equal(string(res.Report.Markdown), buf.String(), "multi-byte content must pass through untouched")
}
