// orchestrator — the multi-agent front door. It receives a user query and routes it to the right
// specialist, then calls that agent over a RESILIENT GoFr HTTP service (circuit breaker + retry +
// rate limiter + health check, all from config). The front door itself is API-key protected.
//
// Routing is driven by a single CAPABILITY REGISTRY (see registry below) — the one source of truth
// that declares each agent's route, service, request shape, and a description of what it's for. That
// registry drives three things with no per-agent code: the resilient-service registration, an
// LLM-FIRST router (the classifier prompt is generated from the registry, so the model picks over
// live descriptions rather than a hand-maintained prompt), and a /capabilities discovery endpoint.
// A cheap keyword match — also derived from the registry — is only a fallback for when the model call
// fails. Adding an agent is one registry entry; there are no keyword chains or prompt prose to edit.
//
// Because both the orchestrator and the specialist export traces, one /assistant call becomes a
// single distributed trace spanning two services.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/service"
)

// specialist is one entry in the capability registry: everything the orchestrator needs to register,
// route to, describe, and call an agent. This is the ONLY place an agent is declared.
type specialist struct {
	Route      string   // the route word the classifier emits, e.g. "data"
	Service    string   // the GoFr HTTP service name, e.g. "data-agent"
	DefaultURL string   // default address (overridable via <SERVICE>_URL env)
	Path       string   // the agent's endpoint path, e.g. "ask"
	BodyFields []string // JSON keys to fill with the query (most agents take one; a few take two)
	Desc       string   // what the agent does — shown to the routing model AND at /capabilities
	Keywords   []string // offline fallback only (used when the routing model is unavailable)
}

// defaultRoute is the last resort when neither the model nor a keyword matches.
const defaultRoute = "data"

// registry is the single source of truth. Order matters only for the keyword fallback (more specific
// entries first; the default-route agent needs no keywords). Add an agent here and it is registered,
// routable, and discoverable — no other code changes.
var registry = []specialist{
	{"research", "research-agent", "http://localhost:8008", "research", []string{"question"},
		"a question that includes one or more http(s) links to fetch, read and answer with citations",
		[]string{"http://", "https://"}},
	{"review", "code-review-agent", "http://localhost:8003", "review", []string{"title", "diff"},
		"review a code diff / patch / pull request and comment on it",
		[]string{"diff", "patch", "pull request", "code review", "changeset"}},
	{"redact", "pii-redaction-agent", "http://localhost:8004", "redact", []string{"text"},
		"detect and redact PII (names, emails, SSNs, cards) from a block of text",
		[]string{"redact", "pii", "ssn", "credit card", "personally identifiable"}},
	{"summarize", "summarizer-agent", "http://localhost:8005", "summarize", []string{"text"},
		"summarize a long document, email thread or chat transcript",
		[]string{"summarize", "summary", "tl;dr", "tldr", "thread", "long document"}},
	{"extract", "extraction-agent", "http://localhost:8009", "extract", []string{"text"},
		"turn unstructured text into structured, typed JSON fields (invoice, resume, contract, entities)",
		[]string{"extract", "parse", "structured", "into json", "into fields"}},
	{"migrate", "migration-agent", "http://localhost:8016", "migrate", []string{"instruction"},
		"apply a mechanical code change (codemod) across existing files — rename, replace a deprecated API",
		[]string{"codemod", "apply a codemod", "rename across", "replace deprecated", "bulk edit", "across the codebase", "in every file", "migrate the code"}},
	{"scaffold", "scaffold-agent", "http://localhost:8015", "scaffold", []string{"spec"},
		"generate a new project/service code skeleton in any language from a description",
		[]string{"scaffold", "skeleton", "boilerplate", "starter code", "stub out", "bootstrap a service", "new service skeleton", "generate a service", "generate a module"}},
	{"spec", "spec-agent", "http://localhost:8013", "spec", []string{"ticket"},
		"turn a ticket / feature request / user story into a structured engineering spec",
		[]string{"write a spec", "a spec for", "into a spec", "acceptance criteria", "task breakdown", "break down the work", "user story", "scope this"}},
	{"estimate", "estimation-agent", "http://localhost:8014", "estimate", []string{"text"},
		"size a piece of work — story points, effort, or how long it will take",
		[]string{"story points", "story-points", "how many points", "t-shirt size", "size this work", "effort estimate", "estimate the effort", "estimate how long", "give an estimate", "rough estimate", "how long to build", "how long will it take to build"}},
	{"localdocs", "local-rag-agent", "http://localhost:8010", "ask", []string{"question"},
		"answer from the user's own privately-ingested local documents / notes (on-device RAG)",
		[]string{"my documents", "my notes", "my handbook", "ingested", "local rag", "private docs"}},
	{"schedule", "scheduler-agent", "http://localhost:8011", "schedule", []string{"request"},
		"remind, notify, or trigger a webhook at a future time or after a delay",
		[]string{"remind", "reminder", "schedule", "webhook", "cron job", "fire a task", "notify me later"}},
	{"sql", "sql-agent", "http://localhost:8007", "query", []string{"question"},
		"answer a question about structured data by generating and running SQL against a database",
		[]string{"sql", "database", "query", "deals", "headcount", "sales rep", "pipeline"}},
	{"kb", "kb-agent", "http://localhost:8002", "ask", []string{"question"},
		"IT/HR policy and how-to questions grounded in internal docs (leave, VPN, passwords)",
		[]string{"vpn", "password", "leave", "policy", "reset", "how do i", "how to"}},
	{"support", "support-agent", "http://localhost:8001", "triage", []string{"title", "body"},
		"triage a bug report / error / outage / support ticket and draft a reply",
		[]string{"crash", "error", "bug", "outage", "panic", "ticket", "not working"}},
	{"data", "data-agent", "http://localhost:8000", "ask", []string{"question"},
		"products, orders, inventory, revenue and stats from the user's own service (the default)",
		nil},
}

func main() {
	app := gofr.New()

	// Register every agent in the registry as a resilient HTTP service. These options are GoFr
	// features working on the agent-to-agent calls — no extra code in the handler. No custom
	// HealthConfig: GoFr's circuit breaker already probes the liveness path it serves
	// (/.well-known/alive) by default.
	for _, s := range registry {
		app.AddHTTPService(s.Service, envOr(envKey(s.Service), s.DefaultURL),
			&service.CircuitBreakerConfig{Threshold: 4, Interval: 2 * time.Second},
			&service.RateLimiterConfig{Requests: 20, Window: time.Second, Burst: 25},
		)
	}

	// Front-door protection: callers must send  X-Api-Key: agents-demo-key
	app.EnableAPIKeyAuth(envOr("API_KEY", "agents-demo-key"))

	app.POST("/assistant", assistant)
	app.GET("/capabilities", capabilities) // discovery: what agents exist and what they're for

	app.Run()
}

// capabilities is the discovery endpoint — it returns the registry (public view), so a caller or a
// peer can see what agents exist and what each is for without any hardcoded knowledge.
func capabilities(_ *gofr.Context) (any, error) {
	out := make([]map[string]any, 0, len(registry))

	for _, s := range registry {
		out = append(out, map[string]any{
			"route":       s.Route,
			"agent":       s.Service,
			"description": s.Desc,
		})
	}

	return map[string]any{"agents": out, "count": len(out)}, nil
}

func assistant(c *gofr.Context) (any, error) {
	var in struct {
		Query string `json:"query"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	spec := route(c, in.Query)

	// Call the chosen specialist over the resilient HTTP service. The trace context propagates
	// automatically, so this call is a child span in the same trace.
	resp, err := c.GetHTTPService(spec.Service).Post(c, spec.Path, nil, body(spec, in.Query))
	if err != nil {
		// Degrade gracefully: when a specialist is down or its breaker is open, return a clear message
		// naming it instead of leaking a transport error to the caller as a raw 500.
		c.Logger.Errorf("orchestrator: %s unavailable for route %q: %v", spec.Service, spec.Route, err)

		return map[string]any{
			"route":        spec.Route,
			"routed_to":    spec.Service,
			"degraded":     true,
			"circuit_open": errors.Is(err, service.ErrCircuitOpen),
			"error":        fmt.Sprintf("the %s is temporarily unavailable — please retry shortly", spec.Service),
		}, nil
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	var payload any
	_ = json.Unmarshal(raw, &payload)

	return map[string]any{
		"route":     spec.Route,
		"routed_to": spec.Service,
		"response":  payload,
	}, nil
}

// body shapes the request for a specialist by filling each of its declared BodyFields with the query.
func body(s specialist, query string) []byte {
	m := make(map[string]string, len(s.BodyFields))
	for _, f := range s.BodyFields {
		m[f] = query
	}

	b, _ := json.Marshal(m)

	return b
}

// route picks the specialist for a query, LLM-FIRST: the model chooses over the registry's live
// descriptions. Only if the model call fails (or answers with no known route) do we fall back to the
// registry-derived keyword match — so a flaky model degrades to a heuristic instead of a 500, but the
// model, not a hand-tuned keyword list, is the primary router.
func route(c *gofr.Context, query string) specialist {
	resp, err := c.LLM().Generate(c, routerPrompt(query), ai.WithTemperature(0))
	if err == nil {
		if s, ok := matchRoute(resp.Content); ok {
			return s
		}
	} else {
		c.Logger.Errorf("orchestrator: router model unavailable, using keyword fallback: %v", err)
	}

	return keywordFallback(query)
}

// routerPrompt builds the classifier prompt dynamically from the registry, so the model always sees
// the current set of agents and their descriptions — nothing to keep in sync by hand.
func routerPrompt(query string) string {
	var b strings.Builder

	b.WriteString("You are a router for a multi-agent system. Choose the ONE agent best suited to the " +
		"request. Reply with ONLY its route word.\n\nAgents:\n")

	for _, s := range registry {
		fmt.Fprintf(&b, "- %s: %s\n", s.Route, s.Desc)
	}

	b.WriteString("\nRequest: ")
	b.WriteString(query)

	return b.String()
}

// matchRoute maps the model's reply to a registry entry: an exact route word if present, else the
// entry whose route word appears in the reply. Returns false when the model named nothing we know.
func matchRoute(reply string) (specialist, bool) {
	w := strings.ToLower(strings.TrimSpace(reply))

	for _, s := range registry { // exact match first
		if w == s.Route {
			return s, true
		}
	}

	for _, s := range registry { // else a contained route word
		if strings.Contains(w, s.Route) {
			return s, true
		}
	}

	return specialist{}, false
}

// keywordFallback is the deterministic fallback used only when the routing model is unavailable: the
// first registry entry whose keywords appear in the query, else the default-route agent.
func keywordFallback(query string) specialist {
	q := strings.ToLower(query)

	for _, s := range registry {
		if containsAny(q, s.Keywords...) {
			return s
		}
	}

	return byRoute(defaultRoute)
}

// byRoute looks up a specialist by its route word (the registry is small, a linear scan is fine).
func byRoute(r string) specialist {
	for _, s := range registry {
		if s.Route == r {
			return s
		}
	}

	return registry[len(registry)-1] // registry is never empty; last entry is the default agent
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}

	return false
}

// envKey derives the URL override env var for a service, e.g. "data-agent" → "DATA_AGENT_URL".
func envKey(service string) string {
	return strings.ToUpper(strings.ReplaceAll(service, "-", "_")) + "_URL"
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}
