package sink

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/testutil/golden"
	"github.com/rxbynerd/chiron/internal/types"
)

// The bytes --output json writes are an external contract: every key, its
// type and which keys are omitted when empty. Each case pins one facet, and
// every optional key is set in one case and empty in another, so a renamed tag
// or a changed omission rule shows up as a diff in testdata.
func TestStdoutJSON_Write_MatchesGolden(t *testing.T) {
	for _, tt := range []struct {
		name   string
		golden string
		result *types.RunResult
	}{
		{
			// Every field set, so each key is present.
			name:   "every field set",
			golden: "run-result.json",
			result: &types.RunResult{
				InteractionID: "v1_golden",
				Status:        types.StatusCompleted,
				Report: &types.Report{
					Markdown: []byte("# Report\n\n![Chart 1](chart-1.png)\n"),
					Assets: []types.Asset{
						{Name: "chart-1.png", MIMEType: "image/png", Data: []byte("png-bytes")},
					},
				},
				Usage: types.Usage{
					InputTokens:         1000,
					CachedTokens:        200,
					OutputTokens:        3000,
					ToolUseTokens:       400,
					ThoughtTokens:       500,
					SearchCount:         80,
					RecallCount:         3,
					PollCount:           12,
					ReconnectCount:      1,
					EstimatedCostGBP:    1.58,
					WebCostUSD:          0.043,
					WebCostUnknownCalls: 2,
				},
				Duration: time.Minute + 34*time.Second + 500*time.Millisecond,
			},
		},
		{
			// A failed run carries a placeholder report and no usage: the
			// usage object and the report's assets are omitted.
			name:   "failed run with zero usage",
			golden: "run-result-failed.json",
			result: &types.RunResult{
				InteractionID: "v1_failed",
				Status:        types.StatusFailed,
				Report:        &types.Report{Markdown: []byte("# Failed\n")},
				Duration:      2 * time.Second,
			},
		},
		{
			// Without a report, usage or duration only the id and status remain.
			name:   "no report",
			golden: "run-result-no-report.json",
			result: &types.RunResult{InteractionID: "v1_none", Status: types.StatusCancelled},
		},
		{
			// Inside a present object, empty fields are omitted individually:
			// the asset's MIME type and every usage key except the token
			// counters.
			name:   "token counters only",
			golden: "run-result-sparse.json",
			result: &types.RunResult{
				InteractionID: "v1_sparse",
				Status:        types.StatusCompleted,
				Report: &types.Report{
					Markdown: []byte("# Sparse\n"),
					Assets:   []types.Asset{{Name: "chart.bin", Data: []byte("x")}},
				},
				Usage: types.Usage{
					InputTokens:   10,
					CachedTokens:  2,
					OutputTokens:  30,
					ToolUseTokens: 4,
					ThoughtTokens: 5,
				},
			},
		},
		{
			// The complement of the case above: a present usage object holding
			// only the activity counters and the cost keys.
			name:   "activity counters and costs only",
			golden: "run-result-counters.json",
			result: &types.RunResult{
				InteractionID: "v1_counters",
				Status:        types.StatusIncomplete,
				Usage: types.Usage{
					SearchCount:         7,
					RecallCount:         1,
					PollCount:           2,
					ReconnectCount:      3,
					EstimatedCostGBP:    0.25,
					WebCostUSD:          0.002,
					WebCostUnknownCalls: 1,
				},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			var buf bytes.Buffer

			must.NoError(NewStdoutJSON(&buf).Write(context.Background(), tt.result))

			golden.Assert(t, tt.golden, buf.Bytes())
		})
	}
}
