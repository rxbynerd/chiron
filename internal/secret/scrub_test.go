package secret

import (
	"bytes"
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

func TestScrubLeavesProseAlone(t *testing.T) {
	tests := []string{
		"polling interaction for status, attempt 3 of 12",
		"credentialPatternScrubbingHandlerWrapperImplementation", // long identifier, no digits
		"the research agent searched 14 sources in 32 minutes",
		"wrote report to /tmp/chiron/deep-research-output.md",
	}
	for _, in := range tests {
		if got := Scrub(in); got != in {
			t.Errorf("Scrub(%q) = %q; want unchanged", in, got)
		}
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
