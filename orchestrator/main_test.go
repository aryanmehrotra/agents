package main

import "testing"

// TestKeywordRoute verifies the deterministic fallback the orchestrator uses when the
// LLM router is unavailable or answers unexpectedly, so routing stays correct regardless.
func TestKeywordRoute(t *testing.T) {
	cases := map[string]string{
		"please review this git diff for bugs":                                          "review",
		"look at this patch and suggest changes":                                        "review",
		"open a pull request for review":                                                "review",
		"inspect this changeset":                                                        "review",
		"how do I reset my VPN password":                                                "kb",
		"what is the leave policy":                                                      "kb",
		"how to configure the vpn":                                                      "kb",
		"the app crashes with a panic on startup":                                       "support",
		"login returns a 500 error, service outage":                                     "support",
		"there is a bug and it is not working":                                          "support",
		"what is our total shipped revenue":                                             "data",
		"list all products in inventory":                                                "data",
		"how many orders are pending":                                                   "data",
		"run a sql query against the sales database":                                    "sql",
		"which sales rep has the biggest pipeline":                                      "sql",
		"compare https://example.com/a and https://example.com/b and cite your sources": "research",
		"summarize http://news.example.com/article for me":                              "research",
		"extract the invoice number and total from this text":                           "extract",
		"parse this resume into structured fields":                                      "extract",
		"what do my notes say about the onboarding process":                             "localdocs",
		"search my documents for the refund policy":                                     "localdocs",
		"remind me in 10 minutes to check the deploy":                                   "schedule",
		"schedule a webhook to fire tomorrow at 9am":                                    "schedule",
		"set a reminder for the standup":                                                "schedule",
		"write a spec for the checkout flow":                                            "spec",
		"turn this ticket into a spec with acceptance criteria":                         "spec",
		"how many story points is the login work":                                       "estimate",
		"give me a rough estimate to build oauth":                                       "estimate",
		"scaffold a new orders service with a /orders endpoint":                         "scaffold",
		"generate a module skeleton with boilerplate":                                   "scaffold",
		// Regression: an effort word embedded in a data/sql question must NOT hijack the
		// estimate route — the greedy bare "estimate"/"how long will" keywords are gone.
		"what is our estimated revenue this quarter": "data",
		"how long will the summer sale run":          "data",
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
