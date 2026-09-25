package secret

import (
	"encoding/base64"
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Credential patterns, applied in order: header, bearer and basic forms
// first (their values would otherwise be half-eaten by the narrower
// patterns), then known key shapes, then the high-entropy backstop. The
// backstop is deliberately biased towards scrubbing — redacting an
// innocent token is an annoyance, leaking a key is unrecoverable.
var (
	// x-goog-api-key header or assignment, however quoted.
	reGoogHeader = regexp.MustCompile(`(?i)(x-goog-api-key\b['"]?\s*[:=]\s*['"]?)[A-Za-z0-9._~+/=-]+`)

	// RFC 6750 bearer credentials.
	reBearer = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/-]+=*`)

	// RFC 7617 Basic credentials in an Authorization header or assignment,
	// however quoted or URL-encoded. Header context is always redacted, so
	// the token class also admits base64url and JSON-escaped slashes.
	reBasicHeader = regexp.MustCompile(`(?i)(authorization\b['"]?(?:\s*[:=]|%3A)(?:\s|%20)*['"]?basic(?:\s|%20)+)[A-Za-z0-9+/_\\-]+=*`)

	// Bare Basic credentials. Redacted only when the token decodes to
	// user:password text, so prose such as "basic research" survives.
	reBasic = regexp.MustCompile(`(?i)\b(basic(?:\s|%20)+)([A-Za-z0-9+/]{4,}=*)`)

	// Google API keys: AIza plus 35 key characters.
	reGoogleKey = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)

	// Langfuse project keys: pk-lf- (public) or sk-lf- (secret) and at
	// least eight key characters, also inside a longer word. Generated keys
	// carry a UUID; self-hosted projects may set shorter ones.
	reLangfuseKey = regexp.MustCompile(`[ps]k-lf-[A-Za-z0-9_-]{8,}`)

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
	s = reBasicHeader.ReplaceAllString(s, "${1}[REDACTED:basic-credentials]")
	s = reBasic.ReplaceAllStringFunc(s, func(match string) string {
		m := reBasic.FindStringSubmatch(match)
		if !basicCredential(m[2]) {
			return match
		}
		return m[1] + "[REDACTED:basic-credentials]"
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

// basicCredential reports whether tok is base64 of printable text holding a
// colon: the user:password shape of an RFC 7617 credential. A truncated
// token whose last character completes no byte is decoded without it.
func basicCredential(tok string) bool {
	tok = strings.TrimRight(tok, "=")
	if len(tok)%4 == 1 {
		tok = tok[:len(tok)-1]
	}
	raw, err := base64.RawStdEncoding.DecodeString(tok)
	if err != nil || !utf8.Valid(raw) {
		return false
	}
	text := string(raw)
	if !strings.Contains(text, ":") {
		return false
	}
	for _, r := range text {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
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
