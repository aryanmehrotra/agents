// oncall-summary-agent — a GoFr 1.58 service that distils an incident channel (a pasted chat
// transcript, or a structured list of messages) into a timeline of events and a deduplicated action
// item list with owners. It's the last stop of the "Operate" stage of the SDLC suite: as teams move
// incident response into chat-ops (Slack/Teams war rooms), the channel itself becomes the primary
// record of an incident, and someone still has to turn hundreds of scrollback messages into a timeline
// and a punch list before the postmortem. AI-generated summaries of incident histories and conversation
// threads — turning a war-room channel into a timeline plus a drafted stakeholder update — are already
// a shipped enterprise pattern (Atomicwork, "Top 25+ AI agent use cases for enterprises in 2026":
// https://www.atomicwork.com/blog/ai-agent-use-cases).
//
// An incident channel is ATTACKER-INFLUENCED and self-service text: anyone who can post in the channel
// (or paste a log line into it) can try to steer the summary — assign a task to someone who never
// agreed to it, or smuggle an exfiltration/SSRF URL into an "action item" that a human or automation
// later acts on without re-reading. So the model only PROPOSES a timeline and action items; Go DISPOSES:
//   - every timeline entry's quote must appear VERBATIM in a real message; if it does, the entry's
//     timestamp and author are taken from that REAL message, never from the model's own claim — a model
//     that reorders or reattributes an event can't make that stick.
//   - every action item's owner must be one of the channel's ACTUAL participants (derived from the
//     messages themselves); an owner the model invented falls back to "unassigned" and is kept only as
//     an unverified `suggested_owner` side note.
//   - any http(s) URL appearing in an action item, timeline note, decision or open question is checked
//     by the same SSRF-style guardrail research-agent and incident-triage-agent use before the item is
//     ever returned — an item that references an unsafe URL is dropped whole, not partially redacted.
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
	maxMessages    = 2000 // cap on input messages (structured or parsed from a transcript)
	maxCorpusChars = 20000
	maxTimeline    = 30
	maxActionItems = 20
	maxDecisions   = 15
	maxQuestions   = 15
	maxFieldChars  = 400
)

var severities = map[string]bool{"critical": true, "high": true, "medium": true, "low": true}

var urlRe = regexp.MustCompile(`https?://[^\s<>"'\)]+`)

// transcriptLineRe matches one chat line in the "[ts] author: text" format the agent parses a pasted
// transcript with. A line that doesn't match is treated as a continuation of the previous message
// (real chat pastes routinely wrap a message across lines).
var transcriptLineRe = regexp.MustCompile(`^\[([^\]]+)\]\s*([^:]+):\s*(.*)$`)

// chatMessage is one message in the incident channel — the only source of truth for what was actually
// said, by whom, and when.
type chatMessage struct {
	TS     string `json:"ts"`
	Author string `json:"author"`
	Text   string `json:"text"`
}

// timelineEntry is a guardrailed timeline event: ts/author are always resolved from a real message that
// contains the quote, never taken from the model's own claim.
type timelineEntry struct {
	TS     string `json:"ts"`
	Author string `json:"author"`
	Quote  string `json:"quote"`
	Note   string `json:"note"`
}

// actionItem is a guardrailed action item: owner is only ever a real channel participant or
// "unassigned" — never an identity the model invented.
type actionItem struct {
	Owner          string `json:"owner"`
	Description    string `json:"description"`
	OwnerVerified  bool   `json:"owner_verified"`
	SuggestedOwner string `json:"suggested_owner,omitempty"`
}

// summaryResult is the normalized, guardrailed output — what the caller can rely on.
type summaryResult struct {
	Participants    []string        `json:"participants"`
	MessageCount    int             `json:"message_count"`
	OverallSummary  string          `json:"overall_summary"`
	Severity        string          `json:"severity"`
	Timeline        []timelineEntry `json:"timeline"`
	TimelineDropped int             `json:"timeline_dropped"`
	ActionItems     []actionItem    `json:"action_items"`
	KeyDecisions    []string        `json:"key_decisions"`
	OpenQuestions   []string        `json:"open_questions"`
	Refused         []string        `json:"refused,omitempty"` // what the guardrail rejected, and why
}

func main() {
	app := gofr.New()

	app.POST("/summarize", summarize)

	app.Run()
}

func summarize(c *gofr.Context) (any, error) {
	var in struct {
		Messages []chatMessage `json:"messages"`
		Text     string        `json:"text"` // alias slot for the orchestrator's single-string call
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
			"error": "provide `messages`: an array of {ts, author, text}, or `text`: a pasted transcript " +
				"with one message per line formatted `[ts] author: message`.",
		}, nil
	}

	if len(msgs) > maxMessages {
		msgs = msgs[:maxMessages]
	}

	participants := participantsOf(msgs)
	corpus := corpusOf(msgs)

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are an SRE writing an incident postmortem from a chat " +
			"channel transcript. Reply with ONLY a single JSON object — no markdown, no prose — with " +
			"exactly these keys: overall_summary (2-3 sentences), severity (\"critical\"|\"high\"|" +
			"\"medium\"|\"low\"), timeline (array of {\"quote\": exact text copied verbatim from a " +
			"message, \"note\": one sentence on why it matters} — never paraphrase the quote), " +
			"action_items (array of {\"owner\": must be one of the participants listed, \"description\": " +
			"the task}), key_decisions (string[]), open_questions (string[] — things still unresolved). " +
			"Only use owners from the given participants list; if no one clearly owns a task, use " +
			"\"unassigned\"."},
		{Role: ai.RoleUser, Content: fmt.Sprintf("Participants: %s\n\nTranscript:\n%s",
			strings.Join(participants, ", "), truncate(corpus, maxCorpusChars))},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, err := extractJSON(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON summary: " + err.Error()}, nil
	}

	result := guardrail(raw, msgs, participants)

	return map[string]any{"summary": result}, nil
}

// parseTranscript is the pure, deterministic parser for the orchestrator's/caller's single-string
// `text` alias: one message per `[ts] author: text` line; a line that doesn't match is folded into the
// previous message as a continuation (real chat pastes routinely wrap one message across lines).
func parseTranscript(text string) []chatMessage {
	var msgs []chatMessage

	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" {
			continue
		}

		if m := transcriptLineRe.FindStringSubmatch(line); m != nil {
			msgs = append(msgs, chatMessage{
				TS:     strings.TrimSpace(m[1]),
				Author: strings.TrimSpace(m[2]),
				Text:   strings.TrimSpace(m[3]),
			})

			continue
		}

		if len(msgs) > 0 {
			last := &msgs[len(msgs)-1]
			last.Text = strings.TrimSpace(last.Text + " " + line)
		}
	}

	return msgs
}

// participantsOf derives the ONLY authoritative list of who was actually in the channel — the sole
// source an action item's owner is ever checked against.
func participantsOf(msgs []chatMessage) []string {
	seen := make(map[string]bool, len(msgs))
	out := []string{}

	for _, m := range msgs {
		name := strings.TrimSpace(m.Author)
		if name == "" {
			continue
		}

		key := strings.ToLower(name)
		if seen[key] {
			continue
		}

		seen[key] = true

		out = append(out, name)
	}

	return out
}

// corpusOf builds the transcript text the model is shown and that timeline quotes are verified against.
func corpusOf(msgs []chatMessage) string {
	var b strings.Builder

	for _, m := range msgs {
		fmt.Fprintf(&b, "[%s] %s: %s\n", m.TS, m.Author, m.Text)
	}

	return b.String()
}

// guardrail is the deterministic disposal step: it turns the model's raw JSON object into a
// summaryResult where timeline attribution, action-item ownership and every surfaced URL are never
// taken on the model's word alone.
func guardrail(raw map[string]any, msgs []chatMessage, participants []string) summaryResult {
	var refused []string

	severity := strings.ToLower(strings.TrimSpace(str(raw["severity"])))
	if !severities[severity] {
		severity = "medium"
	}

	timeline, timelineDropped, tRefused := groundTimeline(rawObjects(raw["timeline"], maxTimeline), msgs)
	refused = append(refused, tRefused...)

	items, iRefused := groundActionItems(rawObjects(raw["action_items"], maxActionItems), participants)
	refused = append(refused, iRefused...)

	decisions, dRefused := sanitizeLines(raw["key_decisions"], maxDecisions)
	refused = append(refused, dRefused...)

	questions, qRefused := sanitizeLines(raw["open_questions"], maxQuestions)
	refused = append(refused, qRefused...)

	return summaryResult{
		Participants:    participants,
		MessageCount:    len(msgs),
		OverallSummary:  cleanLine(str(raw["overall_summary"])),
		Severity:        severity,
		Timeline:        timeline,
		TimelineDropped: timelineDropped,
		ActionItems:     items,
		KeyDecisions:    decisions,
		OpenQuestions:   questions,
		Refused:         refused,
	}
}

// groundTimeline keeps only timeline entries whose quote is verbatim in a real message; the entry's
// ts/author are then always taken from THAT message, never from the model's own claim, and its note is
// checked for an unsafe URL before being kept.
func groundTimeline(entries []map[string]any, msgs []chatMessage) (kept []timelineEntry, dropped int, refused []string) {
	seen := make(map[string]bool, len(entries))

	for _, e := range entries {
		quote := strings.TrimSpace(strings.Trim(str(e["quote"]), "\"'"))
		if quote == "" {
			continue
		}

		ts, author, ok := groundQuote(quote, msgs)
		if !ok {
			dropped++
			continue
		}

		if seen[quote] {
			continue
		}

		seen[quote] = true

		note := cleanLine(str(e["note"]))

		if bad, ok := findUnsafeURL(note); ok {
			refused = append(refused, fmt.Sprintf("timeline: dropped entry referencing unsafe URL %q", bad))
			continue
		}

		kept = append(kept, timelineEntry{TS: ts, Author: author, Quote: quote, Note: note})
		if len(kept) == maxTimeline {
			break
		}
	}

	if kept == nil {
		kept = []timelineEntry{}
	}

	return kept, dropped, refused
}

// groundQuote finds the real message a claimed quote came from — the only source a timeline entry's
// ts/author is ever taken from.
func groundQuote(quote string, msgs []chatMessage) (ts, author string, ok bool) {
	for _, m := range msgs {
		if strings.Contains(m.Text, quote) {
			return m.TS, m.Author, true
		}
	}

	return "", "", false
}

// groundActionItems keeps an action item only if its description references no unsafe URL, and resolves
// its owner against the real participant list — an owner the model invented is never trusted, only kept
// as an unverified side note.
func groundActionItems(items []map[string]any, participants []string) (kept []actionItem, refused []string) {
	for _, it := range items {
		description := cleanLine(str(it["description"]))
		if description == "" {
			continue
		}

		if bad, ok := findUnsafeURL(description); ok {
			refused = append(refused, fmt.Sprintf("action_items: dropped item referencing unsafe URL %q", bad))
			continue
		}

		claimed := cleanLine(str(it["owner"]))

		owner, verified := matchParticipant(claimed, participants)

		item := actionItem{Owner: owner, Description: description, OwnerVerified: verified}

		if !verified {
			item.Owner = "unassigned"
			if claimed != "" && !strings.EqualFold(claimed, "unassigned") {
				item.SuggestedOwner = claimed
				refused = append(refused, fmt.Sprintf(
					"action_items: owner %q is not a channel participant — refusing to assign, using "+
						"\"unassigned\"", claimed))
			}
		}

		kept = append(kept, item)
		if len(kept) == maxActionItems {
			break
		}
	}

	if kept == nil {
		kept = []actionItem{}
	}

	return kept, refused
}

// matchParticipant resolves a claimed owner against the real participant list, case-insensitively,
// returning the participant's own casing so the response never echoes back an attacker's spelling.
func matchParticipant(claimed string, participants []string) (string, bool) {
	for _, p := range participants {
		if strings.EqualFold(p, claimed) {
			return p, true
		}
	}

	return "", false
}

// sanitizeLines caps and cleans a model-proposed string list, dropping any line that references an
// unsafe URL rather than returning it with the URL redacted — a partially-scrubbed line is still a line
// an attacker got to shape.
func sanitizeLines(v any, n int) (kept []string, refused []string) {
	arr, ok := v.([]any)
	if !ok {
		return []string{}, nil
	}

	seen := make(map[string]bool, len(arr))

	for _, e := range arr {
		line := cleanLine(str(e))
		if line == "" {
			continue
		}

		if bad, unsafe := findUnsafeURL(line); unsafe {
			refused = append(refused, fmt.Sprintf("dropped line referencing unsafe URL %q", bad))
			continue
		}

		key := strings.ToLower(line)
		if seen[key] {
			continue
		}

		seen[key] = true

		kept = append(kept, line)
		if len(kept) == n {
			break
		}
	}

	if kept == nil {
		kept = []string{}
	}

	return kept, refused
}

// findUnsafeURL extracts every http(s) URL in s and returns the first one that fails the SSRF-style
// guardrail below — the deterministic check every URL this agent might surface has to pass, whether it
// came from a channel message quoted back or a task description the model wrote itself.
func findUnsafeURL(s string) (string, bool) {
	for _, u := range urlRe.FindAllString(s, -1) {
		if ok, _ := isSafeURL(u); !ok {
			return u, true
		}
	}

	return "", false
}

// isSafeURL is the same SSRF-style guardrail research-agent and incident-triage-agent use before any
// URL leaves this system. It blocks non-http(s) schemes, embedded credentials, localhost/internal/
// metadata hostnames, and literal loopback/private/link-local IPs (the classic 169.254.169.254
// cloud-metadata SSRF target).
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

// rawObjects coerces a model value into a capped slice of JSON objects, ignoring anything else.
func rawObjects(v any, n int) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}

	out := make([]map[string]any, 0, len(arr))

	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
			if len(out) == n {
				break
			}
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

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
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
