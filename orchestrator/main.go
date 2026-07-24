// orchestrator — the multi-agent front door. It receives a user query, uses an LLM
// to route it to the right specialist agent (data / support / kb / review / redact), and calls
// that agent over a RESILIENT GoFr HTTP service: circuit breaker + retry + rate limiter +
// health check, all from config. The front door itself is protected with API-key auth.
//
// Because both the orchestrator and the specialist export traces, one /assistant call
// becomes a single distributed trace spanning two services.
package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/service"
)

func main() {
	app := gofr.New()

	// Register each specialist as a resilient HTTP service. These options are GoFr
	// features working on the agent-to-agent calls — no extra code in the handler.
	for name, addr := range map[string]string{
		"data-agent":          envOr("DATA_AGENT_URL", "http://localhost:8000"),
		"support-agent":       envOr("SUPPORT_AGENT_URL", "http://localhost:8001"),
		"kb-agent":            envOr("KB_AGENT_URL", "http://localhost:8002"),
		"code-review-agent":   envOr("CODE_REVIEW_AGENT_URL", "http://localhost:8003"),
		"pii-redaction-agent": envOr("PII_REDACTION_AGENT_URL", "http://localhost:8004"),
		"summarizer-agent":    envOr("SUMMARIZER_AGENT_URL", "http://localhost:8005"),
	} {
		app.AddHTTPService(name, addr,
			&service.CircuitBreakerConfig{Threshold: 4, Interval: 2 * time.Second},
			&service.RateLimiterConfig{Requests: 20, Window: time.Second, Burst: 25},
			&service.HealthConfig{HealthEndpoint: ".well-known/health-check"},
		)
	}

	// Front-door protection: callers must send  X-Api-Key: agents-demo-key
	app.EnableAPIKeyAuth(envOr("API_KEY", "agents-demo-key"))

	app.POST("/assistant", assistant)

	app.Run()
}

// specialist maps a route to a target service + how to shape the request.
type specialist struct {
	service string
	path    string
	body    func(query string) []byte
}

var routes = map[string]specialist{
	"data": {"data-agent", "ask", func(q string) []byte {
		b, _ := json.Marshal(map[string]string{"question": q})
		return b
	}},
	"support": {"support-agent", "triage", func(q string) []byte {
		b, _ := json.Marshal(map[string]string{"title": q, "body": q})
		return b
	}},
	"kb": {"kb-agent", "ask", func(q string) []byte {
		b, _ := json.Marshal(map[string]string{"question": q})
		return b
	}},
	"review": {"code-review-agent", "review", func(q string) []byte {
		b, _ := json.Marshal(map[string]string{"title": q, "diff": q})
		return b
	}},
	"redact": {"pii-redaction-agent", "redact", func(q string) []byte {
		b, _ := json.Marshal(map[string]string{"text": q})
		return b
	}},
	"summarize": {"summarizer-agent", "summarize", func(q string) []byte {
		b, _ := json.Marshal(map[string]string{"text": q})
		return b
	}},
}

func assistant(c *gofr.Context) (any, error) {
	var in struct {
		Query string `json:"query"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	route := classify(c, in.Query)
	spec := routes[route]

	// Call the chosen specialist over the resilient HTTP service. The trace context
	// propagates automatically, so this call is a child span in the same trace.
	resp, err := c.GetHTTPService(spec.service).Post(c, spec.path, nil, spec.body(in.Query))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var payload any
	_ = json.Unmarshal(body, &payload)

	return map[string]any{
		"route":     route,
		"routed_to": spec.service,
		"response":  payload,
	}, nil
}

// classify asks the LLM which specialist should handle the query, and falls back to a
// deterministic keyword route when the model is unavailable or answers unexpectedly — so
// routing stays correct even when the LLM is slow, rate-limited, or flaky under load.
func classify(c *gofr.Context, query string) string {
	resp, err := c.LLM().Generate(c, "You are a router for a multi-agent system. Classify the user "+
		"request into exactly one word:\n"+
		"- data: products, orders, inventory, revenue, stats\n"+
		"- support: bug reports, errors, crashes, outages, tickets\n"+
		"- kb: IT/HR policy, leave, VPN, passwords, how-to questions\n"+
		"- review: a code diff / patch / pull request to review\n"+
		"- redact: text that may contain PII (names, emails, SSNs, cards) and needs redaction\n"+
		"- summarize: a long document, email thread or chat transcript that needs summarizing\n"+
		"Reply with ONLY the single word.\n\nRequest: "+query,
		ai.WithTemperature(0))
	if err != nil {
		return keywordRoute(query)
	}

	switch w := strings.ToLower(strings.TrimSpace(resp.Content)); {
	case strings.Contains(w, "support"):
		return "support"
	case strings.Contains(w, "kb"):
		return "kb"
	case strings.Contains(w, "review"):
		return "review"
	case strings.Contains(w, "redact"):
		return "redact"
	case strings.Contains(w, "summarize"):
		return "summarize"
	case strings.Contains(w, "data"):
		return "data"
	default:
		return keywordRoute(query)
	}
}

// keywordRoute is the deterministic fallback: a cheap heuristic used only when the model
// errors or returns something unexpected, instead of silently dumping everything on "data".
func keywordRoute(query string) string {
	q := strings.ToLower(query)

	switch {
	case containsAny(q, "diff", "patch", "pull request", "code review", "changeset"):
		return "review"
	case containsAny(q, "redact", "pii", "ssn", "credit card", "personally identifiable"):
		return "redact"
	case containsAny(q, "summarize", "summary", "tl;dr", "tldr", "thread", "long document"):
		return "summarize"
	case containsAny(q, "vpn", "password", "leave", "policy", "reset", "how do i", "how to"):
		return "kb"
	case containsAny(q, "crash", "error", "bug", "outage", "panic", "ticket", "not working"):
		return "support"
	default:
		return "data"
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}

	return false
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
