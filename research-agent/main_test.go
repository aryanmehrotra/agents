package main

import (
	"strings"
	"testing"
)

// TestIsSafeURL is the guardrail test: every one of these must be refused BEFORE any outbound
// request is made, regardless of what the model or a prompt-injected question asked for.
func TestIsSafeURL(t *testing.T) {
	cases := []struct {
		url  string
		safe bool
	}{
		{"https://en.wikipedia.org/wiki/Retrieval-augmented_generation", true},
		{"http://example.com/docs", true},
		{"ftp://example.com/file", false},
		{"file:///etc/passwd", false},
		{"javascript:alert(1)", false},
		{"http://localhost:8080/admin", false},
		{"http://127.0.0.1/", false},
		{"http://169.254.169.254/latest/meta-data/iam/security-credentials/", false}, // cloud metadata SSRF
		{"http://10.0.0.5/internal", false},
		{"http://192.168.1.1/", false},
		{"http://metadata.google.internal/computeMetadata/v1/", false},
		{"http://user:pass@evil.example.com/", false},
		{"http://[::1", false}, // unparseable
	}

	for _, tc := range cases {
		ok, reason := isSafeURL(tc.url)
		if ok != tc.safe {
			t.Errorf("isSafeURL(%q) = (%v, %q), want safe=%v", tc.url, ok, reason, tc.safe)
		}
	}
}

// TestExtractURLs covers the pure logic that decides which sources research() will even attempt to
// fetch: dedupe and trailing sentence punctuation must not leak into the URL.
func TestExtractURLs(t *testing.T) {
	text := "Compare https://a.example.com/page and https://b.example.com/other, also see " +
		"https://a.example.com/page again (duplicate) and (https://c.example.com)."

	got := extractURLs(text)
	want := []string{
		"https://a.example.com/page",
		"https://b.example.com/other",
		"https://c.example.com",
	}

	if len(got) != len(want) {
		t.Fatalf("extractURLs() = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("extractURLs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExtractURLsNone(t *testing.T) {
	if got := extractURLs("no links in this question at all"); len(got) != 0 {
		t.Errorf("extractURLs() = %v, want empty", got)
	}
}

// TestStripHTML is the other required pure-logic test: scripts/styles and tags must be gone, and
// entities decoded, while the actual text content survives.
func TestStripHTML(t *testing.T) {
	input := `<html><head><style>body{color:red}</style></head><body>` +
		`<script>alert('evil')</script><p>Hello&nbsp;<b>World</b>.</p></body></html>`

	got := stripHTML(input)

	if strings.Contains(got, "<") || strings.Contains(got, ">") {
		t.Errorf("stripHTML() left markup in output: %q", got)
	}

	if strings.Contains(got, "evil") || strings.Contains(got, "color:red") {
		t.Errorf("stripHTML() leaked script/style content: %q", got)
	}

	if !strings.Contains(got, "Hello") || !strings.Contains(got, "World") {
		t.Errorf("stripHTML() dropped real text: %q", got)
	}
}
