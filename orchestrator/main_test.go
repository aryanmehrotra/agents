package main

import "testing"

// TestKeywordRoute verifies the deterministic fallback the orchestrator uses when the
// LLM router is unavailable or answers unexpectedly, so routing stays correct regardless.
func TestKeywordRoute(t *testing.T) {
	cases := map[string]string{
		"please review this git diff for bugs":       "review",
		"look at this patch and suggest changes":     "review",
		"open a pull request for review":             "review",
		"inspect this changeset":                     "review",
		"how do I reset my VPN password":             "kb",
		"what is the leave policy":                   "kb",
		"how to configure the vpn":                   "kb",
		"the app crashes with a panic on startup":    "support",
		"login returns a 500 error, service outage":  "support",
		"there is a bug and it is not working":       "support",
		"what is our total shipped revenue":          "data",
		"list all products in inventory":             "data",
		"how many orders are pending":                "data",
		"run a sql query against the sales database": "sql",
		"which sales rep has the biggest pipeline":   "sql",
		"compare https://example.com/a and https://example.com/b and cite your sources": "research",
		"summarize http://news.example.com/article for me":                              "research",
	}

	for query, want := range cases {
		if got := keywordRoute(query); got != want {
			t.Errorf("keywordRoute(%q) = %q, want %q", query, got, want)
		}
	}
}

func TestContainsAny(t *testing.T) {
	if !containsAny("hello world", "foo", "world") {
		t.Error("containsAny should match a present substring")
	}

	if containsAny("hello world", "foo", "bar") {
		t.Error("containsAny should not match absent substrings")
	}
}
