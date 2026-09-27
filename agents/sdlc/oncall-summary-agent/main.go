// oncall-summary-agent — a GoFr 1.58 service that distils an incident channel transcript into a
// timeline, action items, open questions and follow-up links. It's the second "Operate" stage agent
// of the SDLC suite, alongside incident-triage-agent: once an incident is triaged, the channel fills
// with dozens of messages, and someone has to turn that into a postmortem-ready record before the
// context evaporates. Distilling an incident channel into a timeline and action items — so nobody has
// to scroll back through a 200-message thread to write the postmortem — is one of the concrete,
// narrowly-scoped incident-ops workflows teams are handing to agents in 2026, alongside triage and
// root-cause summarization (Rootly, "Top AI Observability Trends Shaping 2026 Incident Ops";
// incident.io, "Incident management trends 2026").
//
// An incident channel is ATTACKER/NOISE-INFLUENCED text: anyone who can post in the channel (including
// a compromised bot or a paste of hostile log output) can try to plant a fake timeline entry, assign an
// action item to someone who was never in the room, or slip in a "follow-up link" that's actually an
// SSRF target. So the model only PROPOSES a timeline, action items, open questions and links; Go
// DISPOSES on every front:
//   - every timeline entry and action item must carry a `quote` that appears VERBATIM in the transcript
//     it was given — an entry the model can't back up with a real quote is dropped, not trusted.
//   - an action item's `owner` is only kept if that name actually posted in the transcript (the
//     participant set is derived deterministically from the parsed messages, never from the model); an
//     owner outside that set is refused and the item falls back to unassigned.
//   - every follow-up `link` must both appear verbatim in the transcript (the model can't invent a new
//     URL) AND pass the same SSRF-style guardrail research-agent and incident-triage-agent use — no
//     non-http(s) scheme, no localhost/internal/metadata host, no literal loopback/private/link-local IP.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const (
	maxMessages      = 500   // cap on parsed transcript messages per request
	maxTextChars     = 20000 // combined transcript text sent to the model is truncated beyond this
	maxTimeline      = 30
	maxActionItems   = 20
	maxOpenQuestions = 15
	maxLinks         = 10
	maxFieldChars    = 400
)

// lineRe parses one transcript line into an optional time, an author token and the rest of the
// message: "14:02 alice: rolling back the deploy" or, without a time, "alice: rolling back the
// deploy". The author must be a single token (no spaces) — the common shape for a chat username.
var lineRe = regexp.MustCompile(`^(?:(\d{1,2}:\d{2}(?::\d{2})?)\s+)?([A-Za-z0-9_.\-]+):\s*(.*)$`)

// message is one line of the incident channel, however it was supplied.
type message struct {
	Time   string `json:"time,omitempty"`
	Author string `json:"author"`
	Text   string `json:"text"`
}

// timelineEntry is one moment in the incident, grounded in a verbatim quote from the transcript.
type timelineEntry struct {
	Time  string `json:"time,omitempty"`
	Event string `json:"event"`
	Quote string `json:"quote"`
}

// actionItem is one follow-up task, with its owner only ever a real transcript participant.
type actionItem struct {
	Owner         string `json:"owner"`
	Action        string `json:"action"`
	Quote         string `json:"quote"`
	OwnerVerified bool   `json:"owner_verified"`
}

// summaryResult is the normalized, guardrailed output.
type summaryResult struct {
	Channel            string          `json:"channel,omitempty"`
	Participants       []string        `json:"participants"`
	Timeline           []timelineEntry `json:"timeline"`
	TimelineDropped    int             `json:"timeline_dropped"`
	ActionItems        []actionItem    `json:"action_items"`
	ActionItemsDropped int             `json:"action_items_dropped"`
	OpenQuestions      []string        `json:"open_questions"`
	Links              []string        `json:"links"`
	LinksDropped       int             `json:"links_dropped"`
	Summary            string          `json:"summary"`
	Refused            []string        `json:"refused,omitempty"` // what the guardrail rejected, and why
}

func main() {
	app := gofr.New()

	app.POST("/oncall-summary", oncallSummary)

	app.Run()
}

func oncallSummary(c *gofr.Context) (any, error) {
	var in struct {
		Channel  string    `json:"channel"`
		Messages []message `json:"messages"`
		Text     string    `json:"text"` // alias: free-form transcript, "[time] author: message" per line
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	msgs := in.Messages
	if len(msgs) == 0 {
		msgs = parseTranscript(in.Text)
	}

	if len(msgs) == 0 {
		return map[string]any{
			"error": "provide `messages`: an array of {time?, author, text}, or `text`: a free-form " +
				"transcript with one message per line, e.g. \"14:02 alice: rolling back the deploy\".",
		}, nil
	}

	if len(msgs) > maxMessages {
		msgs = msgs[:maxMessages]
	}

	transcript := renderTranscript(msgs)
	if len(transcript) > maxTextChars {
		transcript = transcript[:maxTextChars]
	}

	participants := participantSet(msgs)

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are an SRE scribe distilling an incident channel transcript. " +
			"Reply with ONLY a single JSON object — no markdown, no prose — with exactly these keys: " +
			"timeline (array of {time, event, quote} — event is a short factual description of what " +
			"happened, quote is an EXACT substring copied verbatim from one message's text that " +
			"supports it; never paraphrase the quote), action_items (array of {owner, action, quote} — " +
			"owner is whoever in the transcript committed to or was assigned the task, quote is an " +
			"exact verbatim substring from that person's own message), open_questions (string[] — " +
			"unresolved questions raised in the transcript), links (string[] — URLs mentioned in the " +
			"transcript that need follow-up, copied exactly, never invented), summary (a two to three " +
			"sentence overview). Base every field only on the transcript given; return an empty array " +
			"for anything nothing in the transcript supports."},
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
	result.Channel = strings.TrimSpace(in.Channel)

	return map[string]any{
		"messages_processed": len(msgs),
		"summary":            result,
	}, nil
}

// parseTranscript deterministically parses a free-form transcript into messages. A line that doesn't
// match "[time] author: text" is kept with an empty author (never counted as a participant, so an
// action item can never be attributed to it) rather than dropped outright, so it still grounds
// timeline/link quotes.
func parseTranscript(text string) []message {
	var out []message

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		m := lineRe.FindStringSubmatch(line)
		if m == nil {
			out = append(out, message{Text: line})
			continue
		}

		out = append(out, message{Time: m[1], Author: m[2], Text: strings.TrimSpace(m[3])})
	}

	return out
}

// renderTranscript builds the single block of text the model is grounded on — and what every quote and
// link is verified against. The model never sees, and is never checked against, anything the caller
// didn't actually supply.
func renderTranscript(msgs []message) string {
	var b strings.Builder

	for _, m := range msgs {
		switch {
		case m.Time != "" && m.Author != "":
			fmt.Fprintf(&b, "%s %s: %s\n", m.Time, m.Author, m.Text)
		case m.Author != "":
			fmt.Fprintf(&b, "%s: %s\n", m.Author, m.Text)
		default:
			fmt.Fprintf(&b, "%s\n", m.Text)
		}
	}

	return b.String()
}

// participantSet is the deterministic registry of who actually posted in the transcript — the ONLY
// source an action item's `owner_verified` is ever decided from. The model never gets a vote in who
// counts as a participant.
func participantSet(msgs []message) []string {
	seen := map[string]bool{}

	var out []string

	for _, m := range msgs {
		a := strings.ToLower(strings.TrimSpace(m.Author))
		if a == "" || seen[a] {
			continue
		}

		seen[a] = true

		out = append(out, m.Author)
	}

	if out == nil {
		out = []string{}
	}

	return out
}

// guardrail is the deterministic disposal step: it turns the model's raw JSON object into a
// summaryResult where every timeline entry, action item and link is grounded in the real transcript —
// never taken on the model's word alone.
func guardrail(raw map[string]any, transcript string, participants []string) summaryResult {
	var refused []string

	byLower := make(map[string]bool, len(participants))
	for _, p := range participants {
		byLower[strings.ToLower(p)] = true
	}

	timeline, timelineDropped := groundTimeline(raw["timeline"], transcript)

	actionItems, actionDropped, actionRefused := groundActionItems(raw["action_items"], transcript, byLower)
	refused = append(refused, actionRefused...)

	links, linksDropped, linkRefused := groundLinks(raw["links"], transcript)
	refused = append(refused, linkRefused...)

	return summaryResult{
		Participants:       participants,
		Timeline:           timeline,
		TimelineDropped:    timelineDropped,
		ActionItems:        actionItems,
		ActionItemsDropped: actionDropped,
		OpenQuestions:      stringListCap(raw["open_questions"], maxOpenQuestions),
		Links:              links,
		LinksDropped:       linksDropped,
		Summary:            cleanLine(str(raw["summary"])),
		Refused:            refused,
	}
}

// groundTimeline keeps only timeline entries whose quote literally appears in the transcript — an
// event the model can't back up with a real quote is dropped, not trusted.
func groundTimeline(v any, transcript string) (kept []timelineEntry, dropped int) {
	arr, _ := v.([]any)

	for _, e := range arr {
		obj, ok := e.(map[string]any)
		if !ok {
			dropped++
			continue
		}

		quote := strings.TrimSpace(strings.Trim(str(obj["quote"]), "\"'"))
		if quote == "" || !strings.Contains(transcript, quote) {
			dropped++
			continue
		}

		kept = append(kept, timelineEntry{
			Time:  cleanLine(str(obj["time"])),
			Event: cleanLine(str(obj["event"])),
			Quote: quote,
		})

		if len(kept) == maxTimeline {
			break
		}
	}

	if kept == nil {
		kept = []timelineEntry{}
	}

	return kept, dropped
}

// groundActionItems keeps only action items whose quote literally appears in the transcript, and marks
// an owner as verified only when that name actually posted in the transcript — an owner the model
// invented (or that a hostile message tried to reassign the task to) is refused and the item falls back
// to unassigned rather than being routed to someone who was never in the channel.
func groundActionItems(v any, transcript string, participants map[string]bool) (kept []actionItem, dropped int, refused []string) {
	arr, _ := v.([]any)

	for _, e := range arr {
		obj, ok := e.(map[string]any)
		if !ok {
			dropped++
			continue
		}

		quote := strings.TrimSpace(strings.Trim(str(obj["quote"]), "\"'"))
		if quote == "" || !strings.Contains(transcript, quote) {
			dropped++
			continue
		}

		owner := cleanLine(str(obj["owner"]))
		verified := owner != "" && participants[strings.ToLower(owner)]

		if owner != "" && !verified {
			refused = append(refused, fmt.Sprintf(
				"action_items: owner %q never posted in this transcript — refusing to assign, "+
					"falling back to unassigned", owner))
			owner = ""
		}

		kept = append(kept, actionItem{
			Owner:         owner,
			Action:        cleanLine(str(obj["action"])),
			Quote:         quote,
			OwnerVerified: verified,
		})

		if len(kept) == maxActionItems {
			break
		}
	}

	if kept == nil {
		kept = []actionItem{}
	}

	return kept, dropped, refused
}

// groundLinks keeps only links that both appear verbatim in the transcript (the model can't invent a
// new URL) and pass the SSRF-style safety check — the same guardrail research-agent and
// incident-triage-agent apply before ever surfacing a URL, even though this agent never fetches it.
func groundLinks(v any, transcript string) (kept []string, dropped int, refused []string) {
	arr, _ := v.([]any)
	seen := map[string]bool{}

	for _, e := range arr {
		link := strings.TrimSpace(strings.Trim(str(e), "\"'"))
		if link == "" {
			continue
		}

		if !strings.Contains(transcript, link) {
			dropped++
			continue
		}

		if ok, reason := isSafeURL(link); !ok {
			dropped++
			refused = append(refused, fmt.Sprintf("links: %q refused — %s", link, reason))
			continue
		}

		if seen[link] {
			continue
		}

		seen[link] = true

		kept = append(kept, link)
		if len(kept) == maxLinks {
			break
		}
	}

	if kept == nil {
		kept = []string{}
	}

	return kept, dropped, refused
}

// isSafeURL is the same SSRF-style guardrail research-agent and incident-triage-agent run before any
// outbound-facing URL is returned. It blocks non-http(s) schemes, embedded credentials,
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

func stringListCap(v any, n int) []string {
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
