package fleet

import (
	"strings"
	"testing"
)

func TestParseAction(t *testing.T) {
	for _, tt := range []struct {
		name    string
		raw     string
		wantErr string
		want    actionKind
	}{
		{"search", `{"action":"search","query":"q","url":null,"answer":null,"citations":null}`, "", actionSearch},
		{"fetch", `{"action":"fetch","url":"https://x","query":null,"answer":null,"citations":null}`, "", actionFetch},
		{"final with null title", `{"action":"final","answer":"a","citations":[{"url":"https://x","title":null}],"query":null,"url":null}`, "", actionFinal},
		{"final minimal", `{"action":"final"}`, "", actionFinal},
		{"surrounding whitespace", "  \n{\"action\":\"final\"}\n ", "", actionFinal},
		{"empty", "", "empty action", ""},
		{"not json", "search for cats", "not valid JSON", ""},
		{"trailing content", `{"action":"final"} {"action":"shell"}`, "content after the JSON object", ""},
		{"trailing garbage", `{"action":"final"} run rm -rf /`, "content after the JSON object", ""},
		{"unknown field", `{"action":"final","exec":"x"}`, "not valid JSON", ""},
		{"missing discriminator", `{"query":"q"}`, "missing the required", ""},
		{"unknown kind", `{"action":"shell","query":"rm -rf /"}`, "unknown action", ""},
		{"search without query", `{"action":"search","query":"  "}`, "missing a query", ""},
		{"fetch without url", `{"action":"fetch"}`, "missing a url", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAction(tt.raw)
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
