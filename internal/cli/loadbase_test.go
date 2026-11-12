package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --config <file> is the operator's persistent-base path; the file's values
// must actually reach the resolved config, not just be accepted silently.
func TestLoadBaseExplicitFile(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "base.yaml")
	must.NoError(os.WriteFile(path, []byte("agent: deep-research-max\n"), 0o600))

	stdout, _, err := execute(t, "research-config", "--config", path)
	must.NoError(err, "research-config --config")
	var cfg map[string]any
	must.NoError(json.Unmarshal([]byte(stdout), &cfg), "stdout is not one JSON document:\n%s", stdout)
	must.Equal("deep-research-max", cfg["agent"], "the base file's value never reached the config")
}

// A missing --config file fails before any seam is built: no client, no
// request, no spend.
func TestLoadBaseExplicitFileMissing(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Fail("a missing config file must fail before any request", "got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	t.Setenv("CHIRON_GEMINI_BASE_URL", server.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")

	_, _, err := execute(t, "research", "--query", "q", "--config", "/no/such/file.yaml", "-o", "none")
	must.ErrorContains(err, "open base config")
}

// Malformed YAML in the base file is a loud failure, not silently-ignored config.
func TestLoadBase_MalformedExplicitFile_IsRejected(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "bad.yaml")
	must.NoError(os.WriteFile(path, []byte(":::not yaml:::\n"), 0o600))

	_, _, err := execute(t, "research-config", "--config", path)
	must.Error(err, "malformed base config was accepted silently")
}
