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

// joined concatenates its parts. The fake credentials below are built from
// parts at run time so that no literal in the source has the shape of a real
// credential and trips a secret scanner, while the value still exercises the
// pattern.
func joined(parts ...string) string { return strings.Join(parts, "") }

// Fake provider keys with the real shapes: "sk-" (plain, project and
// Anthropic forms) and the GitHub classic and fine-grained token forms.
var (
	fakeOpenAIKey    = joined("s", "k-proj-", "A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0")
	fakeAnthropicKey = joined("s", "k-ant-", "api03-Zk3Qm9Vx2Lr7Tb5Nc8Wd1Hf4Jg6Ps0Ya2Ue5Io")
	fakeGitHubToken  = joined("gh", "p_", "A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8")
	fakeGitHubPAT    = joined("github", "_pat_", "11ABCDEFG0a1b2c3d4e5f6_G7h8I9j0K1l2M3n4")
)

// Fake third-party credentials with the real shapes, for ScrubShapes.
var (
	fakeSKAlphanumeric   = joined("s", "k-", "Ab3dE5gH7jK9mN1pQ3sT5vW7yZ9bC2eF4hJ6kL8nP0rS2uV4")
	fakeSKHex            = joined("s", "k-", "3fa85f6457174562b3fc2c963f66afa6")
	fakeJWTHeader        = joined("ey", "JhbGciOiJIUzI1NiJ9")
	fakeJWTPayload       = joined("ey", "JzdWIiOiIxMjM0NTY3ODkwIn0")
	fakeJWTSignature     = "dQw4w9WgXcQ-SflKxwRJSMeKKF2QT4fw"
	fakeJWT              = fakeJWTHeader + "." + fakeJWTPayload + "." + fakeJWTSignature
	fakePEMBegin         = joined("-----BEGIN RSA ", "PRIVATE KEY-----")
	fakePEMEnd           = joined("-----END RSA ", "PRIVATE KEY-----")
	fakePEMBody          = fakePEMBegin + "\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\nKgEAAoIBAQC7VJTUt9Us8cKj\n" + fakePEMEnd
	fakeSlackToken       = joined("xo", "xb-", "123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx")
	fakeStripeLive       = joined("s", "k_", "live_", strings.Repeat("a1B2", 6))
	fakeStripeRestricted = joined("r", "k_", "test_", strings.Repeat("c3D4", 6))
	fakeGoogleOAuth      = joined("ya", "29.", "a0AfH6SMBx1234567890abcdefghijklmnop")
	fakeHuggingFace      = joined("h", "f_", "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789")
	fakeAWSKeyID         = joined("AK", "IA", "IOSFODNN7EXAMPLE")
	fakeAWSTempKeyID     = joined("AS", "IA", "IOSFODNN7EXAMPLE")
)

// basicUserPass is the base64 of "user:hunter2".
const basicUserPass = "dXNlcjpodW50ZXIy"

// Identifiers a report must be able to quote and a reader must be able to
// verify.
const (
	fakeCommitHash   = "9fceb02d0ae598e95dc970b74767f19372d61af8"
	fakeSHA256Digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
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

// shapeRedactionCases are inputs ScrubShapes must redact: the text that
// follows each credential's start must not survive.
var shapeRedactionCases = []struct {
	name   string
	in     string
	leaked string // must not survive scrubbing
}{
	{"google api key", "request failed for key " + fakeGoogleKey + " try again", fakeGoogleKey},
	{"google api key in url", "GET /v1beta/interactions?key=" + fakeGoogleKey, fakeGoogleKey},
	{"x-goog-api-key header", "x-goog-api-key: " + fakeGoogleKey, fakeGoogleKey},
	{"x-api-key header", "x-api-key: 3f2b9c1e-5a7d-4e8f-9b0a-1c2d3e4f5a6b", "3f2b9c1e-5a7d-4e8f-9b0a-1c2d3e4f5a6b"},
	{"x-api-key go header map", "map[X-Api-Key:[3f2b9c1e-5a7d-4e8f-9b0a-1c2d3e4f5a6b]]", "3f2b9c1e-5a7d-4e8f-9b0a-1c2d3e4f5a6b"},
	{"x-api-key bearer value", "x-api-key: Bearer 3f2b9c1e-5a7d-4e8f-9b0a-1c2d3e4f5a6b", "3f2b9c1e-5a7d-4e8f-9b0a-1c2d3e4f5a6b"},
	{"langfuse public key", "the key " + fakeLangfusePublic + " was rejected", fakeLangfusePublic},
	{"langfuse secret key", "the key " + fakeLangfuseSecret + " was rejected", fakeLangfuseSecret},
	{"authorization bearer header", "Authorization: Bearer " + joined("ya", "29.tok-") + fakeToken, fakeToken},
	{"authorization bearer lowercase", "authorization: bearer " + fakeToken, fakeToken},
	{"authorization bearer short value", "Authorization: Bearer abc123", "abc123"},
	{"authorization bearer value without a digit", "Authorization: Bearer secretvalue", "secretvalue"},
	{"authorization bearer in json", `{"Authorization": "Bearer hunter2"}`, "hunter2"},
	{"bearer jwt", "Authorization: Bearer " + fakeJWTHeader + "." + fakeJWTPayload + ".c2lnbmF0dXJl", "c2lnbmF0dXJl"},
	{"bearer token in prose", "the server wants bearer " + fakeToken, fakeToken},
	{"bearer token in backticks", "send Bearer `" + fakeToken + "` on each call", fakeToken},
	{"bearer token in bold", "send Bearer **" + fakeToken + "** on each call", fakeToken},
	{"bearer with a colon", "Bearer: " + fakeToken, fakeToken},
	{"bearer with a percent-encoded space", "?auth=Bearer%20" + fakeToken, fakeToken},
	{"bearer with a non-breaking space", "Bearer\u00a0" + fakeToken, fakeToken},
	{"basic credentials", "Authorization: Basic " + basicUserPass, basicUserPass},
	{"basic credentials before a full stop", "Authorization: Basic " + basicUserPass + ".", basicUserPass},
	{"basic raw pair", "authorization: basic " + fakeLangfusePublic + ":" + fakeLangfuseSecret, fakeLangfuseSecret},
	{"openai-style key", "set OPENAI_API_KEY=" + fakeOpenAIKey + " first", fakeOpenAIKey},
	{"anthropic key", "the key " + fakeAnthropicKey + " leaked", fakeAnthropicKey},
	{"long alphanumeric sk key", "the key " + fakeSKAlphanumeric + " leaked", fakeSKAlphanumeric},
	{"hexadecimal sk key", "the key " + fakeSKHex + " leaked", fakeSKHex},
	{"github classic token", "clone with " + fakeGitHubToken + "@github.com", fakeGitHubToken},
	{"github fine-grained token", "token " + fakeGitHubPAT + " in the log", fakeGitHubPAT},
	{"jwt in prose", "the cookie " + fakeJWT + " was set", fakeJWTSignature},
	{"jwt without a signature", "the token " + fakeJWT[:strings.LastIndex(fakeJWT, ".")+1] + " was set", fakeJWTPayload},
	{"terminated private key", "key:\n" + fakePEMBody + "\nthen more text", "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC"},
	{"unterminated private key", "key:\n" + joined("-----BEGIN OPENSSH ", "PRIVATE KEY-----") + "\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmU", "b3BlbnNzaC1rZXktdjEAAAAABG5vbmU"},
	{"slack token", "bot " + fakeSlackToken + " posted", fakeSlackToken},
	{"stripe secret key", "STRIPE_KEY=" + fakeStripeLive, fakeStripeLive},
	{"stripe restricted test key", "key " + fakeStripeRestricted, fakeStripeRestricted},
	{"aws access key id", "AWS_ACCESS_KEY_ID=" + fakeAWSKeyID, fakeAWSKeyID},
	{"aws temporary access key id", "key " + fakeAWSTempKeyID + " issued", fakeAWSTempKeyID},
	{"google oauth access token", "token " + fakeGoogleOAuth + " expires", fakeGoogleOAuth},
	{"hugging face token", "HF_TOKEN=" + fakeHuggingFace, fakeHuggingFace},
	{"presigned url signature", "https://b.s3.amazonaws.com/f?X-Amz-Date=20260101T000000Z&X-Amz-Signature=" + fakeSHA256Digest, fakeSHA256Digest},
}

func TestScrubShapes_RedactsCredentialShapes(t *testing.T) {
	for _, tt := range shapeRedactionCases {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got := ScrubShapes(tt.in)
			must.NotContains(got, tt.leaked, "ScrubShapes(%q): credential survived", tt.in)
			must.Contains(got, "[REDACTED", "ScrubShapes(%q): no redaction marker", tt.in)
		})
	}
}

// Scrubbing already-scrubbed text changes nothing, so a report can be
// scrubbed twice (a stitched fallback does) without markers wrapping each
// other.
func TestScrubShapes_IsIdempotent(t *testing.T) {
	for _, tt := range shapeRedactionCases {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			once := ScrubShapes(tt.in)
			must.Equal(once, ScrubShapes(once))
		})
	}
}

// A PEM block is redacted through its END line and no further.
func TestScrubShapes_PrivateKeyEndsAtItsEndLine(t *testing.T) {
	must := require.New(t)
	must.Equal("key:\n[REDACTED:private-key]\nthen more text", ScrubShapes("key:\n"+fakePEMBody+"\nthen more text"))
}

func TestScrubShapes_LeavesIdentifiersAndProseAlone(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"commit hash", "fixed in commit " + fakeCommitHash + " on main"},
		{"sha-256 digest", "the checksum is " + fakeSHA256Digest + "."},
		{"image digest", "pull ghcr.io/example/app@sha256:" + fakeSHA256Digest},
		{"md5 digest", "md5 d41d8cd98f00b204e9800998ecf8427e"},
		{"mixed-case identifier", "request id " + fakeToken},
		{"github commit url", "https://github.com/example/repo/commit/" + fakeCommitHash},
		{"github blob url with commit hash", "([license header](https://github.com/example/repo/blob/" + fakeCommitHash + "/Sources/Example/Example.swift))."},
		{"github blob url with mixed-case path", "([source](https://github.com/example/repo/blob/main/Sources/ExampleKit/Networking/RequestRetryPolicy2Tests.swift))"},
		{"bearer token prose", "The API uses a Bearer token in the Authorization header"},
		{"bearer table cell", "| Application bearer token with scopes, expiry, revocation | yes |"},
		{"bearer long prose word", "Bearer authentication is described in the specification"},
		{"authorization header placeholder", "send Authorization: Bearer <token> on every request"},
		{"short bearer value in prose", "the bearer abc123 is a placeholder"},
		{"bearer with a colon in prose", "a bearer: the holder of the token"},
		{"sk slug in a url", "https://example.com/reports/sk-telecom-annual-report-2024-financial-results"},
		{"sk slug with short codes", "see sk-telecom-v2-q4-annual-report-summary-results"},
		{"jwt prefix prose", "a JWT starts with eyJ and has three dot-separated parts"},
		{"slack prefix prose", "tokens start with xoxb- or xoxp- and a long suffix"},
		{"stripe prefix prose", "keys start with sk_live_ or sk_test_ and a long suffix"},
		{"aws key id lookalike", "the id " + fakeAWSKeyID + "X is one character too long"},
		{"hugging face function", "call hf_hub_download to fetch the file"},
		{"oauth prefix prose", "access tokens start with ya29. and are opaque"},
		{"signature parameter prose", "the X-Amz-Signature parameter holds an HMAC"},
		{"unclosed key header prose", "a PEM file begins with a line of dashes and BEGIN PRIVATE"},
		{"basic authorization prose", "basic authorization rules apply: the desk-lf team owns keys"},
		{"sk prefix in a word", "the disk-encryption-configuration-reference-for-all-volumes guide"},
		{"short sk prefix", "run sk-learn to fit the model"},
		{"langfuse key prose", "langfuse keys start pk-lf- or sk-lf- followed by a uuid"},
		{"github token prefix prose", "tokens start with ghp_ or github_pat_ and a long suffix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.in, ScrubShapes(tt.in))
		})
	}
}

// A sentence-ending full stop is not part of a Basic credential.
func TestScrub_BasicAuth_RedactsCredentialBeforeFullStop(t *testing.T) {
	must := require.New(t)
	got := Scrub("Authorization: Basic " + basicUserPass + ".")
	must.Equal("Authorization: Basic [REDACTED:basic-credentials].", got)
}

// Scrub keeps its aggressive behaviour for diagnostics: the identifiers
// ScrubShapes keeps are redacted, and so is the word after "Bearer".
func TestScrub_RedactsWhatScrubShapesKeeps(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		leaked string
	}{
		{"commit hash", "commit " + fakeCommitHash, fakeCommitHash},
		{"sha-256 digest", "digest " + fakeSHA256Digest, fakeSHA256Digest},
		{"bearer prose word", "a Bearer credential in the header", "credential"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got := Scrub(tt.in)
			must.NotContains(got, tt.leaked)
			must.Contains(got, "[REDACTED")
		})
	}
}
