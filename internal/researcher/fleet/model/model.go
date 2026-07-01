// Package model is a small, SDK-free net/http client for one standard
// frontier model, targeting a minimal OpenAI-compatible Chat Completions
// JSON API. It is the model substrate shared by the in-process research
// lead and workers (docs/V2-RESEARCH-AGENT §5): the lead's decompose/cite
// calls and every worker's reasoning/synthesis turn go through this one
// adapter.
//
// It is deliberately not a full OpenAI Responses adapter (an
// V2-RESEARCH-AGENT non-negotiable): the wire surface is the bare
// Chat Completions request/response needed for text and provider-native
// structured output, with all wire structs unexported and internal to the
// package. Callers map Usage into internal/types and add cost estimates;
// this package carries no domain coupling.
//
// Money-safety and security follow the Gemini adapter
// (internal/researcher/gemini, internal/interactions): the paid POST is
// never auto-retried, response bodies are bounded, cross-host redirects
// carrying the credential are refused, and the API key travels only in the
// Authorization header — never a URL, log, error, or trace.
package model

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Defaults, all overridable via Options. The body bound is generous — a
// verbose structured answer with a large schema echo stays well under it —
// so a misbehaving endpoint cannot exhaust memory, not that a normal
// response ever approaches it.
const (
	defaultRequestTimeout = 60 * time.Second
	defaultMaxBodyBytes   = 16 << 20
	maxErrorBodyBytes     = 1 << 20
	chatCompletionsPath   = "/chat/completions"
)

// Options configures a Client.
type Options struct {
	// Endpoint is the OpenAI-compatible base URL, e.g.
	// "https://api.openai.com/v1"; the client POSTs to
	// "{Endpoint}/chat/completions". Required. It must be an absolute
	// https:// URL, with http:// admitted for loopback hosts only
	// (127.0.0.1, ::1, localhost) — the credential travels to whatever
	// endpoint is set, so a cleartext or internal override would be a
	// key-exfiltration and SSRF channel (CWE-918, CWE-319).
	Endpoint string
	// Model is the model identifier sent in the request body, e.g.
	// "gpt-5.5". Required.
	Model string
	// APIKey is the ALREADY-RESOLVED literal key. This adapter never
	// resolves secret:// references itself — resolution happens at the CLI
	// composition root. Required. It is sent only in the Authorization
	// header.
	APIKey string
	// HTTPClient supplies the underlying client. nil builds one. A
	// caller-supplied client is shallow-copied so the cross-host redirect
	// policy can be set without mutating the caller's client; leave its
	// Timeout zero — per-call deadlines come from RequestTimeout.
	HTTPClient *http.Client
	// RequestTimeout bounds each Generate call including reading the body.
	// Default 60s. A caller-supplied context deadline still wins if it is
	// tighter.
	RequestTimeout time.Duration
	// MaxBodyBytes bounds how much of a response body is read before
	// failing. Default 16 MiB. A response exceeding the bound is an error,
	// not a silent truncation-to-success.
	MaxBodyBytes int64
}

// Client is a hand-rolled net/http client for the Chat Completions API.
// The API key travels only in the Authorization header: it is never
// embedded in URLs, logged, or included in error text.
type Client struct {
	endpoint       string // base URL, no trailing slash
	model          string
	apiKey         string
	httpClient     *http.Client
	requestTimeout time.Duration
	maxBodyBytes   int64
}

// New builds a Client, validating everything that can fail before any
// money is spent: the endpoint scheme, the model, and the key are all
// required. The endpoint is validated here defensively even though config
// validates it too — the adapter is a reusable seam and must not trust its
// caller to have checked.
func New(opts Options) (*Client, error) {
	if opts.APIKey == "" {
		return nil, errors.New("model: API key must not be empty")
	}
	if opts.Model == "" {
		return nil, errors.New("model: model must not be empty")
	}
	if err := validateEndpoint(opts.Endpoint); err != nil {
		return nil, err
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	// Go's net/http strips only its own sensitive headers (Authorization,
	// Cookie, ...) on cross-domain redirects, and following a same-scheme
	// cross-host redirect would still hand the Authorization header to the
	// new host. The same-host policy is set on a shallow copy (sharing the
	// caller's Transport, jar and timeout), so supplying a bare client via
	// Options.HTTPClient cannot lose the guarantee.
	hc := *httpClient
	hc.CheckRedirect = refuseCrossHostRedirects
	httpClient = &hc

	requestTimeout := opts.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	maxBodyBytes := opts.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultMaxBodyBytes
	}

	return &Client{
		endpoint:       strings.TrimSuffix(opts.Endpoint, "/"),
		model:          opts.Model,
		apiKey:         opts.APIKey,
		httpClient:     httpClient,
		requestTimeout: requestTimeout,
		maxBodyBytes:   maxBodyBytes,
	}, nil
}

// validateEndpoint applies the CHIRON_GEMINI_BASE_URL rule: an absolute
// https:// URL, with http:// admitted for loopback hosts only. This
// mirrors allowedEndpointScheme in internal/config (validated there too);
// it is re-applied here because the adapter is a reusable seam that must
// not depend on config having run. The error echoes only the endpoint,
// never a credential.
func validateEndpoint(raw string) error {
	if raw == "" {
		return errors.New("model: endpoint must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !allowedEndpointScheme(u) {
		return fmt.Errorf("model: endpoint %q must be an absolute https:// URL (http:// only for loopback test servers)", raw)
	}
	return nil
}

// allowedEndpointScheme admits https anywhere and http on loopback only —
// the same rule internal/config's allowedEndpointScheme and internal/cli's
// allowedBaseScheme enforce, mirrored here because this seam cannot import
// either without depending on the CLI/config layer. The duplication is
// noted in docs/DECISIONS.md as acceptable pending a shared helper.
func allowedEndpointScheme(u *url.URL) bool {
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}

// refuseCrossHostRedirects is the client's redirect policy: same-host
// redirects are followed (capped at three hops), cross-host redirects are
// refused outright — the Authorization header travels on every request,
// and following one would hand the key to the redirect target (CWE-601).
// This reimplements internal/interactions.refuseCrossHostRedirects, which
// is unexported; the duplication is noted in docs/DECISIONS.md.
func refuseCrossHostRedirects(req *http.Request, via []*http.Request) error {
	if req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("model: redirect to %s refused: cross-origin redirect with sensitive headers", req.URL.Host)
	}
	if len(via) >= 3 {
		return errors.New("model: too many redirects")
	}
	return nil
}
