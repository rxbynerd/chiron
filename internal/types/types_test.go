package types

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatus_Terminal_ClassifiesEveryStatus(t *testing.T) {
	tests := []struct {
		name     string
		status   Status
		terminal bool
	}{
		{"completed", StatusCompleted, true},
		{"failed", StatusFailed, true},
		{"cancelled", StatusCancelled, true},
		{"incomplete", StatusIncomplete, true},
		{"budget exceeded", StatusBudgetExceeded, true},
		{"in progress", StatusInProgress, false},
		{"requires action", StatusRequiresAction, false},
		{"empty", Status(""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.terminal, tt.status.Terminal())
		})
	}
}

func TestInteraction_JSON_RoundTripKeepsFields(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	in := Interaction{
		ID:           "v1_abc",
		Agent:        "deep-research",
		Query:        "the query",
		Status:       StatusFailed,
		StatusDetail: "budget exhausted upstream",
		Outputs: []Output{
			{Type: OutputThoughtSummary, Text: "thinking..."},
			{Type: OutputText, Text: "# Report"},
			{Type: OutputImage, MIMEType: "image/png", Data: []byte("hello")},
		},
		Citations: []Citation{{URI: "https://example.com", Title: "Example"}},
		Usage: Usage{
			InputTokens:   250000,
			CachedTokens:  150000,
			OutputTokens:  60000,
			ToolUseTokens: 1000,
			ThoughtTokens: 9000,
			SearchCount:   80,
		},
	}

	b, err := json.Marshal(in)
	must.NoError(err, "marshal")
	var out Interaction
	err = json.Unmarshal(b, &out)
	must.NoError(err, "unmarshal")

	check.Equal(in.StatusDetail, out.StatusDetail)
	check.Equal(in.Usage, out.Usage)
	must.Len(out.Outputs, 3)
	check.Equal("hello", string(out.Outputs[2].Data))
}

// TestUsage_Add_SumsEveryField sets every field of Usage to one and checks
// that Add doubles each.
func TestUsage_Add_SumsEveryField(t *testing.T) {
	must := require.New(t)

	var u Usage
	v := reflect.ValueOf(&u).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Int:
			f.SetInt(1)
		case reflect.Float64:
			f.SetFloat(1)
		default:
			must.FailNowf("unsupported Usage field kind",
				"Usage.%s has kind %s; teach Add and this test to sum it", v.Type().Field(i).Name, f.Kind())
		}
	}

	sum := reflect.ValueOf(u.Add(u))
	for i := range sum.NumField() {
		t.Run(sum.Type().Field(i).Name, func(t *testing.T) {
			must := require.New(t)
			f := sum.Field(i)
			var got float64
			if f.Kind() == reflect.Int {
				got = float64(f.Int())
			} else {
				got = f.Float()
			}
			must.InDelta(2, got, 0)
		})
	}
}

// TestUsage_Add_RoundsWebCost: summed micro-dollar amounts stay exact.
func TestUsage_Add_RoundsWebCost(t *testing.T) {
	must := require.New(t)

	got := Usage{WebCostUSD: 0.007}.Add(Usage{WebCostUSD: 0.014}).Add(Usage{WebCostUSD: 0.001})
	// A zero delta demands the exactly rounded value.
	must.InDelta(0.022, got.WebCostUSD, 0)
}
