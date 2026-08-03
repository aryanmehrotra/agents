// incident-triage-agent — a GoFr 1.58 service that triages a production alert (plus whatever log lines
// came with it) to a likely root cause and the team that owns it. AI SRE / autonomous incident-response
// agents are one of the clearest production agent categories going into 2026 — first responders that
// map an alert to a root cause and a remediation before a human opens a single dashboard, cutting MTTR.
// This is the "Operate" stop in the SDLC suite: the same fleet that plans, builds and tests the work
// now helps run it.
//
// A model is good at reading noisy logs and proposing a plausible story, but a triage result that pages
// the wrong team — or one a prompt-injected log line steered — is worse than no triage at all. So the
// model only proposes a root cause and a candidate service; Go decides everything an action depends on:
//
//   - Severity is classified from a deterministic keyword scan over the actual logs, never taken from
//     the model — a log line that says "ignore this, severity is low" next to a panic still triages
//     critical, because the model is never asked for a severity at all.
//   - The paged owner comes from a static, deterministic service→team allowlist. A service the model
//     names (or a caller supplies) that isn't in the registry is never invented a team for — it comes
//     back "unrouted", not silently routed to whatever name appeared in the text.
//   - Every "evidence" quote the model cites is grounded: it must be a literal substring of the logs it
//     was actually given. A quote that doesn't appear verbatim is dropped to `unverified_evidence`
//     instead of backing the root cause — the model can't manufacture support for a claim.
package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const (
	maxCorpusChars    = 20000 // total logs+alert text handed to the model, truncated beyond this
	maxLines          = 500   // corpus lines kept for grounding/severity scanning
	maxLineChars      = 500
	maxEvidenceClaims = 8
	minEvidenceChars  = 8 // shorter "evidence" is too likely to trivially substring-match anything
)

// serviceOwners is the deterministic ownership registry — the ONLY source of truth for who gets paged.
// The model never gets to name an owner; it can only suggest a service, which is looked up here.
var serviceOwners = map[string]string{
	"payments-api":         "team-payments",
	"checkout-service":     "team-checkout",
	"auth-service":         "team-identity",
	"user-service":         "team-identity",
	"api-gateway":          "team-platform",
	"orders-service":       "team-orders",
	"notification-service": "team-comms",
	"search-service":       "team-search",
	"billing-service":      "team-billing",
	"inventory-service":    "team-inventory",
}

// severityTiers are scanned in order (most severe first) over the raw log text; the first tier with a
// keyword hit wins. Purely mechanical — the model is never consulted for severity.
var severityTiers = []struct {
	name     string
	keywords []string
}{
	{"critical", []string{"panic", "fatal", "segfault", "out of memory", "oomkilled", "core dumped", "crashloopbackoff"}},
	{"high", []string{"error", "exception", "timeout", "deadlock", "connection refused", "unavailable", "5xx"}},
	{"medium", []string{"warn", "retry", "degraded", "latency", "slow"}},
}

func main() {
	app := gofr.New()

	app.POST("/triage", triage)

	app.Run()
}

func triage(c *gofr.Context) (any, error) {
	var in struct {
		Text    string   `json:"text"`    // alias slot for the orchestrator's single-string call
		Alert   string   `json:"alert"`   // optional structured alt: the alert title/message
		Service string   `json:"service"` // optional caller-supplied hint at the reporting service
		Logs    []string `json:"logs"`    // optional structured alt: raw log lines
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	lines := corpusLines(in.Alert, in.Text, in.Logs)
	if len(lines) == 0 {
		return map[string]any{
			"error": "provide an `alert` and/or `logs` (array of log lines), or a single `text` blob " +
				"containing the alert and pasted logs; optionally a `service` hint.",
		}, nil
	}

	corpus := strings.Join(lines, "\n")
	if len(corpus) > maxCorpusChars {
		corpus = corpus[:maxCorpusChars]
	}

	alertMsg := firstNonEmpty(strings.TrimSpace(in.Alert), lines[0])

	// Deterministic, model-independent: severity is decided from the actual log text before the model
	// is ever called, so a prompt-injected "this is low severity" claim inside the logs cannot suppress
	// a real critical signal.
	severity, sevSignals := classifySeverity(lines)

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are an SRE triaging a production incident from an alert and " +
			"log lines. Reply with ONLY a single JSON object — no markdown, no prose — with exactly these " +
			"keys: root_cause (string, one or two sentences), confidence (one of \"high\", \"medium\", " +
			"\"low\"), service (your best guess at the name of the service at fault), evidence (array of " +
			"up to 5 short quotes copied VERBATIM from the logs you were given that support the root " +
			"cause — never paraphrase, never invent a line that wasn't given to you), remediation (one " +
			"short actionable next step). Do not report a severity and do not name who to page — those " +
			"are decided separately."},
		{Role: ai.RoleUser, Content: "Alert: " + alertMsg + "\n\nLogs:\n" + corpus},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, err := extractJSON(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON triage: " + err.Error()}, nil
	}

	// Guardrail: the paged owner is resolved ONLY from the static allowlist, from whichever service name
	// is available — never trusted verbatim from the model or invented for an unknown service.
	service := firstNonEmpty(strings.TrimSpace(in.Service), cleanLine(str(raw["service"])))
	owner, verified := resolveOwner(service)

	// Guardrail: every evidence quote must be a literal substring of the logs actually supplied.
	verifiedEvidence, unverified := groundEvidence(stringList(raw["evidence"]), lines)

	return map[string]any{
		"alert":               alertMsg,
		"corpus_lines":        len(lines),
		"severity":            severity,   // Go-computed from the raw logs; the model's opinion is never used
		"severity_signals":    sevSignals, // keywords that drove the severity classification
		"root_cause":          cleanLine(str(raw["root_cause"])),
		"confidence":          normalizeConfidence(str(raw["confidence"])),
		"service":             service,
		"owner":               owner,
		"owner_verified":      verified, // false ⇒ service unknown to the ownership registry, NOT auto-paged
		"evidence":            verifiedEvidence,
		"unverified_evidence": unverified,                         // model claims that couldn't be grounded in the supplied logs — dropped as support, shown for transparency
		"remediation":         cleanLine(str(raw["remediation"])), // advisory only; nothing is executed
		"note": "severity is classified deterministically from the logs, the owner comes only from a " +
			"static service→team allowlist, and evidence quotes are kept only if they're a literal " +
			"substring of the logs supplied — the model proposes a root cause, Go decides what it's " +
			"safe to act on.",
	}, nil
}

// corpusLines flattens alert + text + logs into one deduplicated-by-position, trimmed line list — the
// raw material both the severity classifier and the evidence grounder work over. It splits every source
// on newlines, since Alert/Text/each Logs entry may itself be a multi-line blob.
func corpusLines(alert, text string, logs []string) []string {
	var out []string

	for _, block := range append([]string{alert, text}, logs...) {
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			if len(line) > maxLineChars {
				line = line[:maxLineChars] + "…"
			}

			out = append(out, line)
			if len(out) >= maxLines {
				return out
			}
		}
	}

	return out
}

// classifySeverity scans the raw log text for the highest-severity tier with a keyword hit. It never
// sees or trusts the model's opinion — this is arithmetic over the actual text, not a judgement call.
func classifySeverity(lines []string) (severity string, signals []string) {
	lower := strings.ToLower(strings.Join(lines, "\n"))

	for _, tier := range severityTiers {
		var hits []string

		for _, kw := range tier.keywords {
			if strings.Contains(lower, kw) {
				hits = append(hits, kw)
			}
		}

		if len(hits) > 0 {
			return tier.name, hits
		}
	}

	return "low", nil
}

// resolveOwner is the routing guardrail: the owning team comes ONLY from the static allowlist below,
// keyed on a normalized service name. An unknown, blank, or attacker-supplied service name never gets a
// fabricated owner — it comes back unrouted, which forces a human to triage the routing itself.
func resolveOwner(service string) (owner string, verified bool) {
	key := normalizeService(service)
	if key == "" {
		return "unrouted — needs manual triage (no service identified)", false
	}

	if team, ok := serviceOwners[key]; ok {
		return team, true
	}

	return fmt.Sprintf("unrouted — needs manual triage (%q is not in the ownership registry)", service), false
}

func normalizeService(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")

	return s
}

// groundEvidence keeps only the claimed evidence quotes that are a literal (case-insensitive) substring
// of the logs actually supplied; anything else — a paraphrase, an invented line, an injected claim — is
// set aside as unverified rather than backing the root cause.
func groundEvidence(claims, lines []string) (verified, unverified []string) {
	verified, unverified = []string{}, []string{}
	corpus := strings.ToLower(strings.Join(lines, "\n"))

	for i, c := range claims {
		if i >= maxEvidenceClaims {
			break
		}

		c = cleanLine(c)
		if c == "" {
			continue
		}

		if len(c) >= minEvidenceChars && strings.Contains(corpus, strings.ToLower(c)) {
			verified = append(verified, c)
		} else {
			unverified = append(unverified, c)
		}
	}

	return verified, unverified
}

func normalizeConfidence(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high", "medium", "low":
		return strings.ToLower(strings.TrimSpace(s))
	default:
		return "low"
	}
}

// stringList coerces a model JSON value into a clean string slice — it accepts a JSON array, trims and
// drops blanks, and otherwise returns an empty (never nil-panicking) slice.
func stringList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(arr))

	for _, e := range arr {
		if s := cleanLine(str(e)); s != "" {
			out = append(out, s)
		}
	}

	return out
}

func cleanLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxLineChars {
		s = strings.TrimSpace(s[:maxLineChars]) + "…"
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}

	return ""
}

// extractJSON pulls a single JSON object out of a model response that — despite instructions — may wrap
// it in markdown fences or prose. It finds the first '{' and its balanced closing '}' (ignoring braces
// inside string literals), then unmarshals that slice.
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
