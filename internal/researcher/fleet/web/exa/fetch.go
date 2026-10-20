package exa

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"github.com/rxbynerd/chiron/internal/httpx"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/web"
)

const (
	// maxURLBytes is Exa's limit on one requested URL.
	maxURLBytes = 2048
	// pageContentType labels Exa's text view, which is Markdown-style.
	pageContentType = "text/markdown; charset=utf-8"
	statusSuccess   = "success"
	statusError     = "error"
)

// localSuffixes are special-use and local-network name suffixes whose hosts
// are never public, so a URL naming one is never disclosed to Exa.
var localSuffixes = []string{".localhost", ".local", ".internal", ".home.arpa", ".lan"}

// FetchError reports that Exa answered but could not retrieve the page: the
// requested URL's status was an error. Tag is Exa's reason, such as
// CRAWL_NOT_FOUND, and TargetStatus the HTTP status the target page
// answered, 0 when Exa reported none.
type FetchError struct {
	URL          string
	Tag          string
	TargetStatus int
}

func (e *FetchError) Error() string {
	msg := fmt.Sprintf("exa: could not retrieve %s", e.URL)
	if e.Tag != "" {
		msg += ": " + e.Tag
	}
	if e.TargetStatus != 0 {
		msg += fmt.Sprintf(" (the page answered HTTP %d)", e.TargetStatus)
	}
	return msg
}

// Fetch asks Exa for the text of one page, bounded to MaxCharacters, and
// returns it as Markdown. The URL is checked first (checkURL); a refusal
// wraps web.ErrRefusedDestination and sends nothing. Page.URL is the URL as
// sent, without its fragment: Exa reports no final URL after redirects.
// Truncated is set when the text reached MaxCharacters, so a page of exactly
// that length also reads as truncated. A URL Exa could not retrieve is a
// *FetchError; the returned Page still carries the reply's reported spend.
func (c *Client) Fetch(ctx context.Context, rawURL string) (web.Page, error) {
	target, err := checkURL(rawURL)
	if err != nil {
		return web.Page{}, err
	}
	payload, err := json.Marshal(contentsRequest{
		URLs:        []string{target},
		Text:        textOptions{MaxCharacters: c.maxCharacters},
		MaxAgeHours: c.maxAgeHours,
	})
	if err != nil {
		return web.Page{}, fmt.Errorf("exa: encoding contents request: %w", err)
	}

	data, spend, err := c.post(ctx, contentsPath, payload)
	if err != nil {
		return web.Page{Spend: spend}, err
	}
	var env contentsEnvelope
	if err := decodeObject(data, &env); err != nil {
		return web.Page{Spend: web.Spend{Unknown: true}}, fmt.Errorf("exa: decoding contents response: %s", c.scrub(err.Error()))
	}
	spend = spendFrom(env.CostDollars)

	if len(env.Statuses) != 1 || env.Statuses[0].ID != target {
		return web.Page{Spend: spend}, fmt.Errorf("exa: the contents reply has no single status for %s", c.scrub(target))
	}
	switch st := env.Statuses[0]; st.Status {
	case statusSuccess:
	case statusError:
		return web.Page{Spend: spend}, c.fetchError(target, st.Error)
	default:
		return web.Page{Spend: spend}, fmt.Errorf("exa: the contents reply has an unknown status for %s", c.scrub(target))
	}
	if len(env.Results) != 1 || strings.TrimSpace(env.Results[0].Text) == "" {
		return web.Page{Spend: spend}, fmt.Errorf("exa: %s returned an empty page", c.scrub(target))
	}

	text, truncated := runeCut(strings.ToValidUTF8(env.Results[0].Text, "�"), c.maxCharacters)
	return web.Page{
		URL:         target,
		ContentType: pageContentType,
		Content:     []byte(text),
		Truncated:   truncated,
		Spend:       spend,
	}, nil
}

// fetchError builds the FetchError for a URL's error status, keeping only a
// well-formed tag and a plausible HTTP status.
func (c *Client) fetchError(target string, ce *ContentError) *FetchError {
	fe := &FetchError{URL: c.scrub(target)}
	if ce == nil {
		return fe
	}
	if tagPattern.MatchString(ce.Tag) {
		fe.Tag = ce.Tag
	}
	if ce.HTTPStatusCode != nil && *ce.HTTPStatusCode >= 100 && *ce.HTTPStatusCode <= 999 {
		fe.TargetStatus = *ce.HTTPStatusCode
	}
	return fe
}

// checkURL returns rawURL as it may be sent to Exa, or an error wrapping
// web.ErrRefusedDestination. It admits only an http or https URL without
// userinfo, of at most maxURLBytes once its fragment is dropped, whose host
// is a public-looking name or a public address. Refused hosts are internal
// addresses (httpx.InternalAddr), localhost, single-label names, names under
// localSuffixes, and names whose last label is numeric or not ASCII, which a
// WHATWG URL parser may read as an IPv4 address.
func checkURL(rawURL string) (string, error) {
	refuse := func(reason string) error {
		return fmt.Errorf("exa: %w: %s", web.ErrRefusedDestination, reason)
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", refuse("the URL does not parse")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", refuse("only http and https URLs are fetched")
	}
	if u.User != nil {
		return "", refuse("the URL carries userinfo")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", refuse("the URL has no host")
	}
	if reason := hostRefusal(host); reason != "" {
		return "", refuse(reason)
	}
	u.Fragment, u.RawFragment = "", ""
	target := u.String()
	if len(target) > maxURLBytes {
		return "", refuse(fmt.Sprintf("the URL exceeds %d bytes", maxURLBytes))
	}
	return target, nil
}

// hostRefusal returns why host must not be sent to Exa, or "".
func hostRefusal(host string) string {
	if ip, err := netip.ParseAddr(host); err == nil {
		if httpx.InternalAddr(ip, false) {
			return "the host is an internal address"
		}
		return ""
	}
	if host == "localhost" {
		return "the host is local"
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "the host is a single-label name"
	}
	for _, suffix := range localSuffixes {
		if strings.HasSuffix(host, suffix) || host == suffix[1:] {
			return "the host is a special-use or local-network name"
		}
	}
	if last := labels[len(labels)-1]; numericLabel(last) || !asciiLabel(last) {
		return "the host's last label is numeric or not ASCII"
	}
	return ""
}

// numericLabel reports whether label is decimal digits or a 0x-prefixed hex
// number, the forms a WHATWG IPv4 parser accepts.
func numericLabel(label string) bool {
	if label == "" {
		return false
	}
	digits := label
	isHex := false
	if len(label) >= 2 && label[0] == '0' && (label[1] == 'x' || label[1] == 'X') {
		digits, isHex = label[2:], true
	}
	for _, r := range digits {
		switch {
		case r >= '0' && r <= '9':
		case isHex && (r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'):
		default:
			return false
		}
	}
	return true
}

func asciiLabel(label string) bool {
	for i := 0; i < len(label); i++ {
		if label[i] >= 0x80 {
			return false
		}
	}
	return true
}

// runeCut cuts s to at most n runes and reports whether s held n or more.
func runeCut(s string, n int) (string, bool) {
	count := 0
	for i := range s {
		if count == n {
			return s[:i], true
		}
		count++
	}
	return s, count >= n
}
