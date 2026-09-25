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

// Langfuse project keys: a pk-lf- or sk-lf- prefix and a lower-case UUID,
// which the high-entropy backstop alone does not catch.
const (
	fakeLangfusePublic = "pk-lf-1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d"
	fakeLangfuseSecret = "sk-lf-6d5c4b3a-2f1e-0d9c-8b7a-6f5e4d3c2b1a"
)

func TestScrubLangfuseAndBasicCredentials(t *testing.T) {
	langfuseBasic := base64.StdEncoding.EncodeToString([]byte(fakeLangfusePublic + ":" + fakeLangfuseSecret))
	shortBasic := base64.StdEncoding.EncodeToString([]byte("user:pass"))

	tests := []struct {
		name   string
		in     string
		leaked string // must not survive scrubbing
	}{
		{"langfuse public key", "public key " + fakeLangfusePublic + " rejected", fakeLangfusePublic},
		{"langfuse secret key json", `{"secretKey":"` + fakeLangfuseSecret + `"}`, fakeLangfuseSecret},
		{"langfuse key non-uuid tail", "using sk-lf-abcdefghij_klmnopqrst-uv now", "abcdefghij_klmnopqrst-uv"},
		{"authorization basic header", "Authorization: Basic " + langfuseBasic, langfuseBasic},
		{"authorization basic lowercase", "authorization: basic " + shortBasic, shortBasic},
		{"authorization basic json", `{"Authorization":"Basic ` + shortBasic + `"}`, shortBasic},
		{"authorization basic unpadded", "Authorization: Basic " + strings.TrimRight(langfuseBasic, "="), strings.TrimRight(langfuseBasic, "=")},
		{"bare basic credential", "sent Basic " + shortBasic + " to the collector", shortBasic},
		{"bare basic langfuse credential", "credential Basic " + langfuseBasic, langfuseBasic},
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

// survivor returns the longest substring of secret, at least eight bytes
// long, that appears in out.
func survivor(secret, out string) string {
	best := ""
	for i := range len(secret) {
		for j := len(secret); j-i > len(best) && j-i >= 8; j-- {
			if strings.Contains(out, secret[i:j]) {
				best = secret[i:j]
				break
			}
		}
	}
	return best
}

// TestScrubEncodedAndShortCredentials: short Langfuse keys, URL-encoded and
// base64url Basic headers, and truncated Basic tokens leave no eight-byte
// fragment behind.
func TestScrubEncodedAndShortCredentials(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte("otel-user:S3cr3t!pw"))
	truncated := short[:17]
	if len(truncated)%4 != 1 {
		t.Fatalf("truncated token is %d chars, want len%%4 == 1", len(truncated))
	}
	urlSafe := base64.URLEncoding.EncodeToString([]byte("user:pa>>ss??word~~"))
	if !strings.ContainsAny(urlSafe, "-_") {
		t.Fatalf("%q has no base64url character", urlSafe)
	}

	tests := []struct {
		name   string
		in     string
		secret string
	}{
		{"short langfuse key json", `{"secretKey":"sk-lf-1234567890"}`, "sk-lf-1234567890"},
		{"langfuse key after a word character", "x" + fakeLangfuseSecret, strings.TrimPrefix(fakeLangfuseSecret, "sk-lf-")},
		{"otel header variable form", "Authorization=Basic%20" + short, short},
		{"url-encoded header", "Authorization%3A%20Basic%20" + short, short},
		{"truncated bare basic token", "Basic " + truncated, truncated},
		{"base64url header token", "Authorization: Basic " + urlSafe, urlSafe},
		{"short langfuse key in a resolver error", "environment variable pk-lf-1234567890 is not set", "pk-lf-1234567890"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Scrub(tt.in)
			if s := survivor(tt.secret, got); s != "" {
				t.Fatalf("Scrub(%q) = %q; %q survived", tt.in, got, s)
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
		"a basic search found 3 results",
		"Basic research methods apply here",
		"Basic auth is disabled; the basic idea is simple",
		"keys carry a pk-lf- or sk-lf- prefix",
		"Basic aGVsbG8gd29ybGQ=", // "hello world": no colon
		"Basic dXNlcjoBcGFzcw==", // "user:\x01pass": not printable
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
