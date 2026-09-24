package fleet

import (
	"strings"
	"testing"
)

func TestHTMLToText(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", "hello"},
		{"tags stripped", "<p>one</p><p>two</p>", "\none\ntwo"},
		{"script dropped", "a<script>alert(1)</script>b", "a\nb"},
		{"style dropped", "a<style>p{}</style>b", "a\nb"},
		{"case-insensitive skip", "a<SCRIPT type=\"x\">x</SCRIPT>b", "a\nb"},
		{"comment dropped", "a<!-- c -->b", "ab"},
		{"entities decoded", "R&amp;D &lt;3", "R&D <3"},
		{"quoted gt in attribute", `<a href="x" title="a>b">link</a>`, "link"},
		{"table cells tabbed", "<tr><td>a</td><td>b</td></tr>", "\n\ta\tb"},
		{"self-closing br", "a<br/>b", "a\nb"},
		{"unterminated tag", "a<div", "a"},
		{"unterminated script", "a<script>never closed", "a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := htmlToText(tt.in); got != tt.want {
				t.Errorf("htmlToText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPageText(t *testing.T) {
	for _, tt := range []struct {
		name        string
		contentType string
		content     string
		maxBytes    int
		wantOK      bool
		wantText    string
		wantTrunc   bool
	}{
		{"html", "text/html; charset=utf-8", "<p>a</p>\n\n\n<p>b</p>", 1024, true, "a\n\nb", false},
		{"plain", "text/plain", "  x   y \n\n\n\nz", 1024, true, "x y\n\nz", false},
		{"json", "application/json", `{"a":1}`, 1024, true, `{"a":1}`, false},
		{"ld+json suffix", "application/ld+json", `{}`, 1024, true, `{}`, false},
		{"sniffed html", "", "<html><body>hi</body></html>", 1024, true, "hi", false},
		{"image refused", "image/png", "\x89PNG", 1024, false, "", false},
		{"pdf refused", "application/pdf", "%PDF-1.4", 1024, false, "", false},
		{"octet refused", "application/octet-stream", "bytes", 1024, false, "", false},
		{"truncated on rune boundary", "text/plain", "ab£cd", 3, true, "ab", true},
		{"invalid utf8 replaced", "text/plain", "a\xffb", 1024, true, "a�b", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pageText(fetchedPage{ContentType: tt.contentType, Content: []byte(tt.content)}, tt.maxBytes)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.text != tt.wantText {
				t.Errorf("text = %q, want %q", got.text, tt.wantText)
			}
			if got.truncated != tt.wantTrunc {
				t.Errorf("truncated = %v, want %v", got.truncated, tt.wantTrunc)
			}
		})
	}
}

func TestBoundDetail(t *testing.T) {
	long := strings.Repeat("é", maxDetailBytes)
	got := boundDetail(long)
	if len(got) > maxDetailBytes+len(" [truncated]") {
		t.Errorf("bounded detail is %d bytes", len(got))
	}
	if !strings.HasSuffix(got, " [truncated]") {
		t.Errorf("bounded detail lacks the truncation marker: %q", got[len(got)-20:])
	}
	if strings.ContainsRune(got, '�') {
		t.Error("truncation split a rune")
	}
	if boundDetail("short") != "short" {
		t.Error("short detail was altered")
	}
}
