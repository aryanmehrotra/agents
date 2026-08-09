// incident-triage-agent — the "Operate" stage of the SDLC suite. It turns a raw alert / stack trace /
// log excerpt into a triage: a likely root-cause category, a confidence, the candidate owning service,
// remediation steps, and — grounded in a static ownership registry, never the model's own guess — the
// on-call team to page. AI SRE / incident-triage agents that shorten the gap between "something's
// wrong" and "we know where to look and who owns it" are one of the clearest 2026 production agent
// patterns, moving the first minutes of an incident from a human paging around to an agent doing it.
//
// The LLM only *proposes*: a root-cause category, a confidence, which service looks responsible, a
// one-line summary, remediation steps, and — only if the alert/runbook text it was given actually names
// one — a webhook URL to page the owning team. Go disposes on every one of those:
//   - the category and confidence are clamped to a fixed enum — a model that invents a category never
//     reaches a caller or a downstream system keyed off it;
//   - the paged "owner" is always resolved from a static ownership registry, by looking up the model's
//     candidate service name — the model is never trusted to name a team directly, so it can't
//     hallucinate an on-call team to page;
//   - because the notify URL comes out of untrusted, model-parsed alert text, it goes through the same
//     deterministic SSRF guardrail research-agent and scheduler-agent use before any real outbound page
//     is fired — an alert that tries to redirect the page to an internal/cloud-metadata host is refused,
//     never sent.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gofr.dev/pkg/gofr/logging"
)

const (
	notifyTimeout      = 8 * time.Second
	maxRemediationStep = 8
	maxAlertExcerpt    = 240
)

// validCategories is the fixed root-cause taxonomy. A category the model returns outside this set is
// clamped to "unknown" — never passed through as free text to a caller or paging system.
var validCategories = map[string]bool{
	"timeout":           true,
	"oom":               true,
	"deploy-regression": true,
	"dependency-outage": true,
	"config-error":      true,
	"data-corruption":   true,
	"network-partition": true,
	"unknown":           true,
}

var validConfidences = map[string]bool{"low": true, "medium": true, "high": true}

// ownershipRegistry is the static, deterministic source of truth for who owns what. The model proposes
// a candidate service name; Go — never the model — decides which team that resolves to. Unlisted here,
// keys are matched case-insensitively as a substring of the candidate service name.
var ownershipRegistry = map[string]string{
	"checkout":     "payments-team",
	"payment":      "payments-team",
	"billing":      "payments-team",
	"auth":         "identity-team",
	"login":        "identity-team",
	"api-gateway":  "platform-team",
	"gateway":      "platform-team",
	"search":       "search-team",
	"inventory":    "commerce-team",
	"catalog":      "commerce-team",
	"notification": "platform-team",
	"scheduler":    "platform-team",
}

// defaultOwner is who gets paged when the candidate service doesn't match any known owner — a safe,
// generic on-call rather than silence or a hallucinated team name.
const defaultOwner = "platform-oncall"

// notifyClient fires the real outbound page. It rides GoFr's OpenTelemetry transport (like
// research-agent and scheduler-agent) so a fired page shows up as a span, and re-checks the SSRF
// guardrail on every redirect hop.
var notifyClient = &http.Client{
	Timeout:   notifyTimeout,
	Transport: otelhttp.NewTransport(http.DefaultTransport),
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("stopped after 5 redirects")
		}

		if ok, reason := isSafeURL(req.URL.String()); !ok {
			return fmt.Errorf("redirect blocked: %s", reason)
		}

		return nil
	},
}

func main() {
	app := gofr.New()

	app.POST("/triage", triage)

	app.Run()
}

// triageSpec is the shape the model is asked to produce — parsed and then normalized/validated
// deterministically before any of it is trusted.
type triageSpec struct {
	Category         string   `json:"category"`
	Confidence       string   `json:"confidence"`
	Service          string   `json:"service"`
	Summary          string   `json:"summary"`
	RemediationSteps []string `json:"remediation_steps"`
	NotifyURL        string   `json:"notify_url"`
}

func triage(c *gofr.Context) (any, error) {
	var in struct {
		Alert string `json:"alert"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	if strings.TrimSpace(in.Alert) == "" {
		return map[string]any{"error": "alert must not be empty"}, nil
	}

	resp, err := c.LLM().Generate(c, triagePrompt(in.Alert), ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	spec, err := parseTriageResponse(resp.Content)
	if err != nil {
		return map[string]any{
			"alert":   excerpt(in.Alert, maxAlertExcerpt),
			"refused": true,
			"reason":  "could not understand the alert: " + err.Error(),
		}, nil
	}

	owner, ownerMatched := resolveOwner(spec.Service)

	result := map[string]any{
		"alert_excerpt":     excerpt(in.Alert, maxAlertExcerpt),
		"category":          normalizeCategory(spec.Category),
		"confidence":        normalizeConfidence(spec.Confidence),
		"candidate_service": spec.Service,
		"owner":             owner,
		"owner_matched":     ownerMatched,
		"summary":           spec.Summary,
		"remediation_steps": normalizeSteps(spec.RemediationSteps),
		"notify":            notifyIfRequested(c.Logger, spec.NotifyURL, owner, spec.Summary),
	}

	return result, nil
}

// notifyIfRequested pages the resolved owner only when the model actually extracted a webhook from the
// alert/runbook text. The guardrail runs BEFORE any outbound request — a URL that resolves to an
// unsafe target is refused and reported, never fired.
func notifyIfRequested(logger logging.Logger, notifyURL, owner, summary string) map[string]any {
	if strings.TrimSpace(notifyURL) == "" {
		return map[string]any{"attempted": false}
	}

	if ok, reason := isSafeURL(notifyURL); !ok {
		return map[string]any{"attempted": true, "refused": true, "reason": reason}
	}

	body, _ := json.Marshal(map[string]any{
		"owner":    owner,
		"summary":  summary,
		"paged_at": time.Now().UTC(),
	})

	req, err := http.NewRequest(http.MethodPost, notifyURL, bytes.NewReader(body))
	if err != nil {
		return map[string]any{"attempted": true, "refused": false, "error": err.Error()}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "agents-incident-triage-agent/1.0")

	resp, err := notifyClient.Do(req)
	if err != nil {
		logger.Errorf("incident-triage-agent: page to %s failed: %v", owner, err)
		return map[string]any{"attempted": true, "refused": false, "error": err.Error()}
	}
	defer resp.Body.Close()

	return map[string]any{"attempted": true, "refused": false, "result": fmt.Sprintf("HTTP %d", resp.StatusCode)}
}

// resolveOwner is the guardrail behind the "who gets paged" decision: it never trusts a team name the
// model might invent, only a candidate service name, which it resolves against the static
// ownershipRegistry. No match falls back to a safe, generic on-call rather than silence or a guess.
func resolveOwner(service string) (owner string, matched bool) {
	s := strings.ToLower(strings.TrimSpace(service))
	if s == "" {
		return defaultOwner, false
	}

	for key, team := range ownershipRegistry {
		if strings.Contains(s, key) {
			return team, true
		}
	}

	return defaultOwner, false
}

// normalizeCategory clamps the model's root-cause category to the fixed taxonomy — a value outside it
// becomes "unknown" rather than passing arbitrary free text on to a caller or paging system.
func normalizeCategory(cat string) string {
	c := strings.ToLower(strings.TrimSpace(cat))
	if validCategories[c] {
		return c
	}

	return "unknown"
}

// normalizeConfidence clamps to low/medium/high, defaulting to the conservative "low" for anything else.
func normalizeConfidence(conf string) string {
	c := strings.ToLower(strings.TrimSpace(conf))
	if validConfidences[c] {
		return c
	}

	return "low"
}

// normalizeSteps drops blank entries and caps the list — a model asked for remediation steps will
// sometimes return an empty string in the list or an unbounded number of them.
func normalizeSteps(steps []string) []string {
	out := make([]string, 0, len(steps))

	for _, s := range steps {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}

		out = append(out, s)

		if len(out) == maxRemediationStep {
			break
		}
	}

	return out
}

func excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "…"
}

func triagePrompt(alert string) string {
	return "You triage a production incident from a raw alert, stack trace, or log excerpt. Reply with " +
		"ONLY a single JSON object, no markdown fences, no explanation, of the exact shape:\n" +
		`{"category": "<one of: timeout, oom, deploy-regression, dependency-outage, config-error, ` +
		`data-corruption, network-partition, unknown>", "confidence": "<low|medium|high>", ` +
		`"service": "<the service name most likely responsible, inferred from the text>", ` +
		`"summary": "<one or two sentence root-cause hypothesis>", ` +
		`"remediation_steps": ["<short actionable step>", "..."], ` +
		`"notify_url": "<a webhook/paging URL ONLY if one is explicitly present in the alert or runbook ` +
		`text below, else an empty string — never invent one>"}` +
		"\n\nAlert:\n" + alert
}

// parseTriageResponse is pure and deterministic: it strips markdown fences models sometimes add despite
// being told not to, then decodes strict JSON. It never trusts the model's output beyond "this is
// well-formed JSON" — normalizeCategory/normalizeConfidence/resolveOwner/isSafeURL do the rest.
func parseTriageResponse(raw string) (triageSpec, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	var spec triageSpec
	if err := json.Unmarshal([]byte(s), &spec); err != nil {
		return triageSpec{}, fmt.Errorf("not valid JSON: %w", err)
	}

	return spec, nil
}

// isSafeURL is the SSRF guardrail, identical in spirit to research-agent's and scheduler-agent's: only
// http/https, no userinfo, no localhost/internal/metadata hostnames, no literal loopback/private/
// link-local IPs (the classic 169.254.169.254 cloud-metadata SSRF target). Because paging an owner
// means a real outbound request built from a URL the model pulled out of untrusted alert text, this
// must run deterministically in Go — never left to the model's restraint.
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

	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") ||
		strings.Contains(host, "metadata") {
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
