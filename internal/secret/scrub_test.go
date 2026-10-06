package secret

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
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
	if len(fakeGoogleKey) != 39 {
		t.Fatalf("fakeGoogleKey is %d chars, want 39", len(fakeGoogleKey))
	}

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
		{"high entropy token", "got credential " + fakeToken + " from resolver", fakeToken},
		{"hex token", "session 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Scrub(tt.in)
			if strings.Contains(got, tt.leaked) {
				t.Fatalf("Scrub(%q) = %q; credential survived", tt.in, got)
			}
			if !strings.Contains(got, "[REDACTED") {
				t.Fatalf("Scrub(%q) = %q; no redaction marker", tt.in, got)
			}
		})
	}
}

// TestScrubLangfuseKeys covers Langfuse keys on their own, in the OTLP
// Basic Authorization header Langfuse ingestion uses, in a URL, in JSON
// and in prose. Neither half of any credential may survive, so a pattern
// that stops part-way through a key fails here.
func TestScrubLangfuseKeys(t *testing.T) {
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
			got := Scrub(tt.in)
			for _, secret := range tt.leaked {
				half := len(secret) / 2
				for _, part := range []string{secret[:half], secret[half:]} {
					if strings.Contains(got, part) {
						t.Fatalf("Scrub(%q) = %q; credential fragment %q survived", tt.in, got, part)
					}
				}
			}
			if !strings.Contains(got, "[REDACTED") {
				t.Fatalf("Scrub(%q) = %q; no redaction marker", tt.in, got)
			}
		})
	}
}

func TestScrubLeavesProseAlone(t *testing.T) {
	tests := []string{
		"polling interaction for status, attempt 3 of 12",
		"credentialPatternScrubbingHandlerWrapperImplementation", // long identifier, no digits
		"the research agent searched 14 sources in 32 minutes",
		"wrote report to /tmp/chiron/deep-research-output.md",
		"basic authorization rules apply: the desk-lf team owns keys",
		"langfuse keys start pk-lf- or sk-lf- followed by a uuid",
		"the task-lf-database-name-here config key",
	}
	for _, in := range tests {
		if got := Scrub(in); got != in {
			t.Errorf("Scrub(%q) = %q; want unchanged", in, got)
		}
	}
}

// TestScrubBasicAuthSplitAcrossKeyAndValue proves the Basic-credential
// pattern fires on a value scrubbed on its own, with no "authorization"
// anywhere in the same string — the shape a tracer's SetAttr produces
// when it scrubs an attribute's key and value as two independent Scrub
// calls (see TestJSONLScrubsSplitBasicAuthHeader for the same regression
// through the real tracer).
func TestScrubBasicAuthSplitAcrossKeyAndValue(t *testing.T) {
	pair := base64.StdEncoding.EncodeToString([]byte(fakeLangfuseZeroPublic + ":" + fakeLangfuseZeroSecret))

	key := Scrub("http.request.header.authorization")
	if key != "http.request.header.authorization" {
		t.Errorf("Scrub(key) = %q; want unchanged, no credential in the key", key)
	}

	value := Scrub("Basic " + pair)
	if strings.Contains(value, pair) {
		t.Fatalf("Scrub(value) = %q; credential survived a value-only call", value)
	}
	if !strings.Contains(value, "[REDACTED") {
		t.Fatalf("Scrub(value) = %q; no redaction marker", value)
	}
}

// TestKeyCannotTransitLogger is the structural guarantee: a credential
// logged through any slog surface — message, attr, error, group,
// WithAttrs, WithGroup, LogValuer — must never reach the inner handler.
func TestKeyCannotTransitLogger(t *testing.T) {
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
	for _, leaked := range []string{fakeGoogleKey, fakeToken} {
		if strings.Contains(out, leaked) {
			t.Fatalf("credential transited the logger:\n%s", out)
		}
	}
	if !strings.Contains(out, "[REDACTED") {
		t.Fatalf("no redaction markers in output:\n%s", out)
	}
}

// leakyValuer resolves to a string carrying a credential, the way an
// API client type with a LogValue method might.
type leakyValuer struct{}

func (leakyValuer) LogValue() slog.Value {
	return slog.StringValue("resolved to " + fakeGoogleKey)
}

func TestScrubHandlerEnabledDelegates(t *testing.T) {
	var buf bytes.Buffer
	h := NewScrubHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	logger := slog.New(h)
	logger.Info("below level", "api_key", fakeGoogleKey)
	if buf.Len() != 0 {
		t.Fatalf("record below level reached inner handler: %s", buf.String())
	}
	logger.Warn("at level")
	if buf.Len() == 0 {
		t.Fatal("record at level did not reach inner handler")
	}
}
