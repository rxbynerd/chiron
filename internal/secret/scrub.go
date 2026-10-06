package secret

import (
	"encoding/base64"
	"math"
	"regexp"
	"strings"
)

// Credential patterns, applied in order: header and bearer forms first
// (their values would otherwise be half-eaten by the narrower patterns),
// then known key shapes, then the high-entropy backstop. The backstop is
// deliberately biased towards scrubbing — redacting an innocent token is
// an annoyance, leaking a key is unrecoverable.
var (
	// x-goog-api-key header or assignment, however quoted.
	reGoogHeader = regexp.MustCompile(`(?i)(x-goog-api-key\b['"]?\s*[:=]\s*['"]?)[A-Za-z0-9._~+/=-]+`)

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

	// Candidate runs for the high-entropy backstop.
	reCandidate = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)
)

// Scrub replaces credential-shaped substrings with redaction markers. It
// is the single scrubbing primitive: the slog handler wrapper and the
// tracers all route string payloads through it, so a key cannot transit
// logs or traces.
func Scrub(s string) string {
	s = reGoogHeader.ReplaceAllString(s, "${1}[REDACTED:x-goog-api-key]")
	s = reBearer.ReplaceAllString(s, "${1}[REDACTED:bearer-token]")
	s = reBasicAuth.ReplaceAllStringFunc(s, func(match string) string {
		sub := reBasicAuth.FindStringSubmatch(match)
		if !looksBasicCredential(sub[2]) {
			return match
		}
		return sub[1] + "[REDACTED:basic-credentials]"
	})
	s = reGoogleKey.ReplaceAllString(s, "[REDACTED:google-api-key]")
	s = reLangfuseKey.ReplaceAllString(s, "[REDACTED:langfuse-key]")
	s = reCandidate.ReplaceAllStringFunc(s, func(tok string) string {
		if highEntropy(tok) {
			return "[REDACTED:high-entropy]"
		}
		return tok
	})
	return s
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
