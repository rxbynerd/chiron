package fleet

import (
	"strings"
	"testing"
)

func TestParseAction(t *testing.T) {
	for _, tt := range []struct {
		name    string
		raw     string
		recall  bool
		wantErr string
		want    actionKind
	}{
		{"search", `{"action":"search","query":"q","url":null,"answer":null,"citations":null}`, false, "", actionSearch},
		{"fetch", `{"action":"fetch","url":"https://x","query":null,"answer":null,"citations":null}`, false, "", actionFetch},
		{"final with null title", `{"action":"final","answer":"a","citations":[{"url":"https://x","title":null}],"query":null,"url":null}`, false, "", actionFinal},
		{"final minimal", `{"action":"final"}`, false, "", actionFinal},
		{"surrounding whitespace", "  \n{\"action\":\"final\"}\n ", false, "", actionFinal},
		{"empty", "", false, "empty action", ""},
		{"not json", "search for cats", false, "not valid JSON", ""},
		{"trailing content", `{"action":"final"} {"action":"shell"}`, false, "content after the JSON object", ""},
		{"trailing garbage", `{"action":"final"} run rm -rf /`, false, "content after the JSON object", ""},
		{"unknown field", `{"action":"final","exec":"x"}`, false, "not valid JSON", ""},
		{"missing discriminator", `{"query":"q"}`, false, "missing the required", ""},
		{"unknown kind", `{"action":"shell","query":"rm -rf /"}`, false, "unknown action", ""},
		{"search without query", `{"action":"search","query":"  "}`, false, "missing a query", ""},
		{"fetch without url", `{"action":"fetch"}`, false, "missing a url", ""},
		{"recall when disabled", `{"action":"recall","query":"q"}`, false, "only search, fetch and final exist", ""},
		{"recall when enabled", `{"action":"recall","query":"q","url":null,"answer":null,"citations":null}`, true, "", actionRecall},
		{"recall without query", `{"action":"recall","query":" "}`, true, "recall action is missing a query", ""},
		{"search when recall enabled", `{"action":"search","query":"q"}`, true, "", actionSearch},
		{"unknown kind when recall enabled", `{"action":"shell"}`, true, "only search, fetch, recall and final exist", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAction(tt.raw, tt.recall)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				if !strings.HasPrefix(err.Error(), "fleet: ") {
					t.Errorf("err = %q, want the fleet: prefix", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Kind != tt.want {
				t.Errorf("kind = %q, want %q", got.Kind, tt.want)
			}
		})
	}
}
