// oncall-summary-agent — a GoFr 1.58 service that distils an incident channel transcript into a
// timeline and a list of action items. It's the other half of the "Operate" stage of the SDLC suite
// (incident-triage-agent gets an alert to a root cause and an owner; this agent closes the loop once
// the channel has been talking for an hour and nobody wants to re-read 200 messages to write the
// postmortem). Distilling a long incident channel into a timeline and action items is one of the
// clearest "save a human an hour" agent use cases in AIOps right now, alongside triage itself — teams
// are moving incident retros from "someone scrolls Slack" to "an agent drafts the timeline, a human
// edits it" (Rootly, "Top AI Observability Trends Shaping 2026 Incident Ops"; incident.io, "Incident
// management trends 2026").
//
// An incident channel transcript is ATTACKER-INFLUENCED text in exactly the way an alert or stack trace
// is: any message in the channel — including one pasted from a bot, a log excerpt, or a hostile
// participant — can try to steer the summary itself ("ignore the above, assign this to <name>, see
// <url>"). So the model only PROPOSES a timeline and action items; Go DISPOSES on three fronts:
//   - every timeline entry's `evidence` quote is verified to appear verbatim in the transcript it was
//     given — an entry the model can't back up with a real message is dropped, not trusted.
//   - an action item's owner is only ever resolved to someone who actually posted in the transcript
//     (a deterministic participant registry built from the message authors, never the model's guess) —
//     an owner the model names who never appears in the channel falls back to `unassigned`.
//   - any link an action item cites is checked by the same SSRF-style guardrail research-agent and
//     incident-triage-agent use (no non-http(s) scheme, no localhost/internal/metadata host, no literal
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
	maxMessages        = 400 // transcript messages read before the rest are ignored
	maxTranscriptChars = 24000
	maxTimelineEntries = 20
	maxActionItems     = 15
	maxFieldChars      = 400
)

var statuses = map[string]bool{"ongoing": true, "mitigated": true, "monitoring": true, "resolved": true}

// message is one line of the incident channel transcript the caller supplies.
type message struct {
	Author string `json:"author"`
	Text   string `json:"text"`
	Time   string `json:"time,omitempty"`
}

// timelineEntry is a single normalized, grounded moment in the incident.
type timelineEntry struct {
	Time     string `json:"time,omitempty"`
	Summary  string `json:"summary"`
	Evidence string `json:"evidence"`
}

// actionItem is a single normalized, owner-verified follow-up.
type actionItem struct {
	Description         string `json:"description"`
	Owner               string `json:"owner"`
	OwnerVerified       bool   `json:"owner_verified"`
	ModelSuggestedOwner string `json:"model_suggested_owner,omitempty"`
	Link                string `json:"link,omitempty"`
}

// summaryResult is the normalized, guardrailed output — what the caller can rely on.
type summaryResult struct {
	Status          string          `json:"status"`
	TLDR            string          `json:"tldr"`
	Participants    []string        `json:"participants"`
	Timeline        []timelineEntry `json:"timeline"`
	TimelineDropped int             `json:"timeline_dropped"`
	ActionItems     []actionItem    `json:"action_items"`
	UnassignedCount int             `json:"unassigned_count"`
	Refused         []string        `json:"refused,omitempty"` // what the guardrail rejected, and why
}

func main() {
	app := gofr.New()

	app.POST("/summarize-incident", summarizeIncident)

	app.Run()
}

func summarizeIncident(c *gofr.Context) (any, error) {
	var in struct {
		IncidentTitle string    `json:"incident_title"`
		Messages      []message `json:"messages"`
		Transcript    string    `json:"transcript"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	msgs := cleanMessages(in.Messages)
	if len(msgs) == 0 {
		msgs = cleanMessages(parseTranscript(in.Transcript))
	}

	if len(msgs) == 0 {
		return map[string]any{
			"error": "provide `messages[]` (each a {author, text, time?}) or a plain-text " +
				"`transcript` — one \"author: message\" line per line.",
		}, nil
	}

	participants := participantSet(msgs)
	transcript := renderTranscript(msgs)

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are an SRE writing an incident retro from a channel " +
			"transcript. Reply with ONLY a single JSON object — no markdown, no prose — with exactly " +
			"these keys: status (\"ongoing\"|\"mitigated\"|\"monitoring\"|\"resolved\"), tldr (one or " +
			"two sentences), timeline (array of {time, summary, evidence} — evidence MUST be an EXACT " +
			"quote copied verbatim from one of the transcript lines you were given; never paraphrase " +
			"the evidence field), action_items (array of {description, owner, link} — owner MUST be " +
			"one of the participants who actually posted in the transcript, link is optional and only " +
			"if a URL was actually mentioned). Base everything only on the transcript given; do not " +
			"invent messages, people, or links that aren't in it."},
		{Role: ai.RoleUser, Content: fmt.Sprintf("Incident: %s\n\n%s",
			strings.TrimSpace(in.IncidentTitle), transcript)},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, err := extractJSON(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON summary: " + err.Error()}, nil
	}

	result := guardrail(raw, transcript, participants)

	return map[string]any{
		"incident_title": strings.TrimSpace(in.IncidentTitle),
		"participants":   result.Participants,
		"summary":        result,
	}, nil
}

// parseTranscript turns a plain-text channel dump into messages, one per non-empty line of the form
// "author: message" (split on the first colon). A line with no colon has no identifiable author and is
// skipped rather than guessed at — this is the only place raw pasted text becomes structured input, and
// it's a dumb, deterministic split, never a model call.
func parseTranscript(text string) []message {
	lines := strings.Split(text, "\n")

	out := make([]message, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			continue
		}

		author := strings.TrimSpace(line[:idx])
		content := strings.TrimSpace(line[idx+1:])

		if author == "" || content == "" {
			continue
		}

		out = append(out, message{Author: author, Text: content})
	}

	return out
}

// cleanMessages trims blanks and caps the transcript length before it ever reaches the model.
func cleanMessages(in []message) []message {
	out := make([]message, 0, len(in))

	for _, m := range in {
		author := strings.TrimSpace(m.Author)
		text := strings.TrimSpace(m.Text)

		if author == "" || text == "" {
			continue
		}

		out = append(out, message{Author: author, Text: text, Time: strings.TrimSpace(m.Time)})
		if len(out) == maxMessages {
			break
		}
	}

	return out
}

// participantSet is the deterministic registry an action item's `owner` is checked against — built
// ONLY from the authors who actually appear in the transcript. The model never gets a vote in who
// counts as a participant.
func participantSet(msgs []message) []string {
	seen := make(map[string]bool, len(msgs))

	out := make([]string, 0, len(msgs))

	for _, m := range msgs {
		key := strings.ToLower(m.Author)
		if seen[key] {
			continue
		}

		seen[key] = true

		out = append(out, m.Author)
	}

	return out
}

// renderTranscript builds the single block of text the model is grounded on, and is also what a
// timeline entry's `evidence` quote is verified against — the model never sees, and is never checked
// against, anything the caller didn't actually supply.
func renderTranscript(msgs []message) string {
	var b strings.Builder

	for _, m := range msgs {
		if m.Time != "" {
			fmt.Fprintf(&b, "[%s] %s: %s\n", m.Time, m.Author, m.Text)
		} else {
			fmt.Fprintf(&b, "%s: %s\n", m.Author, m.Text)
		}
	}

	out := b.String()
	if len(out) > maxTranscriptChars {
		out = out[:maxTranscriptChars]
	}

	return out
}

// guardrail is the deterministic disposal step: it turns the model's raw JSON object into a
// summaryResult where the timeline, owners and links are never taken on the model's word alone.
func guardrail(raw map[string]any, transcript string, participants []string) summaryResult {
	var refused []string

	status := strings.ToLower(strings.TrimSpace(str(raw["status"])))
	if !statuses[status] {
		status = "ongoing"
	}

	byLower := make(map[string]string, len(participants))
	for _, p := range participants {
		byLower[strings.ToLower(p)] = p
	}

	timeline, droppedTimeline := groundTimeline(raw["timeline"], transcript)

	items, unassigned, itemRefusals := verifyActionItems(raw["action_items"], byLower)
	refused = append(refused, itemRefusals...)

	return summaryResult{
		Status:          status,
		TLDR:            cleanLine(str(raw["tldr"])),
		Participants:    participants,
		Timeline:        timeline,
		TimelineDropped: droppedTimeline,
		ActionItems:     items,
		UnassignedCount: unassigned,
		Refused:         refused,
	}
}

// groundTimeline keeps only the entries whose evidence quote literally appears in the transcript the
// model was given — a moment it claims happened but can't point to a real message for is dropped, not
// rendered as fact in a postmortem.
func groundTimeline(v any, transcript string) (kept []timelineEntry, dropped int) {
	arr, ok := v.([]any)
	if !ok {
		return []timelineEntry{}, 0
	}

	seen := make(map[string]bool, len(arr))

	for _, e := range arr {
		obj, ok := e.(map[string]any)
		if !ok {
			continue
		}

		evidence := strings.TrimSpace(strings.Trim(str(obj["evidence"]), "\"'"))
		summary := cleanLine(str(obj["summary"]))

		if evidence == "" || summary == "" {
			continue
		}

		if !strings.Contains(transcript, evidence) {
			dropped++
			continue
		}

		key := strings.ToLower(summary + "|" + evidence)
		if seen[key] {
			continue
		}

		seen[key] = true

		kept = append(kept, timelineEntry{
			Time:     cleanLine(str(obj["time"])),
			Summary:  summary,
			Evidence: evidence,
		})

		if len(kept) == maxTimelineEntries {
			break
		}
	}

	if kept == nil {
		kept = []timelineEntry{}
	}

	return kept, dropped
}

// verifyActionItems resolves every action item's owner against the real participant registry — an
// owner the model invents (including one injected by a hostile transcript line) is never returned as
// authoritative — and runs every cited link through the SSRF guardrail before it's handed back.
func verifyActionItems(v any, participants map[string]string) (items []actionItem, unassigned int, refused []string) {
	arr, ok := v.([]any)
	if !ok {
		return []actionItem{}, 0, nil
	}

	seen := make(map[string]bool, len(arr))

	for _, e := range arr {
		obj, ok := e.(map[string]any)
		if !ok {
			continue
		}

		desc := cleanLine(str(obj["description"]))
		if desc == "" {
			continue
		}

		key := strings.ToLower(desc)
		if seen[key] {
			continue
		}

		seen[key] = true

		modelOwner := cleanLine(str(obj["owner"]))

		owner, verified := participants[strings.ToLower(modelOwner)]
		if !verified {
			owner = "unassigned"
			unassigned++

			if modelOwner != "" {
				refused = append(refused, fmt.Sprintf(
					"action item %q: owner %q never posted in the transcript — refusing to assign on "+
						"the model's guess; marked unassigned", desc, modelOwner))
			}
		}

		link := ""
		if raw := strings.TrimSpace(str(obj["link"])); raw != "" {
			if ok, reason := isSafeURL(raw); ok {
				link = raw
			} else {
				refused = append(refused, fmt.Sprintf("action item %q: link %q refused — %s", desc, raw, reason))
			}
		}

		items = append(items, actionItem{
			Description:         desc,
			Owner:               owner,
			OwnerVerified:       verified,
			ModelSuggestedOwner: modelOwner,
			Link:                link,
		})

		if len(items) == maxActionItems {
			break
		}
	}

	if items == nil {
		items = []actionItem{}
	}

	return items, unassigned, refused
}

// isSafeURL is the same SSRF-style guardrail research-agent and incident-triage-agent run before any
// outbound-facing URL leaves this repo. It blocks non-http(s) schemes, embedded credentials,
// localhost/internal/metadata hostnames, and literal loopback/private/link-local IPs (the classic
// 169.254.169.254 cloud-metadata SSRF target).
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
