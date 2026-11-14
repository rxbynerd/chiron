package secret

import (
	"encoding/base64"
	"math"
	"regexp"
	"strings"
)

// minBearerLen is the shortest value ScrubShapes redacts after a bare
// "bearer" in prose.
const minBearerLen = 16

const (
	// bearerSep is what separates a "bearer" scheme word from its value.
	bearerSep = `(?:[\s\x{a0}\x{202f}:]|%20)`
	// valueWrap is the opening markup that may precede a bearer value.
	valueWrap = `["'` + "`" + `*(]*`
)

// Credential patterns, applied in order: header and bearer forms first
// (their values would otherwise be half-eaten by the narrower patterns),
// then known key shapes, then the high-entropy backstop. The backstop is
// deliberately biased towards scrubbing — redacting an innocent token is
// an annoyance, leaking a key is unrecoverable.
var (
	// x-goog-api-key header or assignment, however quoted.
	reGoogHeader = regexp.MustCompile(`(?i)(x-goog-api-key\b['"]?\s*[:=]\s*['"]?)[A-Za-z0-9._~+/=-]+`)

	// x-api-key header or assignment, however quoted, including Go's
	// header-map form (X-Api-Key:[value]) and a value with a Bearer prefix.
	reAPIKeyHeader = regexp.MustCompile(`(?i)\b(x-api-key\b['"]?\s*[:=]\s*[\['"]?\s*(?:bearer\s+)?)[A-Za-z0-9._~+/=-]+`)

	// RFC 6750 bearer credentials.
	reBearer = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/-]+=*`)

	// RFC 7617 basic credentials after a "Basic" scheme token, including
	// the percent-encoded separator an OTEL_EXPORTER_OTLP_HEADERS value
	// uses. Unanchored on any header name: a key and its value reach Scrub
	// as two independent calls (see the tracers' SetAttr), so a header
	// shaped like x-goog-api-key holding "authorization" in the key and
	// "Basic <token>" in the value would never share a string with an
	// "authorization"-anchored pattern. The candidate is only redacted
	// once it structurally base64-decodes to bytes containing ':', so
	// ordinary prose such as "basic authorization rules apply" survives.
	reBasicAuth = regexp.MustCompile(`(?i)\b(basic(?:\s+|%20))([A-Za-z0-9._~+/:%-]+=*)`)

	// Google API keys: AIza plus 35 key characters.
	reGoogleKey = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)

	// Langfuse public and secret keys: pk-lf- or sk-lf- plus a UUID. A
	// lower-case UUID has no upper-case letter, so the backstop misses it.
	// Anchored like every sibling pattern, so a trailing identifier that
	// merely contains the shape (task-lf-database-name-here) is untouched.
	reLangfuseKey = regexp.MustCompile(`(?i)\b[ps]k-lf-[0-9a-z-]{8,}`)

	// Report-text bearer forms. Between the scheme word and the value an
	// RFC 6750 header has whitespace, but text copied from a page may use a
	// colon, a percent-encoded space or a non-breaking space, and may wrap
	// the value in backticks, bold or quotes. "[" is deliberately not a
	// wrapper, so a redaction marker is never matched again.
	reBearerReport = regexp.MustCompile(`(?i)\b(bearer` + bearerSep + `+` + valueWrap + `)([A-Za-z0-9._~+/-]+=*)`)

	// A bearer credential in an Authorization header or assignment. The
	// scheme and the header name together make the value a credential
	// whatever it looks like.
	reAuthorizationBearer = regexp.MustCompile(`(?i)\b(authorization[*"'` + "`" + `]*\s*[:=][\s*"'` + "`" + `]*bearer` + bearerSep + `+` + valueWrap + `)([A-Za-z0-9._~+/-]+=*)`)

	// Provider API keys: "sk-" (OpenAI, Anthropic "sk-ant-", OpenRouter and
	// compatible providers) followed by at least 32 key characters. A
	// candidate that is a plain hyphenated slug is not a key; see
	// looksProviderKey.
	reProviderKey = regexp.MustCompile(`\bsk-([A-Za-z0-9_-]{32,})`)

	// GitHub tokens: classic and app tokens (ghp_, gho_, ghu_, ghs_, ghr_)
	// and fine-grained personal access tokens (github_pat_).
	reGitHubToken = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`)

	// JSON Web Tokens: an encoded header and payload, both starting with the
	// base64 of `{"`, then a possibly empty signature.
	reJWT = regexp.MustCompile(`\beyJ[\w-]{8,}\.eyJ[\w-]{8,}\.[\w-]*`)

	// A PEM private-key block, through its END line or, when the text is cut
	// short, to the end of the text.
	rePrivateKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`)

	// Slack tokens: xoxa, xoxb, xoxp, xoxr and xoxs plus the token body.
	reSlackToken = regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)

	// Stripe secret and restricted keys, live or test.
	reStripeKey = regexp.MustCompile(`\b[rs]k_(?:live|test)_[A-Za-z0-9]{16,}`)

	// AWS access key ids, long-term (AKIA) and temporary (ASIA).
	reAWSAccessKeyID = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)

	// Google OAuth access tokens.
	reGoogleOAuthToken = regexp.MustCompile(`\bya29\.[A-Za-z0-9._-]{20,}`)

	// Hugging Face tokens.
	reHuggingFaceToken = regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}`)

	// The signature of a presigned AWS URL.
	reAWSSignature = regexp.MustCompile(`(?i)(\bX-Amz-Signature=)[A-Za-z0-9]{16,}`)

	// Candidate runs for the high-entropy backstop.
	reCandidate = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)
)

// Scrub replaces credential-shaped substrings with redaction markers. It
// is the scrubbing primitive for diagnostics: the slog handler wrapper and
// the tracers all route string payloads through it, so a key cannot transit
// logs or traces. It is deliberately aggressive, and so unsuitable for
// research prose, where it redacts commit hashes, checksums and long URL
// paths; use ScrubShapes for model-written report text.
func Scrub(s string) string {
	s = redactShapes(s, false)
	s = reCandidate.ReplaceAllStringFunc(s, func(tok string) string {
		if highEntropy(tok) {
			return "[REDACTED:high-entropy]"
		}
		return tok
	})
	return s
}

// ScrubShapes replaces text that has the shape of a credential with
// redaction markers and applies no entropy heuristic, so identifiers such as
// commit hashes, checksums and URL paths survive. It is for model-written
// report text and the findings saved from it, which never hold the run's own
// credentials but may quote a third party's from a fetched page. Everything
// that reaches logs, spans, errors or stderr goes through Scrub instead.
//
// The result is a fixed point: scrubbing it again changes nothing.
func ScrubShapes(s string) string {
	s = rePrivateKey.ReplaceAllString(s, "[REDACTED:private-key]")
	s = redactShapes(s, true)
	s = reJWT.ReplaceAllString(s, "[REDACTED:jwt]")
	s = reSlackToken.ReplaceAllString(s, "[REDACTED:slack-token]")
	s = reStripeKey.ReplaceAllString(s, "[REDACTED:stripe-key]")
	s = reAWSAccessKeyID.ReplaceAllString(s, "[REDACTED:aws-access-key-id]")
	s = reGoogleOAuthToken.ReplaceAllString(s, "[REDACTED:google-oauth-token]")
	s = reHuggingFaceToken.ReplaceAllString(s, "[REDACTED:huggingface-token]")
	s = reAWSSignature.ReplaceAllString(s, "${1}[REDACTED:aws-signature]")
	s = reProviderKey.ReplaceAllStringFunc(s, func(match string) string {
		if !looksProviderKey(match[len("sk-"):]) {
			return match
		}
		return "[REDACTED:api-key]"
	})
	return reGitHubToken.ReplaceAllString(s, "[REDACTED:github-token]")
}

// redactShapes applies the header, bearer, Basic, Google and Langfuse
// patterns shared by Scrub and ScrubShapes in a fixed order. With report
// set it uses the report-text bearer rules and leaves markers it wrote
// alone; without it the output is exactly what Scrub has always produced.
func redactShapes(s string, report bool) string {
	s = reGoogHeader.ReplaceAllString(s, "${1}[REDACTED:x-goog-api-key]")
	s = redactAPIKeyHeader(s, report)
	if report {
		s = redactAuthorizationBearer(s)
		s = redactReportBearer(s)
	} else {
		s = reBearer.ReplaceAllString(s, "${1}[REDACTED:bearer-token]")
	}
	s = reBasicAuth.ReplaceAllStringFunc(s, func(match string) string {
		sub := reBasicAuth.FindStringSubmatch(match)
		token := strings.TrimRight(sub[2], ".")
		if !looksBasicCredential(token) {
			return match
		}
		return sub[1] + "[REDACTED:basic-credentials]" + sub[2][len(token):]
	})
	s = reGoogleKey.ReplaceAllString(s, "[REDACTED:google-api-key]")
	return reLangfuseKey.ReplaceAllString(s, "[REDACTED:langfuse-key]")
}

// redactAPIKeyHeader redacts the value of an x-api-key header or
// assignment. With keepMarkers set, a value that is the remains of an
// earlier redaction is left alone, since the pattern would otherwise read
// the marker as a value and wrap it again.
func redactAPIKeyHeader(s string, keepMarkers bool) string {
	const marker = "[REDACTED:x-api-key]"
	if !keepMarkers {
		return reAPIKeyHeader.ReplaceAllString(s, "${1}"+marker)
	}
	var b strings.Builder
	last := 0
	for _, loc := range reAPIKeyHeader.FindAllStringSubmatchIndex(s, -1) {
		valueStart, end := loc[3], loc[1]
		value, rest := s[valueStart:end], s[end:]
		if isMarkerRemains(value, rest) {
			continue
		}
		b.WriteString(s[last:valueStart])
		b.WriteString(marker)
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// isMarkerRemains reports whether value, followed by rest, is part of an
// x-api-key marker: the marker's own text, or the Bearer word in front of
// it.
func isMarkerRemains(value, rest string) bool {
	if value == "REDACTED" {
		return strings.HasPrefix(rest, ":x-api-key]")
	}
	return strings.EqualFold(value, "bearer") && strings.HasPrefix(strings.TrimLeft(rest, " \t"), "[REDACTED:")
}

// redactAuthorizationBearer redacts the value of a bearer Authorization
// header whatever it looks like.
func redactAuthorizationBearer(s string) string {
	return reAuthorizationBearer.ReplaceAllString(s, "${1}[REDACTED:bearer-token]")
}

// redactReportBearer redacts a value after a "bearer" scheme word outside an
// Authorization header only when it looks like a token, so prose such as "a
// bearer token with scopes" survives.
func redactReportBearer(s string) string {
	return reBearerReport.ReplaceAllStringFunc(s, func(match string) string {
		sub := reBearerReport.FindStringSubmatch(match)
		if !looksBearerCredential(sub[2]) {
			return match
		}
		return sub[1] + "[REDACTED:bearer-token]"
	})
}

// looksBearerCredential reports whether value, the text following a
// "bearer" scheme word in prose, has the length and digit content of a
// token.
func looksBearerCredential(value string) bool {
	value = strings.TrimRight(value, "=")
	return len(value) >= minBearerLen && strings.ContainsAny(value, "0123456789")
}

// looksProviderKey reports whether body, the text after "sk-", is key
// material rather than a hyphenated slug. A slug is made only of segments
// separated by "-" or "_" that are each a lower-case word of at most 16
// letters, a number of at most 4 digits, or a code of at most 3 lower-case
// letters and digits (such as "v2" or "q4"). Any upper-case letter, longer
// segment or longer mixed segment, as in sk-proj-..., sk-ant-api03-... or
// a long alphanumeric body, makes it a key.
func looksProviderKey(body string) bool {
	for _, seg := range strings.FieldsFunc(body, func(r rune) bool { return r == '-' || r == '_' }) {
		if !isSlugSegment(seg) {
			return true
		}
	}
	return false
}

func isSlugSegment(seg string) bool {
	letters, digits := 0, 0
	for _, r := range seg {
		switch {
		case r >= 'a' && r <= 'z':
			letters++
		case r >= '0' && r <= '9':
			digits++
		default:
			return false
		}
	}
	switch {
	case digits == 0:
		return letters <= 16
	case letters == 0:
		return digits <= 4
	default:
		return len(seg) <= 3
	}
}

// looksBasicCredential reports whether token, the text following a
// "Basic" scheme word, is an RFC 7617 credential pair: it decodes as
// base64 (standard or URL alphabet, padded or not) to bytes containing a
// ':' separating user from password. A plain word like "authorization"
// fails to decode at all; prose that happens to decode rarely contains a
// literal colon.
func looksBasicCredential(token string) bool {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if decoded, err := enc.DecodeString(token); err == nil && strings.Contains(string(decoded), ":") {
			return true
		}
	}
	return false
}

// highEntropy reports whether tok looks like a machine-generated
// credential rather than prose or an identifier. Two shapes qualify:
// long hexadecimal runs containing digits, and mixed-case runs with
// digits whose Shannon entropy is at or above 4.0 bits per character —
// camelCase identifiers fail the digit requirement, English prose fails
// the entropy bar, random base64-ish key material clears both.
func highEntropy(tok string) bool {
	var digits, upper, lower int
	hexish := true
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r >= 'A' && r <= 'Z':
			upper++
		case r >= 'a' && r <= 'z':
			lower++
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			hexish = false
		}
	}
	if hexish && len(tok) >= 40 && digits >= 4 {
		return true
	}
	return digits >= 2 && upper >= 1 && lower >= 1 && shannon(tok) >= 4.0
}

// shannon computes the sample Shannon entropy of s in bits per character.
func shannon(s string) float64 {
	freq := make(map[rune]int)
	var n float64
	for _, r := range s {
		freq[r]++
		n++
	}
	var h float64
	for _, c := range freq {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
