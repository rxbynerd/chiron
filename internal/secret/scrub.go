package secret

import (
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

	// Google API keys: AIza plus 35 key characters.
	reGoogleKey = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)

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
	s = reGoogleKey.ReplaceAllString(s, "[REDACTED:google-api-key]")
	s = reCandidate.ReplaceAllStringFunc(s, func(tok string) string {
		if highEntropy(tok) {
			return "[REDACTED:high-entropy]"
		}
		return tok
	})
	return s
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
