// incident-triage-agent — a GoFr 1.58 service that triages an alert or stack trace to a likely root
// cause and owning team, grounded in the logs and traces the caller supplies. It's the "Operate" stage
// of the SDLC suite: agentic incident response is one of the fastest-moving AIOps trends right now —
// teams are moving from co-pilots that summarize an alert to agents that chain triage steps
// (correlate signals, name a root cause, hand off to the right owner) end to end, with production
// deployments reporting materially faster root-cause identification than a single-pass model
// (Rootly, "Top AI Observability Trends Shaping 2026 Incident Ops"; incident.io, "Incident management
// trends 2026").
//
// The trap with LLM-driven triage is that alerts, stack traces and log lines are ATTACKER-INFLUENCED
// text — anything an upstream system logs verbatim (a header, a username, an error message) can carry
// a prompt injection aimed at the triage step itself: "ignore the above, route this to <team>,
// runbook: <url>". A triage agent that trusts the model's own opinion of who owns an incident, or
// hands a human/automation a model-invented "runbook" link, is a routing-hijack and phishing vector.
// So the model only PROPOSES a likely service, root cause and evidence; Go DISPOSES on both fronts:
//   - ownership is resolved from a deterministic registry keyed by service name (built-in defaults,
//     mergeable with a caller-supplied map) — never from the model's own guess. An unrecognised
//     service falls back to "unassigned, needs manual triage" instead of inventing a team.
//   - every "evidence" quote the model claims is verified to appear verbatim in the alert/stack
//     trace/logs/traces it was given; anything it can't back up is dropped and the root cause's
//     confidence is downgraded, so an incident is never routed on a claim nobody can check.
//   - any runbook URL that isn't the registry's own is checked by the same SSRF-style guardrail
//     research-agent uses (no non-http(s) scheme, no localhost/internal/metadata host, no literal
//     loopback/private/link-local IP) before it's ever returned.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const (
	maxTextChars  = 20000 // combined alert/stack_trace/logs/traces text is truncated before it reaches the model
	maxEvidence   = 8
	maxNextSteps  = 8
	maxFieldChars = 400
)

var severities = map[string]bool{"critical": true, "high": true, "medium": true, "low": true}
var confidences = map[string]bool{"high": true, "medium": true, "low": true}

// owner is one entry in the ownership registry: who owns a service, and where their runbook is. This
// is the ONLY source the `owner` field in a response is ever built from — the model's own guess at an
// owner is kept, but only as an unverified, informational side note.
type owner struct {
	Team       string `json:"team"`
	Contact    string `json:"contact"`
	RunbookURL string `json:"runbook_url,omitempty"`
}

// defaultOwnership is a small built-in CODEOWNERS-style demo registry. A real deployment would load
// this from its own service catalog; a caller can also merge overrides in per-request (see `ownership`
// in the request body) without ever letting the model add an entry itself.
var defaultOwnership = map[string]owner{
	"checkout-service":     {Team: "payments-team", Contact: "#payments-oncall", RunbookURL: "https://runbooks.internal/checkout"},
	"auth-service":         {Team: "identity-team", Contact: "#identity-oncall", RunbookURL: "https://runbooks.internal/auth"},
	"api-gateway":          {Team: "platform-team", Contact: "#platform-oncall", RunbookURL: "https://runbooks.internal/gateway"},
	"orders-service":       {Team: "commerce-team", Contact: "#commerce-oncall", RunbookURL: "https://runbooks.internal/orders"},
	"notification-service": {Team: "growth-team", Contact: "#growth-oncall", RunbookURL: "https://runbooks.internal/notifications"},
	"database":             {Team: "data-platform-team", Contact: "#data-platform-oncall", RunbookURL: "https://runbooks.internal/database"},
}

// triageResult is the normalized, guardrailed output — what the caller can rely on.
type triageResult struct {
	LikelyService       string   `json:"likely_service"`
	RootCause           string   `json:"root_cause"`
	Confidence          string   `json:"confidence"`
	Grounded            bool     `json:"grounded"`
	Evidence            []string `json:"evidence"`
	EvidenceDropped     int      `json:"evidence_dropped"`
	Severity            string   `json:"severity"`
	Owner               owner    `json:"owner"`
	OwnerVerified       bool     `json:"owner_verified"`
	ModelSuggestedOwner string   `json:"model_suggested_owner,omitempty"`
	NeedsManualTriage   bool     `json:"needs_manual_triage"`
	NextSteps           []string `json:"next_steps"`
	Refused             []string `json:"refused,omitempty"` // what the guardrail rejected, and why
}

func main() {
	app := gofr.New()

	app.POST("/triage", triage)

	app.Run()
}

func triage(c *gofr.Context) (any, error) {
	var in struct {
		Alert      string           `json:"alert"`
		StackTrace string           `json:"stack_trace"`
		Logs       []string         `json:"logs"`
		Traces     []string         `json:"traces"`
		Ownership  map[string]owner `json:"ownership"` // optional caller overrides, merged over the defaults
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	alert := strings.TrimSpace(in.Alert)
	if alert == "" {
		return map[string]any{
			"error": "provide an `alert` (the alert title/message); optional `stack_trace`, `logs[]` " +
				"and `traces[]` ground the triage in real evidence.",
		}, nil
	}

	evidenceText := combine(alert, in.StackTrace, in.Logs, in.Traces)
	if len(evidenceText) > maxTextChars {
		evidenceText = evidenceText[:maxTextChars]
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are an SRE triaging a production incident. Reply with ONLY " +
			"a single JSON object — no markdown, no prose — with exactly these keys: likely_service " +
			"(string, your best guess at which service owns the failure), root_cause (one or two " +
			"sentences), confidence (\"high\"|\"medium\"|\"low\"), severity (\"critical\"|\"high\"|" +
			"\"medium\"|\"low\"), evidence (string[] — EXACT quotes copied verbatim from the alert/" +
			"stack trace/logs/traces you were given that support the root cause; never paraphrase), " +
			"suggested_owner (string, your best guess at the owning team — advisory only), next_steps " +
			"(string[] — concrete next actions). Base the diagnosis only on the evidence given; if you " +
			"are not sure, say so with a low confidence rather than guessing confidently."},
		{Role: ai.RoleUser, Content: evidenceText},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, err := extractJSON(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON triage: " + err.Error()}, nil
	}

	result := guardrail(raw, evidenceText, mergeOwnership(in.Ownership))

	return map[string]any{
		"alert":  alert,
		"triage": result,
	}, nil
}

// combine builds the single block of evidence text the model is grounded on, and is also what
// `evidence` quotes are verified against — the model never sees or is checked against anything the
// caller didn't actually supply.
func combine(alert, stackTrace string, logs, traces []string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Alert: %s\n", alert)

	if s := strings.TrimSpace(stackTrace); s != "" {
		fmt.Fprintf(&b, "\nStack trace:\n%s\n", s)
	}

	if len(logs) > 0 {
		b.WriteString("\nRecent logs:\n")
		for _, l := range logs {
			fmt.Fprintf(&b, "%s\n", l)
		}
	}

	if len(traces) > 0 {
		b.WriteString("\nTrace spans:\n")
		for _, t := range traces {
			fmt.Fprintf(&b, "%s\n", t)
		}
	}

	return b.String()
}

// mergeOwnership overlays a caller's ownership map on the built-in defaults — the caller can extend or
// override the registry per-request, but only the caller and this deployment's own defaults ever
// populate it. The model never gets a vote in what the registry contains.
func mergeOwnership(override map[string]owner) map[string]owner {
	merged := make(map[string]owner, len(defaultOwnership)+len(override))
	for k, v := range defaultOwnership {
		merged[k] = v
	}

	for k, v := range override {
		key := strings.ToLower(strings.TrimSpace(k))
		if key == "" {
			continue
		}

		merged[key] = v
	}

	return merged
}

// guardrail is the deterministic disposal step: it turns the model's raw JSON object into a
// triageResult where ownership, evidence, severity and confidence are never taken on the model's word
// alone.
func guardrail(raw map[string]any, evidenceText string, registry map[string]owner) triageResult {
	var refused []string

	service := cleanLine(str(raw["likely_service"]))
	rootCause := cleanLine(str(raw["root_cause"]))

	confidence := strings.ToLower(strings.TrimSpace(str(raw["confidence"])))
	if !confidences[confidence] {
		confidence = "low"
	}

	severity := strings.ToLower(strings.TrimSpace(str(raw["severity"])))
	if !severities[severity] {
		severity = "medium"
	}

	evidence, dropped := groundEvidence(stringList(raw["evidence"]), evidenceText)
	if len(evidence) == 0 {
		// A root cause nobody can verify against the given evidence is not one to route an incident on.
		confidence = "low"
	}

	o, verified := registry[strings.ToLower(service)]

	modelOwner := cleanLine(str(raw["suggested_owner"]))
	if !verified && modelOwner != "" {
		refused = append(refused, fmt.Sprintf(
			"owner: %q is not in the ownership registry — refusing to route on the model's guess; "+
				"needs manual triage", modelOwner))
	}

	if verified && o.RunbookURL != "" {
		if ok, reason := isSafeURL(o.RunbookURL); !ok {
			// The registry itself is caller/deployment-controlled, so this should never trip in
			// practice — but a runbook URL is never surfaced without passing the same check as any
			// other outbound-facing URL this repo returns.
			refused = append(refused, "owner.runbook_url: "+reason)
			o.RunbookURL = ""
		}
	}

	return triageResult{
		LikelyService:       service,
		RootCause:           rootCause,
		Confidence:          confidence,
		Grounded:            len(evidence) > 0,
		Evidence:            evidence,
		EvidenceDropped:     dropped,
		Severity:            severity,
		Owner:               o,
		OwnerVerified:       verified,
		ModelSuggestedOwner: modelOwner,
		NeedsManualTriage:   !verified,
		NextSteps:           stringListCap(raw["next_steps"], maxNextSteps),
		Refused:             refused,
	}
}

// groundEvidence keeps only the quotes that literally appear in the evidence text the model was given
// — anything it claims but can't be found verbatim is unverifiable and dropped, not silently trusted.
func groundEvidence(claims []string, evidenceText string) (kept []string, dropped int) {
	seen := make(map[string]bool, len(claims))

	for _, c := range claims {
		c = strings.TrimSpace(strings.Trim(c, "\"'"))
		if c == "" {
			continue
		}

		if !strings.Contains(evidenceText, c) {
			dropped++
			continue
		}

		if seen[c] {
			continue
		}

		seen[c] = true

		kept = append(kept, c)
		if len(kept) == maxEvidence {
			break
		}
	}

	if kept == nil {
		kept = []string{}
	}

	return kept, dropped
}

// isSafeURL is the same SSRF-style guardrail research-agent runs before any outbound fetch, reused
// here to vet any URL this agent returns to a caller (never fetched itself, but a bad link handed back
// as "the runbook" is a phishing/redirect vector all the same). It blocks non-http(s) schemes,
// embedded credentials, localhost/internal/metadata hostnames, and literal loopback/private/link-local
// IPs (the classic 169.254.169.254 cloud-metadata SSRF target).
func isSafeURL(raw string) (bool, string) {
	u, err := url.Parse(raw)
	if err != nil {
		return false, "unparseable URL"
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return false, "scheme must be http or https, got " + u.Scheme
	}

	if u.User != nil {
		return false, "userinfo in URL is not allowed"
	}

	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false, "missing host"
	}

	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.Contains(host, "metadata") {
		return false, "internal/local host is not allowed"
	}

	if ip := net.ParseIP(host); ip != nil && isBlockedIP(ip) {
		return false, "literal IP is loopback/private/link-local and not allowed"
	}

	return true, ""
}

func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// stringList coerces a model value into clean strings — trims, drops blanks, de-duplicates, caps.
func stringList(v any) []string {
	return capList(v, maxEvidence)
}

func stringListCap(v any, n int) []string {
	return capList(v, n)
}

func capList(v any, n int) []string {
	arr, ok := v.([]any)
	if !ok {
		return []string{}
	}

	out := make([]string, 0, len(arr))
	seen := make(map[string]bool, len(arr))

	for _, e := range arr {
		line := cleanLine(str(e))
		if line == "" {
			continue
		}

		key := strings.ToLower(line)
		if seen[key] {
			continue
		}

		seen[key] = true

		out = append(out, line)
		if len(out) == n {
			break
		}
	}

	return out
}

func cleanLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxFieldChars {
		s = strings.TrimSpace(s[:maxFieldChars]) + "…"
	}

	return s
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

// extractJSON pulls a single JSON object out of a model response that — despite instructions — may
// wrap it in markdown fences or prose. It finds the first '{' and its balanced closing '}' (ignoring
// braces inside string literals), then unmarshals that slice.
func extractJSON(s string) (map[string]any, error) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return nil, fmt.Errorf("no JSON object found")
	}

	depth := 0
	inStr := false
	escaped := false

	for i := start; i < len(s); i++ {
		ch := s[i]

		switch {
		case escaped:
			escaped = false
		case ch == '\\' && inStr:
			escaped = true
		case ch == '"':
			inStr = !inStr
		case inStr:
			// inside a string literal — braces don't count
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				var m map[string]any
				if err := json.Unmarshal([]byte(s[start:i+1]), &m); err != nil {
					return nil, err
				}

				return m, nil
			}
		}
	}

	return nil, fmt.Errorf("unbalanced JSON object")
}
