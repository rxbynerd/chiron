package httpx_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rxbynerd/chiron/internal/httpx"
)

func TestParseEndpoint(t *testing.T) {
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
		{"https on an IPv6 literal", "https://[2001:db8::1]/v1", ""},
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
		{"http unspecified IPv4", "http://0.0.0.0:8080", absolute},
		{"http unspecified IPv6", "http://[::]:8080", absolute},
		{"http loopback lookalike name", "http://127.0.0.1.nip.io", absolute},
		{"http localhost suffix", "http://localhost.evil.com", absolute},
		{"http upper-case localhost", "http://LOCALHOST:8080", absolute},
		{"http IPv4-mapped non-loopback", "http://[::ffff:10.0.0.1]", absolute},
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
		{"unterminated IPv6 literal", "http://[::1", absolute},
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
			u, err := httpx.ParseEndpoint(tt.raw)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseEndpoint(%q) = %v, want success", tt.raw, err)
				}
				if u == nil || u.Hostname() == "" {
					t.Fatalf("ParseEndpoint(%q) returned %v, want a parsed URL with a host", tt.raw, u)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseEndpoint(%q) succeeded, want an error containing %q", tt.raw, tt.wantErr)
			}
			if u != nil {
				t.Errorf("ParseEndpoint(%q) returned a URL alongside its error", tt.raw)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ParseEndpoint(%q) = %v, want an error containing %q", tt.raw, err, tt.wantErr)
			}
			if !errors.Is(err, httpx.ErrInvalidEndpoint) {
				t.Errorf("ParseEndpoint(%q) = %v, want it to match ErrInvalidEndpoint", tt.raw, err)
			}
		})
	}
}

func TestParseEndpointReturnsParsedURL(t *testing.T) {
	u, err := httpx.ParseEndpoint("https://api.example.com:8443/v1/")
	if err != nil {
		t.Fatalf("ParseEndpoint: %v", err)
	}
	if u.Scheme != "https" || u.Host != "api.example.com:8443" || u.Path != "/v1/" {
		t.Errorf("ParseEndpoint = scheme %q host %q path %q", u.Scheme, u.Host, u.Path)
	}
}

func TestParseEndpointErrorsNeverEchoRawValue(t *testing.T) {
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
			_, err := httpx.ParseEndpoint(tt.raw)
			if err == nil {
				t.Fatalf("ParseEndpoint(%q) succeeded, want an error", tt.raw)
			}
			if msg := err.Error(); strings.Contains(msg, secret) || strings.Contains(msg, tt.raw) {
				t.Errorf("error echoed the raw value: %v", err)
			}
		})
	}
}

func TestLoopbackHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"127.0.0.1", true},
		{"127.1.2.3", true},
		{"127.255.255.255", true},
		{"::1", true},
		{"0:0:0:0:0:0:0:1", true},
		{"::ffff:127.0.0.1", true},

		{"", false},
		{"LOCALHOST", false},
		{"Localhost", false},
		{"localhost.", false},
		{"localhost.evil.com", false},
		{"evil-localhost", false},
		{"127.0.0.1.nip.io", false},
		{"[::1]", false},
		{"127.0.0.1:8080", false},
		{"::1%lo0", false},
		{"0.0.0.0", false},
		{"::", false},
		{"::ffff:10.0.0.1", false},
		{"126.255.255.255", false},
		{"128.0.0.1", false},
		{"10.0.0.1", false},
		{"169.254.169.254", false},
		{"127.1", false},
		{"0x7f000001", false},
		{"2130706433", false},
		{"127.000.000.001", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := httpx.LoopbackHost(tt.host); got != tt.want {
				t.Errorf("LoopbackHost(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}
