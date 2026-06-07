package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadBaseExplicitFile pins C2-TEST-4 (1/2): --config <file> is the
// operator's persistent-base path; the file's values must actually
// reach the resolved config, not just be accepted silently.
func TestLoadBaseExplicitFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "base.yaml")
	if err := os.WriteFile(path, []byte("agent: deep-research-max\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := execute(t, "research-config", "--config", path)
	if err != nil {
		t.Fatalf("research-config --config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if cfg["agent"] != "deep-research-max" {
		t.Errorf("agent = %v — the base file's value never reached the config", cfg["agent"])
	}
}

// TestLoadBaseExplicitFileMissing pins C2-TEST-4 (2/2): a missing
// --config file fails before any seam is built — no client, no
// request, no spend.
func TestLoadBaseExplicitFileMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a missing config file must fail before any request, got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	_, _, err := execute(t, "research", "--query", "q", "--config", "/no/such/file.yaml", "-o", "none")
	if err == nil || !strings.Contains(err.Error(), "open base config") {
		t.Errorf("err = %v, want the open-base-config failure", err)
	}
}

// TestLoadBaseExplicitFileMalformed: malformed YAML in the base file is
// a loud failure, not silently-ignored config.
func TestLoadBaseExplicitFileMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte(":::not yaml:::\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execute(t, "research-config", "--config", path); err == nil {
		t.Error("malformed base config was accepted silently")
	}
}
