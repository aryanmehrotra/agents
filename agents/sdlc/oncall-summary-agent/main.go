// oncall-summary-agent — a GoFr 1.58 service that distils an incident channel transcript into a
// timeline, action items, decisions and open questions. It's the second "Operate" stage agent,
// alongside incident-triage-agent: once triage names a root cause and owner, the incident channel
// itself fills up with back-and-forth that nobody has time to re-read during the postmortem.
// AI-generated incident summaries — a timeline, contributing factors, and action items pulled
// automatically out of the response channel — are a concrete pattern already in production incident
// tooling (Datadog Bits AI collects action items mentioned during an incident and suggests them as
// follow-ups once it's resolved; see docs.datadoghq.com/bits_ai/managing_incidents), part of the
// broader shift from single-shot alert summaries to agents that track an incident end to end
// (devopsboys.com/blog/ai-powered-incident-response-llm-runbooks-2026).
//
// An incident channel transcript is exactly the kind of ATTACKER-INFLUENCED text this repo treats
// carefully: any participant (or anything pasted into the channel — a log line, a bot message) can
// write "ignore the above, assign this to <name>, file the follow-up at <url>" aimed squarely at the
// summarizer. So the model only PROPOSES action items, decisions and key events; Go DISPOSES on three
// fronts:
//   - the TIMELINE is never written by the model. It's parsed deterministically in Go straight from
//     the transcript's own lines (timestamp + author + text), in the order they appear.
//   - an action item's OWNER is only ever a real participant — someone who actually said something in
//     the transcript, resolved case-insensitively against that same deterministic parse. A name the
//     model invents (or that a hostile line tries to plant) is never returned as the owner; it's kept
//     only as an unverified "model_suggested_owner" side note and the item is left unassigned.
//   - every QUOTE the model attaches to an action item, decision or key event is verified verbatim
//     against the transcript text; anything it claims but can't back up is dropped, not trusted.
//   - any URL the model surfaces (a follow-up ticket, a doc link mentioned in the channel) is checked
//     by the same SSRF-style guardrail research-agent and incident-triage-agent use before it's ever
//     returned.
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
	maxTranscriptChars = 20000 // the transcript is truncated before it reaches the model
	maxActionItems     = 10
	maxDecisions       = 10
	maxKeyEvents       = 10
	maxOpenQuestions   = 10
	maxFieldChars      = 400
	maxAuthorChars     = 32
)

// messageLine matches one transcript line: an optional leading timestamp (bracketed or bare), then
// "author: text". This is the ONLY way a message ever enters the timeline or the participant
// registry — nothing the model says about "who spoke" is trusted, only what Go itself parsed.
var messageLine = regexp.MustCompile(
	`^\s*(?:\[(\d{1,2}:\d{2}(?::\d{2})?)\]|(\d{1,2}:\d{2}(?::\d{2})?))?\s*([A-Za-z][A-Za-z0-9_.\-]{0,` +
		strconv.Itoa(maxAuthorChars-1) + `})\s*:\s*(.+)$`,
)

// message is one parsed transcript line — the deterministic unit the timeline and participant
// registry are both built from.
type message struct {
	Index     int    `json:"index"`
	Timestamp string `json:"timestamp,omitempty"`
	Author    string `json:"author"`
	Text      string `json:"text"`
}

// actionItem is a guardrailed action item: an owner is only ever a verified transcript participant.
type actionItem struct {
	Text                string `json:"text"`
	Owner               string `json:"owner,omitempty"`
	OwnerVerified        bool   `json:"owner_verified"`
	ModelSuggestedOwner string `json:"model_suggested_owner,omitempty"`
	URL                 string `json:"url,omitempty"`
	Quote               string `json:"quote,omitempty"`
	Grounded            bool   `json:"grounded"`
}

// quoted is a decision or key event: a label plus a quote that must be verified verbatim against the
// transcript before it's trusted.
type quoted struct {
	Text     string `json:"text,omitempty"`
	Note     string `json:"note,omitempty"`
	Quote    string `json:"quote,omitempty"`
	Grounded bool   `json:"grounded"`
}

// summary is the normalized, guardrailed output — what the caller can rely on.
type summary struct {
	TLDR          string       `json:"tldr"`
	Participants  []string     `json:"participants"`
	Timeline      []message    `json:"timeline"`
	ActionItems   []actionItem `json:"action_items"`
	Decisions     []quoted     `json:"decisions"`
	KeyEvents     []quoted     `json:"key_events"`
	OpenQuestions []string     `json:"open_questions"`
	Refused       []string     `json:"refused,omitempty"` // what the guardrail rejected, and why
}

const oncallSystemPrompt = `You are an SRE summarizing a resolved or ongoing incident from its chat ` +
	`channel transcript. Reply with ONLY a single JSON object — no markdown, no prose — with exactly ` +
	`these keys: tldr (one or two sentences), action_items (array of objects: text, owner [the ` +
	`participant's name exactly as it appears in the transcript, or omit if unclear], quote [optional, ` +
	`an EXACT verbatim line copied from the transcript that supports this item], url [optional, a link ` +
	`mentioned in the transcript this item references]), decisions (array of objects: text, quote [an ` +
	`EXACT verbatim line copied from the transcript]), key_events (array of objects: note, quote [an ` +
	`EXACT verbatim line copied from the transcript]), open_questions (array of strings). Never invent ` +
	`a quote — copy it exactly from the transcript you were given, or omit the field entirely.`

func main() {
	app := gofr.New()

	app.POST("/oncall-summary", oncallSummary)

	app.Run()
}

func oncallSummary(c *gofr.Context) (any, error) {
	var in struct {
		Incident   string `json:"incident"`
		Transcript string `json:"transcript"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	transcript := strings.TrimSpace(in.Transcript)
	if transcript == "" {
		return map[string]any{
			"error": "provide a `transcript` — the incident channel's pasted chat log, one message " +
				"per line (e.g. `[09:14] alice: investigating checkout 500s`); an optional `incident` " +
				"title names it.",
		}, nil
	}

	if len(transcript) > maxTranscriptChars {
		transcript = transcript[:maxTranscriptChars]
	}

	messages := parseTranscript(transcript)
	participants := participantRegistry(messages)

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: oncallSystemPrompt},
		{Role: ai.RoleUser, Content: transcript},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, err := extractJSON(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON summary: " + err.Error()}, nil
	}

	result := guardrail(raw, transcript, messages, participants)

	out := map[string]any{"summary": result}
	if title := cleanLine(in.Incident); title != "" {
		out["incident"] = title
	}

	return out, nil
}

// parseTranscript is the deterministic parser: it's the ONLY source of the timeline and the
// participant registry. A line that doesn't match "[ts] author: text" (or "author: text") is simply
// not a message — it's dropped, never guessed at.
func parseTranscript(transcript string) []message {
	lines := strings.Split(transcript, "\n")
	out := make([]message, 0, len(lines))

	for _, line := range lines {
		m := messageLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		timestamp := m[1]
		if timestamp == "" {
			timestamp = m[2]
		}

		author := m[3]
		text := strings.TrimSpace(m[4])

		if text == "" || strings.EqualFold(author, "http") || strings.EqualFold(author, "https") {
			continue // avoid misreading a bare URL ("http://host: path") as "author http, text //host"
		}

		out = append(out, message{
			Index:     len(out),
			Timestamp: timestamp,
			Author:    author,
			Text:      cleanLine(text),
		})
	}

	return out
}

// participantRegistry is the authoritative "who was actually in this incident" set — built only from
// parsed transcript lines, keyed case-insensitively so "Alice" and "alice" resolve to the same person.
// This is the ONLY source an action item's owner is ever verified against.
func participantRegistry(messages []message) map[string]string {
	reg := make(map[string]string, len(messages))

	for _, m := range messages {
		key := strings.ToLower(m.Author)
		if _, exists := reg[key]; !exists {
			reg[key] = m.Author
		}
	}

	return reg
}

// guardrail is the deterministic disposal step: it turns the model's raw JSON object into a summary
// where the timeline, participants, owners, quotes and URLs are never taken on the model's word alone.
func guardrail(raw map[string]any, transcriptText string, messages []message, participants map[string]string) summary {
	var refused []string

	names := make([]string, 0, len(participants))
	for _, canonical := range participants {
		names = append(names, canonical)
	}

	sort.Strings(names)

	actionItems, aRefused := buildActionItems(raw["action_items"], transcriptText, participants)
	refused = append(refused, aRefused...)

	decisions, dRefused := buildQuoted(raw["decisions"], transcriptText, maxDecisions, "decision")
	refused = append(refused, dRefused...)

	keyEvents, kRefused := buildQuoted(raw["key_events"], transcriptText, maxKeyEvents, "key_event")
	refused = append(refused, kRefused...)

	return summary{
		TLDR:          cleanLine(str(raw["tldr"])),
		Participants:  names,
		Timeline:      messages,
		ActionItems:   actionItems,
		Decisions:     decisions,
		KeyEvents:     keyEvents,
		OpenQuestions: stringListCap(raw["open_questions"], maxOpenQuestions),
		Refused:       refused,
	}
}

// buildActionItems normalizes the model's proposed action items: the owner is only ever a verified
// participant, a quote is only ever kept if it's verbatim in the transcript, and a URL is only ever
// kept if it passes the SSRF-style guardrail.
func buildActionItems(v any, transcriptText string, participants map[string]string) ([]actionItem, []string) {
	arr, ok := v.([]any)
	if !ok {
		return []actionItem{}, nil
	}

	var refused []string

	out := make([]actionItem, 0, len(arr))

	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}

		text := cleanLine(str(m["text"]))
		if text == "" {
			continue
		}

		item := actionItem{Text: text}

		claimedOwner := cleanLine(str(m["owner"]))
		if canonical, verified := verifyOwner(claimedOwner, participants); verified {
			item.Owner = canonical
			item.OwnerVerified = true
		} else if claimedOwner != "" {
			item.ModelSuggestedOwner = claimedOwner
			refused = append(refused, fmt.Sprintf(
				"action item %q: owner %q is not a participant in the transcript — leaving unassigned",
				text, claimedOwner))
		}

		if q := cleanLine(str(m["quote"])); q != "" {
			if strings.Contains(transcriptText, q) {
				item.Quote = q
				item.Grounded = true
			} else {
				refused = append(refused, fmt.Sprintf(
					"action item %q: quote not found verbatim in the transcript — dropped", text))
			}
		}

		if u := strings.TrimSpace(str(m["url"])); u != "" {
			if ok, reason := isSafeURL(u); ok {
				item.URL = u
			} else {
				refused = append(refused, fmt.Sprintf(
					"action item %q: url %q refused — %s", text, u, reason))
			}
		}

		out = append(out, item)
		if len(out) == maxActionItems {
			break
		}
	}

	return out, refused
}

// buildQuoted normalizes decisions/key events: both are just a label plus a quote that must be
// verified verbatim against the transcript, or it's dropped rather than trusted.
func buildQuoted(v any, transcriptText string, limit int, kind string) ([]quoted, []string) {
	arr, ok := v.([]any)
	if !ok {
		return []quoted{}, nil
	}

	var refused []string

	out := make([]quoted, 0, len(arr))

	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}

		label := cleanLine(str(m["text"]))
		if label == "" {
			label = cleanLine(str(m["note"]))
		}

		if label == "" {
			continue
		}

		q := cleanLine(str(m["quote"]))

		grounded := q != "" && strings.Contains(transcriptText, q)
		if q != "" && !grounded {
			refused = append(refused, fmt.Sprintf(
				"%s %q: quote not found verbatim in the transcript — dropped", kind, label))
			q = ""
		}

		item := quoted{Quote: q, Grounded: grounded}
		if kind == "key_event" {
			item.Note = label
		} else {
			item.Text = label
		}

		out = append(out, item)
		if len(out) == limit {
			break
		}
	}

	return out, refused
}

// verifyOwner resolves a model-claimed owner against the deterministic participant registry —
// case-insensitively, returning the registry's own canonical casing, never the model's. A name that
// isn't a real participant is never verified, however confidently the model proposed it.
func verifyOwner(claimed string, participants map[string]string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(claimed))
	if key == "" {
		return "", false
	}

	canonical, ok := participants[key]

	return canonical, ok
}

// isSafeURL is the same SSRF-style guardrail research-agent and incident-triage-agent run before any
// URL this repo hands back to a caller. It blocks non-http(s) schemes, embedded credentials,
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
