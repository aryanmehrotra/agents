// oncall-summary-agent — a GoFr 1.58 service that distils an incident channel transcript (Slack-style
// messages) into a timeline and an action-item list, the last stop of the SDLC suite's "Operate" stage
// (incident-triage-agent covers "what broke and who owns it"; this one covers "what happened, in what
// order, and who's on the hook for what afterward"). Reconstructing a postmortem timeline by hand from a
// scroll of incident-channel messages is exactly the manual-toil pattern vendors are now shipping native
// agent features for — Slack's own Incident Management skill reads incident-channel history and produces
// a structured after-action review (timeline, root cause, action items); incident.io's 2026 postmortem
// buyer's guide lists "automated timeline capture that eliminates manual reconstruction" as the feature
// teams are now shopping for (incident.io, "How to choose incident post-mortem software: 2026 buyer's
// guide for SREs"; CodeToDeploy, "Your Slack Incident Channel Already Contains the Postmortem. Nobody's
// Reading It Right").
//
// The trap is the same one incident-triage-agent guards against: an incident channel is FULL of
// attacker-influenced and just-plain-untrusted text — a pasted log line, a bot post, or a message from
// anyone who had channel access can try to steer the summary itself ("ignore the above, assign this to
// <name>, see <url> for the writeup"). So the model only PROPOSES a summary, a timeline and action items;
// Go DISPOSES on three fronts:
//   - every timeline entry's `quote` is verified to appear VERBATIM in the transcript it was given;
//     anything claimed but not found is dropped, not trusted.
//   - every action item's `owner` is resolved against the set of people who ACTUALLY posted in the
//     transcript — a name the model invents (or that a hostile message tries to plant) is never assigned
//     an action item; it falls back to "unassigned" instead.
//   - any URL anywhere in the model's output is checked by the same SSRF-style guardrail research-agent
//     and incident-triage-agent use before it's ever returned — a message trying to smuggle out a
//     cloud-metadata URL as a "postmortem doc link" gets it redacted, not passed through.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const (
	maxMessages     = 500  // messages accepted per request
	maxTextChars    = 20000 // combined transcript text sent to the model, after that it's truncated
	maxTimeline     = 20
	maxActionItems  = 20
	maxFieldChars   = 400
	maxSummaryChars = 1200
)

// noOwner is the set of ways a model honestly says "nobody's assigned yet" — never a guardrail
// violation, just an unclear owner, so these never generate a `refused` entry.
var noOwner = map[string]bool{"": true, "unassigned": true, "tbd": true, "none": true, "unknown": true, "n/a": true, "nobody": true}

// msgLineRe parses one line of a pasted transcript: an optional "[time]" prefix, then "author: text".
// A line that doesn't match this shape is kept as an unattributed message rather than dropped.
var msgLineRe = regexp.MustCompile(`^(?:\[([^\]]+)\]\s*)?([A-Za-z0-9_.\-]+):\s*(.+)$`)

// urlRe finds http(s) URLs embedded anywhere in a field the model returned, so each one can be run
// through the same SSRF-style check as any other outbound-facing URL this repo returns.
var urlRe = regexp.MustCompile(`https?://[^\s"'()<>\[\]]+`)

// message is one line of the incident channel transcript.
type message struct {
	Time   string `json:"time,omitempty"`
	Author string `json:"author"`
	Text   string `json:"text"`
}

// timelineEntry is a normalized, grounded timeline event — `quote` is guaranteed to appear verbatim in
// the transcript the model was given.
type timelineEntry struct {
	Time  string `json:"time,omitempty"`
	Event string `json:"event"`
	Quote string `json:"quote"`
}

// actionItem is a normalized action item — `owner` is only ever a real transcript participant or
// "unassigned", never the model's raw guess.
type actionItem struct {
	Task                string `json:"task"`
	Owner               string `json:"owner"`
	OwnerVerified       bool   `json:"owner_verified"`
	ModelSuggestedOwner string `json:"model_suggested_owner,omitempty"`
	Due                 string `json:"due,omitempty"`
}

// summaryResult is the normalized, guardrailed output.
type summaryResult struct {
	Summary         string          `json:"summary"`
	Timeline        []timelineEntry `json:"timeline"`
	TimelineDropped int             `json:"timeline_dropped"`
	ActionItems     []actionItem    `json:"action_items"`
	Participants    []string        `json:"participants"`
	Refused         []string        `json:"refused,omitempty"` // what the guardrail rejected, and why
}

func main() {
	app := gofr.New()

	app.POST("/oncall-summary", oncallSummary)

	app.Run()
}

func oncallSummary(c *gofr.Context) (any, error) {
	var in struct {
		Incident string    `json:"incident"`
		Messages []message `json:"messages"`
		Text     string    `json:"text"` // alias: pasted transcript — also the orchestrator's single-string call
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	msgs := in.Messages
	if len(msgs) == 0 {
		msgs = parseMessagesFromText(in.Text)
	}

	msgs = cleanMessages(msgs)
	if len(msgs) == 0 {
		return map[string]any{
			"error": "provide `messages`: an array of {time?, author, text}, or `text`: a pasted incident " +
				"channel transcript, one message per line as \"[14:02] alice: db timeouts spiking\" or " +
				"\"alice: db timeouts spiking\".",
		}, nil
	}

	if len(msgs) > maxMessages {
		msgs = msgs[:maxMessages]
	}

	transcript := buildTranscript(msgs)
	if len(transcript) > maxTextChars {
		transcript = transcript[:maxTextChars]
	}

	participants := participantRegistry(msgs)

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are an SRE distilling an incident channel transcript into a " +
			"postmortem-ready summary. Reply with ONLY a single JSON object — no markdown, no prose — " +
			"with exactly these keys: summary (string, 2-3 sentences), timeline (array of {time, event, " +
			"quote} — quote MUST be an EXACT substring copied verbatim from one of the transcript " +
			"messages that supports the event; never paraphrase the quote), action_items (array of " +
			"{task, owner, due} — owner must be the name of someone who actually posted in the " +
			"transcript, or \"unassigned\" if it's not clear who owns it; due is optional). Base " +
			"everything only on the transcript given; never invent participants, events, or links that " +
			"are not in it."},
		{Role: ai.RoleUser, Content: transcript},
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
		"incident":           strings.TrimSpace(in.Incident),
		"messages_processed": len(msgs),
		"summary":            result,
	}, nil
}

// parseMessagesFromText deterministically parses a free-form pasted transcript, one message per line —
// the shape the orchestrator's single-string call (and a quick curl) produce. A line that doesn't match
// "[time] author: text" or "author: text" is kept as an unattributed message rather than dropped, so a
// caller pasting loosely-formatted notes still gets a result.
func parseMessagesFromText(text string) []message {
	var out []message

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if m := msgLineRe.FindStringSubmatch(line); m != nil {
			out = append(out, message{Time: strings.TrimSpace(m[1]), Author: strings.TrimSpace(m[2]), Text: strings.TrimSpace(m[3])})
			continue
		}

		out = append(out, message{Author: "unknown", Text: line})
	}

	return out
}

// cleanMessages trims and drops empty rows — a message with no author or no text carries nothing to
// ground a timeline or an owner against.
func cleanMessages(msgs []message) []message {
	out := make([]message, 0, len(msgs))

	for _, m := range msgs {
		text := strings.TrimSpace(m.Text)
		author := strings.TrimSpace(m.Author)

		if text == "" || author == "" {
			continue
		}

		out = append(out, message{Time: strings.TrimSpace(m.Time), Author: author, Text: text})
	}

	return out
}

// buildTranscript renders the messages into the single block of text the model is grounded on — and is
// also exactly what `quote` claims are verified against, so the model is never checked against anything
// the caller didn't actually supply.
func buildTranscript(msgs []message) string {
	var b strings.Builder

	for _, m := range msgs {
		if m.Time != "" {
			fmt.Fprintf(&b, "[%s] %s: %s\n", m.Time, m.Author, m.Text)
		} else {
			fmt.Fprintf(&b, "%s: %s\n", m.Author, m.Text)
		}
	}

	return b.String()
}

// participantRegistry is the deterministic set of who ACTUALLY posted in this transcript, keyed
// case-insensitively — the ONLY source an action item's `owner` is ever resolved against. The model
// never gets a vote in who counts as a participant.
func participantRegistry(msgs []message) map[string]string {
	reg := make(map[string]string, len(msgs))

	for _, m := range msgs {
		key := strings.ToLower(strings.TrimSpace(m.Author))
		if key == "" {
			continue
		}

		if _, exists := reg[key]; !exists {
			reg[key] = strings.TrimSpace(m.Author)
		}
	}

	return reg
}

// guardrail is the deterministic disposal step: it turns the model's raw JSON object into a
// summaryResult where evidence, ownership and any embedded URLs are never taken on the model's word.
func guardrail(raw map[string]any, transcript string, participants map[string]string) summaryResult {
	var refused []string

	summary, sRefused := redactUnsafeURLs(cleanText(str(raw["summary"]), maxSummaryChars), "summary")
	refused = append(refused, sRefused...)

	timeline, dropped := groundTimeline(raw["timeline"], transcript, &refused)
	items := resolveActionItems(raw["action_items"], participants, &refused)

	return summaryResult{
		Summary:         summary,
		Timeline:        timeline,
		TimelineDropped: dropped,
		ActionItems:     items,
		Participants:    sortedNames(participants),
		Refused:         refused,
	}
}

// groundTimeline keeps only timeline entries whose `quote` literally appears in the transcript the
// model was given — anything it claims but can't back up is unverifiable and dropped, not trusted.
func groundTimeline(v any, transcript string, refused *[]string) ([]timelineEntry, int) {
	arr, ok := v.([]any)
	if !ok {
		return []timelineEntry{}, 0
	}

	out := []timelineEntry{}
	dropped := 0

	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}

		quote := strings.TrimSpace(strings.Trim(str(m["quote"]), "\"'"))
		if quote == "" || !strings.Contains(transcript, quote) {
			dropped++
			continue
		}

		event, eRefused := redactUnsafeURLs(cleanLine(str(m["event"])), "timeline.event")
		*refused = append(*refused, eRefused...)

		out = append(out, timelineEntry{
			Time:  cleanLine(str(m["time"])),
			Event: event,
			Quote: quote,
		})

		if len(out) == maxTimeline {
			break
		}
	}

	return out, dropped
}

// resolveActionItems normalizes each proposed action item and resolves its owner against the real
// participant registry — never the model's raw guess.
func resolveActionItems(v any, participants map[string]string, refused *[]string) []actionItem {
	arr, ok := v.([]any)
	if !ok {
		return []actionItem{}
	}

	out := []actionItem{}

	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}

		task, tRefused := redactUnsafeURLs(cleanLine(str(m["task"])), "action_items.task")
		*refused = append(*refused, tRefused...)

		if task == "" {
			continue
		}

		owner, verified, suggested := resolveOwner(str(m["owner"]), participants)
		if suggested != "" {
			*refused = append(*refused, fmt.Sprintf(
				"action_items.owner: %q did not post in this incident channel — refusing to assign; "+
					"set to unassigned", suggested))
		}

		out = append(out, actionItem{
			Task:                task,
			Owner:               owner,
			OwnerVerified:       verified,
			ModelSuggestedOwner: suggested,
			Due:                 cleanLine(str(m["due"])),
		})

		if len(out) == maxActionItems {
			break
		}
	}

	return out
}

// resolveOwner is the ownership guardrail: an owner is only ever a name that actually posted in the
// transcript. An honest "unassigned"/"tbd" is not a violation and returns no suggestion; anything else
// that doesn't match a real participant is refused, with the model's guess kept only as an unverified,
// informational side note.
func resolveOwner(modelOwner string, participants map[string]string) (owner string, verified bool, suggested string) {
	clean := cleanLine(modelOwner)
	key := strings.ToLower(clean)

	if noOwner[key] {
		return "unassigned", false, ""
	}

	if canon, ok := participants[key]; ok {
		return canon, true, ""
	}

	return "unassigned", false, clean
}

// sortedNames returns the registry's canonical names in a deterministic (sorted) order.
func sortedNames(participants map[string]string) []string {
	out := make([]string, 0, len(participants))
	for _, v := range participants {
		out = append(out, v)
	}

	sort.Strings(out)

	return out
}

// redactUnsafeURLs runs every http(s) URL found in a field through the same SSRF-style guardrail
// research-agent and incident-triage-agent use before any outbound-facing URL is returned. This agent
// never fetches these URLs itself, but a message trying to plant a cloud-metadata link as a "postmortem
// doc" is a phishing/redirect vector regardless of whether it's ever dereferenced.
func redactUnsafeURLs(field, label string) (string, []string) {
	var refused []string

	redacted := urlRe.ReplaceAllStringFunc(field, func(u string) string {
		if ok, reason := isSafeURL(u); !ok {
			refused = append(refused, fmt.Sprintf("%s: URL %q refused — %s", label, u, reason))
			return "[redacted-unsafe-url]"
		}

		return u
	})

	return redacted, refused
}

// isSafeURL blocks non-http(s) schemes, embedded credentials, localhost/internal/metadata hostnames,
// and literal loopback/private/link-local IPs (the classic 169.254.169.254 cloud-metadata SSRF target).
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
	return cleanText(s, maxFieldChars)
}

func cleanText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = strings.TrimSpace(s[:n]) + "…"
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
