package secret

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeGoogleKey is shaped exactly like a Google API key (AIza + 35 key
// characters) without being one.
const fakeGoogleKey = "AIzaAb1-x9QzAb1-x9QzAb1-x9QzAb1-x9QzAb1"

// fakeToken is a generic high-entropy credential: 32 mixed-case
// characters with digits and near-maximal sample entropy.
const fakeToken = "9fK2mQ8vL4xR7tB1nZ5cW3jY6uH0eP8s"

// Langfuse-shaped keys: pk-lf- or sk-lf- plus a lower-case UUID, the form
// the high-entropy backstop cannot see. fakeLangfuseZeroSecret is a
// degenerate key whose base64 Basic credential is low-entropy too.
const (
	fakeLangfusePublic     = "pk-lf-1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	fakeLangfuseSecret     = "sk-lf-9f8e7d6c-5b4a-4321-8fed-cba987654321"
	fakeLangfuseZeroPublic = "pk-lf-00000000-0000-0000-0000-000000000000"
	fakeLangfuseZeroSecret = "sk-lf-00000000-0000-0000-0000-000000000000"
)

func TestScrubPatterns(t *testing.T) {
	must := require.New(t)
	must.Len(fakeGoogleKey, 39, "fakeGoogleKey must match the Google API key length")

	tests := []struct {
		name   string
		in     string
		leaked string // must not survive scrubbing
	}{
		{"google api key", "request failed for key " + fakeGoogleKey + " try again", fakeGoogleKey},
		{"google api key in url", "GET /v1beta/interactions?key=" + fakeGoogleKey, fakeGoogleKey},
		{"bearer token", "Authorization: Bearer ya29.tok-" + fakeToken, fakeToken},
		{"bearer lowercase", "authorization: bearer " + fakeToken, fakeToken},
		{"x-goog-api-key header", "x-goog-api-key: " + fakeGoogleKey, fakeGoogleKey},
		{"x-goog-api-key json", `{"x-goog-api-key":"` + fakeGoogleKey + `"}`, fakeGoogleKey},
		{"x-api-key header lowercase uuid", "x-api-key: 3f2b9c1e-5a7d-4e8f-9b0a-1c2d3e4f5a6b", "3f2b9c1e-5a7d-4e8f-9b0a-1c2d3e4f5a6b"},
		{"x-api-key json", `{"X-Api-Key":"3f2b9c1e-5a7d-4e8f-9b0a-1c2d3e4f5a6b"}`, "3f2b9c1e-5a7d-4e8f-9b0a-1c2d3e4f5a6b"},
		{"x-api-key go header map", "map[X-Api-Key:[3f2b9c1e-5a7d-4e8f-9b0a-1c2d3e4f5a6b]]", "3f2b9c1e-5a7d-4e8f-9b0a-1c2d3e4f5a6b"},
		{"x-api-key bearer value", "x-api-key: Bearer 3f2b9c1e-5a7d-4e8f-9b0a-1c2d3e4f5a6b", "3f2b9c1e-5a7d-4e8f-9b0a-1c2d3e4f5a6b"},
		{"high entropy token", "got credential " + fakeToken + " from resolver", fakeToken},
		{"hex token", "session 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got := Scrub(tt.in)
			must.NotContains(got, tt.leaked, "Scrub(%q): credential survived", tt.in)
			must.Contains(got, "[REDACTED", "Scrub(%q): no redaction marker", tt.in)
		})
	}
}

// Langfuse keys are scrubbed on their own, in the OTLP Basic Authorization
// header Langfuse ingestion uses, in a URL, in JSON and in prose. Neither
// half of any credential may survive, so a pattern that stops part-way
// through a key fails here.
func TestScrub_LangfuseKeys_NeitherHalfSurvives(t *testing.T) {
	basic := func(public, secret string) string {
		return base64.StdEncoding.EncodeToString([]byte(public + ":" + secret))
	}
	pair := basic(fakeLangfusePublic, fakeLangfuseSecret)
	zeroPair := basic(fakeLangfuseZeroPublic, fakeLangfuseZeroSecret)

	tests := []struct {
		name   string
		in     string
		leaked []string
	}{
		{"public key alone", fakeLangfusePublic, []string{fakeLangfusePublic}},
		{"secret key alone", fakeLangfuseSecret, []string{fakeLangfuseSecret}},
		{"upper-case uuid", fakeLangfuseSecret[:6] + strings.ToUpper(fakeLangfuseSecret[6:]),
			[]string{strings.ToUpper(fakeLangfuseSecret[6:])}},
		{"upper-cased key", strings.ToUpper(fakeLangfuseSecret), []string{strings.ToUpper(fakeLangfuseSecret)}},
		{"zero-uuid key", "key " + fakeLangfuseZeroSecret, []string{fakeLangfuseZeroSecret}},
		{"basic header base64", "Authorization: Basic " + pair, []string{pair}},
		{"basic header base64 low entropy", "Authorization: Basic " + zeroPair, []string{zeroPair}},
		{"basic header raw pair", "authorization: basic " + fakeLangfusePublic + ":" + fakeLangfuseSecret,
			[]string{fakeLangfusePublic, fakeLangfuseSecret}},
		{"otlp headers env", "OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic%20" + zeroPair, []string{zeroPair}},
		{"basic header json", `{"Authorization":"Basic ` + zeroPair + `"}`, []string{zeroPair}},
		{"url query", "https://cloud.langfuse.com/api/public/otel?public_key=" + fakeLangfusePublic, []string{fakeLangfusePublic}},
		{"json", `{"publicKey":"` + fakeLangfusePublic + `","secretKey":"` + fakeLangfuseSecret + `"}`,
			[]string{fakeLangfusePublic, fakeLangfuseSecret}},
		{"prose", "langfuse rejected " + fakeLangfuseSecret + " as invalid, retrying", []string{fakeLangfuseSecret}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got := Scrub(tt.in)
			for _, secret := range tt.leaked {
				half := len(secret) / 2
				for _, part := range []string{secret[:half], secret[half:]} {
					must.NotContains(got, part, "Scrub(%q): credential fragment survived", tt.in)
				}
			}
			must.Contains(got, "[REDACTED", "Scrub(%q): no redaction marker", tt.in)
		})
	}
}

func TestScrubLeavesProseAlone(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"poll status", "polling interaction for status, attempt 3 of 12"},
		{"long identifier", "credentialPatternScrubbingHandlerWrapperImplementation"}, // no digits
		{"search summary", "the research agent searched 14 sources in 32 minutes"},
		{"report path", "wrote report to /tmp/chiron/deep-research-output.md"},
		{"basic authorization prose", "basic authorization rules apply: the desk-lf team owns keys"},
		{"langfuse key prose", "langfuse keys start pk-lf- or sk-lf- followed by a uuid"},
		{"lf config key", "the task-lf-database-name-here config key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.in, Scrub(tt.in))
		})
	}
}

// Attribute keys and values are scrubbed in separate calls, so a Basic
// credential must be redacted when it is scrubbed without its key.
func TestScrub_BasicAuth_RedactsValueWithoutAuthorizationKey(t *testing.T) {
	pair := base64.StdEncoding.EncodeToString([]byte(fakeLangfuseZeroPublic + ":" + fakeLangfuseZeroSecret))

	t.Run("key is left unchanged", func(t *testing.T) {
		must := require.New(t)
		must.Equal("http.request.header.authorization", Scrub("http.request.header.authorization"))
	})

	t.Run("value is redacted on its own", func(t *testing.T) {
		must := require.New(t)
		value := Scrub("Basic " + pair)
		must.NotContains(value, pair, "credential survived a value-only call")
		must.Contains(value, "[REDACTED", "no redaction marker")
	})
}

// TestKeyCannotTransitLogger is the structural guarantee: a credential
// logged through any slog surface — message, attr, error, group,
// WithAttrs, WithGroup, LogValuer — must never reach the inner handler.
func TestKeyCannotTransitLogger(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := slog.New(NewScrubHandler(slog.NewJSONHandler(&buf, nil)))

	logger.Info("key is " + fakeGoogleKey)
	logger.Info("attr", "api_key", fakeGoogleKey)
	logger.Info("error", "err", errors.New("auth failed for "+fakeGoogleKey))
	logger.Info("any", slog.Any("detail", fmt.Errorf("wrapped: %w", errors.New(fakeToken))))
	logger.Info("group", slog.Group("request", slog.String("key", fakeGoogleKey)))
	logger.With("bound_key", fakeGoogleKey).Info("via WithAttrs")
	logger.WithGroup("creds").Info("via WithGroup", "token", fakeToken)
	logger.Info("logvaluer", "v", leakyValuer{})

	out := buf.String()
	must.NotContains(out, fakeGoogleKey, "credential transited the logger:\n%s", out)
	must.NotContains(out, fakeToken, "credential transited the logger:\n%s", out)
	must.Contains(out, "[REDACTED", "no redaction markers in output:\n%s", out)
}

// leakyValuer resolves to a string carrying a credential, the way an
// API client type with a LogValue method might.
type leakyValuer struct{}

func (leakyValuer) LogValue() slog.Value {
	return slog.StringValue("resolved to " + fakeGoogleKey)
}

func TestScrubHandler_Enabled_DelegatesLevelFiltering(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	h := NewScrubHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	logger := slog.New(h)
	logger.Info("below level", "api_key", fakeGoogleKey)
	must.Empty(buf.String(), "record below level reached inner handler")
	logger.Warn("at level")
	must.NotEmpty(buf.String(), "record at level did not reach inner handler")
}
