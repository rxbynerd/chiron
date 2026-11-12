package httpx_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/httpx"
)

func TestParseEndpoint_AcceptsAndRejects(t *testing.T) {
	const (
		absolute = "must be an absolute https:// URL"
		userinfo = "userinfo"
		queryish = "query or fragment"
	)
	tests := []struct {
		name    string
		raw     string
		wantErr string // "" means accepted
	}{
		{"https host", "https://api.example.com", ""},
		{"https host with path", "https://api.example.com/v1", ""},
		{"https host with trailing slash", "https://api.example.com/v1/", ""},
		{"https host with port", "https://api.example.com:8443/v1", ""},
		{"https on a private address", "https://10.0.0.1/v1", ""},
		{"https on an ipv6 literal", "https://[2001:db8::1]/v1", ""},
		{"encoded question mark in path", "https://api.example.com/a%3Fb", ""},
		{"http on 127.0.0.1", "http://127.0.0.1:8080/v1", ""},
		{"http elsewhere in 127.0.0.0/8", "http://127.1.2.3/v1", ""},
		{"http on localhost", "http://localhost:8080", ""},
		{"http on bracketed ::1", "http://[::1]:8080/v1", ""},
		{"upper-case scheme", "HTTPS://api.example.com", ""},

		{"empty", "", "must not be empty"},

		{"user and password", "https://user:pw@api.example.com", userinfo},
		{"user only", "https://user@api.example.com", userinfo},
		{"password only", "https://:pw@api.example.com", userinfo},
		{"empty userinfo", "https://@api.example.com", userinfo},
		{"userinfo on loopback", "http://user:pw@127.0.0.1:8080", userinfo},
		{"userinfo under a mistyped scheme", "htps://user:pw@api.example.com", userinfo},

		{"http non-loopback", "http://api.example.com/v1", absolute},
		{"http metadata service", "http://169.254.169.254", absolute},
		{"http private address", "http://10.0.0.1", absolute},
		{"http unspecified ipv4", "http://0.0.0.0:8080", absolute},
		{"http unspecified ipv6", "http://[::]:8080", absolute},
		{"http loopback lookalike name", "http://127.0.0.1.nip.io", absolute},
		{"http localhost suffix", "http://localhost.evil.com", absolute},
		{"http upper-case localhost", "http://LOCALHOST:8080", absolute},
		{"http ipv4-mapped non-loopback", "http://[::ffff:10.0.0.1]", absolute},
		{"ftp scheme", "ftp://api.example.com", absolute},
		{"file scheme", "file:///etc/passwd", absolute},
		{"ws scheme on loopback", "ws://localhost:8080", absolute},
		{"relative path", "/v1/chat", absolute},
		{"bare host", "api.example.com", absolute},
		{"scheme-relative", "//api.example.com/v1", absolute},
		{"opaque https", "https:api.example.com", absolute},
		{"no host", "https://", absolute},
		{"empty host with path", "https:///v1", absolute},
		{"port without hostname", "https://:443/v1", absolute},
		{"unparseable", "://not a url", absolute},
		{"invalid port", "https://api.example.com:port/", absolute},
		{"unterminated ipv6 literal", "http://[::1", absolute},
		{"control character", "https://api.example.com/\n", absolute},

		{"query", "https://api.example.com/v1?api_key=x", queryish},
		{"empty query", "https://api.example.com/v1?", queryish},
		{"fragment", "https://api.example.com/v1#frag", queryish},
		{"empty fragment", "https://api.example.com/v1#", queryish},
		{"query on loopback", "http://127.0.0.1:8080/?x=1", queryish},
		{"query then empty fragment", "https://api.example.com/?#", queryish},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			u, err := httpx.ParseEndpoint(tt.raw)
			if tt.wantErr == "" {
				must.NoError(err, "ParseEndpoint(%q)", tt.raw)
				must.NotNil(u, "ParseEndpoint(%q)", tt.raw)
				must.NotEmpty(u.Hostname(), "ParseEndpoint(%q)", tt.raw)
				return
			}
			must.Error(err, "ParseEndpoint(%q)", tt.raw)
			must.Nil(u, "ParseEndpoint(%q) returned a URL alongside its error", tt.raw)
			must.ErrorContains(err, tt.wantErr)
			must.ErrorIs(err, httpx.ErrInvalidEndpoint)
		})
	}
}

func TestParseEndpoint_ReturnsParsedURL(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	u, err := httpx.ParseEndpoint("https://api.example.com:8443/v1/")
	must.NoError(err)

	check.Equal("https", u.Scheme)
	check.Equal("api.example.com:8443", u.Host)
	check.Equal("/v1/", u.Path)
}

func TestParseEndpoint_ErrorsNeverEchoRawValue(t *testing.T) {
	const secret = "sk-Zq8TopSecretCredential"
	tests := []struct {
		name string
		raw  string
	}{
		{"password in userinfo", "https://user:" + secret + "@api.example.com/v1"},
		{"key as username", "https://" + secret + "@api.example.com/v1"},
		{"key in query", "https://api.example.com/v1?api_key=" + secret},
		{"key in fragment", "https://api.example.com/v1#" + secret},
		{"key in path of a cleartext endpoint", "http://api.example.com/" + secret},
		{"key in path of a non-http scheme", "ftp://api.example.com/" + secret},
		{"key as port", "https://api.example.com:" + secret + "/v1"},
		{"single slash hides userinfo", "https:/user:" + secret + "@gateway.corp/v1"},
		{"missing scheme hides userinfo", "://user:" + secret + "@gateway.corp/v1"},
		{"mistyped scheme", "htps://user:" + secret + "@gateway.corp/v1"},
		{"key parsed as host before an at sign in a fragment", "https://" + secret + "#@api.example.com"},
		{"key parsed as host before an at sign in a query", "https://" + secret + "?@api.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			_, err := httpx.ParseEndpoint(tt.raw)
			must.Error(err)

			msg := err.Error()
			must.NotContains(msg, secret)
			must.NotContains(msg, tt.raw)
		})
	}
}

func TestLoopbackHost_ClassifiesHosts(t *testing.T) {
	tests := []struct {
		name string
		host string
		want bool
	}{
		{"localhost", "localhost", true},
		{"ipv4 loopback", "127.0.0.1", true},
		{"ipv4 loopback range", "127.1.2.3", true},
		{"ipv4 loopback range end", "127.255.255.255", true},
		{"ipv6 loopback", "::1", true},
		{"ipv6 loopback expanded", "0:0:0:0:0:0:0:1", true},
		{"ipv4-mapped ipv6 loopback", "::ffff:127.0.0.1", true},

		{"empty host", "", false},
		{"upper-case localhost", "LOCALHOST", false},
		{"mixed-case localhost", "Localhost", false},
		{"localhost with trailing dot", "localhost.", false},
		{"localhost subdomain", "localhost.evil.com", false},
		{"localhost as name suffix", "evil-localhost", false},
		{"loopback address as subdomain", "127.0.0.1.nip.io", false},
		{"bracketed ipv6 loopback", "[::1]", false},
		{"loopback with port", "127.0.0.1:8080", false},
		{"ipv6 loopback with zone", "::1%lo0", false},
		{"ipv4 unspecified", "0.0.0.0", false},
		{"ipv6 unspecified", "::", false},
		{"ipv4-mapped private address", "::ffff:10.0.0.1", false},
		{"address below loopback range", "126.255.255.255", false},
		{"address above loopback range", "128.0.0.1", false},
		{"private ipv4 address", "10.0.0.1", false},
		{"metadata service address", "169.254.169.254", false},
		{"short ipv4 loopback form", "127.1", false},
		{"hex ipv4 loopback", "0x7f000001", false},
		{"decimal ipv4 loopback", "2130706433", false},
		{"zero-padded ipv4 loopback", "127.000.000.001", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got := httpx.LoopbackHost(tt.host)
			must.Equal(tt.want, got, "LoopbackHost(%q)", tt.host)
		})
	}
}
