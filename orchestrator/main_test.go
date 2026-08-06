package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRegistryWellFormed guards the single source of truth: unique routes, and every entry has the
// fields needed to register, route to, and call the agent.
func TestRegistryWellFormed(t *testing.T) {
	seen := map[string]bool{}

	for _, s := range registry {
		if s.Route == "" || s.Service == "" || s.Path == "" || s.DefaultURL == "" || len(s.BodyFields) == 0 || s.Desc == "" {
			t.Errorf("registry entry incomplete: %+v", s)
		}

		if seen[s.Route] {
			t.Errorf("duplicate route %q in registry", s.Route)
		}

		seen[s.Route] = true
	}

	if !seen[defaultRoute] {
		t.Errorf("defaultRoute %q is not in the registry", defaultRoute)
	}
}

// TestRouterPromptDynamic proves the classifier prompt is generated from the registry — every agent's
// route word appears, so adding an agent needs no prompt edit.
func TestRouterPromptDynamic(t *testing.T) {
	p := routerPrompt("do a thing")

	for _, s := range registry {
		if !strings.Contains(p, s.Route+": ") {
			t.Errorf("router prompt is missing route %q — not generated from the registry", s.Route)
		}
	}

	if !strings.Contains(p, "do a thing") {
		t.Error("router prompt should include the query")
	}
}

// TestMatchRoute maps a model reply to a registry entry (exact word, or contained), and rejects an
// unknown answer so routing can fall back.
func TestMatchRoute(t *testing.T) {
	if s, ok := matchRoute("summarize"); !ok || s.Route != "summarize" {
		t.Errorf("matchRoute(exact) = %v, %v", s, ok)
	}

	if s, ok := matchRoute("The best fit is `research`."); !ok || s.Route != "research" {
		t.Errorf("matchRoute(contained) = %v, %v", s, ok)
	}

	if _, ok := matchRoute("i have no idea"); ok {
		t.Error("matchRoute(unknown) should return ok=false")
	}
}

// TestKeywordFallback is the deterministic fallback used only when the routing model is down — driven
// entirely by the registry's per-agent keywords, with the default-route agent as the last resort.
func TestKeywordFallback(t *testing.T) {
	cases := map[string]string{
		"please review this git diff":                           "review",
		"check for breaking api changes before merge":           "breaking",
		"summarize this long thread":                            "summarize",
		"compare https://a.com and https://b.com":               "research",
		"remind me in 10 minutes to deploy":                     "schedule",
		"apply a codemod to rename getUser across the codebase": "migrate",
		"scaffold a new service skeleton":                       "scaffold",
		"write unit tests for this function":                    "testgen",
		"find the flaky tests in our ci history":                "flaky",
		"how many story points is this":                         "estimate",
		"write a spec for the checkout flow":                    "spec",
		"run a sql query on the sales database":                 "sql",
		"how do I reset my vpn password":                        "kb",
		"the app crashes with a panic":                          "support",
		// No keyword → the default-route agent.
		"what is our total shipped revenue": "data",
		// Regression: an effort word embedded in a data question must NOT hijack estimate.
		"what is our estimated revenue this quarter": "data",
	}

	for query, want := range cases {
		if got := keywordFallback(query).Route; got != want {
			t.Errorf("keywordFallback(%q) = %q, want %q", query, got, want)
		}
	}
}

// TestBody fills each declared body field with the query — one field for most agents, two for the few
// that need them.
func TestBody(t *testing.T) {
	sql := byRoute("sql")

	var m map[string]string
	_ = json.Unmarshal(body(sql, "top reps?"), &m)

	if m["question"] != "top reps?" || len(m) != 1 {
		t.Errorf("body(sql) = %v, want {question}", m)
	}

	support := byRoute("support")
	_ = json.Unmarshal(body(support, "it broke"), &m)

	if m["title"] != "it broke" || m["body"] != "it broke" {
		t.Errorf("body(support) = %v, want {title, body}", m)
	}
}

// TestEnvKey derives the URL override variable from the service name, preserving the exact names the
// deploy configs already use (backward compatibility for every agent).
func TestEnvKey(t *testing.T) {
	want := map[string]string{
		"data-agent":          "DATA_AGENT_URL",
		"code-review-agent":   "CODE_REVIEW_AGENT_URL",
		"pii-redaction-agent": "PII_REDACTION_AGENT_URL",
		"local-rag-agent":     "LOCAL_RAG_AGENT_URL",
	}
	for service, key := range want {
		if got := envKey(service); got != key {
			t.Errorf("envKey(%q) = %q, want %q", service, got, key)
		}
	}
}

// TestByRouteUnknownFallsToDefault confirms an unknown route resolves to the declared default agent,
// independent of the registry's ordering.
func TestByRouteUnknownFallsToDefault(t *testing.T) {
	if got := byRoute("no-such-route").Route; got != defaultRoute {
		t.Errorf("byRoute(unknown) = %q, want the default %q", got, defaultRoute)
	}
}

// TestCapabilities returns every registered agent for discovery.
func TestCapabilities(t *testing.T) {
	res, err := capabilities(nil)
	if err != nil {
		t.Fatalf("capabilities() error = %v", err)
	}

	m := res.(map[string]any)
	if m["count"].(int) != len(registry) {
		t.Errorf("capabilities count = %v, want %d", m["count"], len(registry))
	}

	if len(m["agents"].([]map[string]any)) != len(registry) {
		t.Error("capabilities should list every registry entry")
	}
}

func TestContainsAny(t *testing.T) {
	if !containsAny("hello world", "foo", "world") {
		t.Error("containsAny should match a present substring")
	}

	if containsAny("hello", "foo", "bar") {
		t.Error("containsAny should not match absent substrings")
	}
}
