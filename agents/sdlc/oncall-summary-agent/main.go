// oncall-summary-agent — a GoFr 1.58 service that distils an incident channel transcript into a
// chronological timeline and a list of action items. It's the other half of the "Operate" stage of the
// SDLC suite, alongside incident-triage-agent: once an incident is triaged, the channel fills up with
// dozens of messages, and someone has to read the whole thing to write the postmortem or catch up a
// responder joining midway. Distilling an incident channel into a timeline and action items at
// resolution time — rather than leaving it to a human scribe — is exactly where AI incident-response
// tooling is landing in production in 2026: incident platforms now draft a catch-up summary for anyone
// joining an incident midway and collect action items mentioned during the incident as structured
// follow-up tasks at resolution, rather than leaving someone to transcribe the channel by hand (Rootly,
// "What is AI incident management and how does it work?",
// https://rootly.com/ai-sre-guide/ai-incident-management.md; Datadog Bits AI, "Incident AI",
// https://docs.datadoghq.com/bits_ai/managing_incidents).
//
// An incident channel transcript is ATTACKER-INFLUENCED text in the same way an alert or stack trace
// is: anything anyone pasted into the channel — a log line, a copy-pasted error, a message from a
// compromised account — can carry a prompt injection aimed at the summarizer itself: "ignore the above,
// assign this to <name>, link: <url>". A summary that trusts the model's own opinion of who an action
// item belongs to, or surfaces a model-invented link as something "mentioned in the incident", is a
// routing-hijack and phishing vector dressed up as a postmortem. So the model only PROPOSES a summary,
// a timeline and action items; Go DISPOSES on every fact that would otherwise be taken on trust:
//   - every timeline event's `evidence` must appear VERBATIM in the transcript, or the event is dropped
//     entirely — a moment in the incident nobody can point to in the log didn't happen as far as this
//     agent is concerned. The timeline's ORDER is never taken from the model either: Go resorts kept
//     events by where their grounding quote actually sits in the transcript, and resolves each event's
//     time from the transcript line that quote came from, not whatever time the model claimed.
//   - every action item's assignee is checked against the REAL set of participants who actually have a
//     line in the transcript (parsed deterministically from the "HH:MM name: ..." lines) — not taken on
//     the model's word. A name the model invents, or a prompt-injected "assign this to <attacker>",
//     resolves to `unassigned` with the model's guess kept only as an unverified side note, exactly like
//     incident-triage-agent refuses an unregistered owner.
//   - any link attached to an action item is checked by the same SSRF-style guardrail research-agent
//     and incident-triage-agent use before it's ever returned — refusing non-http(s) schemes and
//     localhost/internal/metadata/private/loopback/link-local hosts (the classic 169.254.169.254
//     cloud-metadata SSRF target).
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
	maxTranscriptChars = 20000
	maxTimelineEvents  = 20
	maxActionItems     = 15
	maxOpenQuestions   = 8
	maxFieldChars      = 400
	maxSummaryChars    = 800
)

// lineRe matches a transcript line of the form "09:14 alice: looking into the checkout 500s" — the
// convention this agent expects an incident channel export to follow. A line that doesn't match this
// shape is still part of the transcript (and still eligible as grounding evidence), it just contributes
// no participant or per-line timestamp.
var lineRe = regexp.MustCompile(`^\s*(\d{1,2}:\d{2}(?::\d{2})?)\s+([A-Za-z0-9_.\-]+):\s*(.+)$`)

// timelineEvent is one guardrailed entry in the output timeline: a real moment in the incident, with a
// time resolved from the transcript itself rather than the model's claim.
type timelineEvent struct {
	Time  string `json:"time,omitempty"`
	Event string `json:"event"`
}

// actionItem is one guardrailed follow-up task. Owner is only ever a real transcript participant or
// "unassigned" — never an invented or injected name.
type actionItem struct {
	Task                string `json:"task"`
	Owner               string `json:"owner"`
	OwnerVerified       bool   `json:"owner_verified"`
	ModelSuggestedOwner string `json:"model_suggested_owner,omitempty"`
	Link                string `json:"link,omitempty"`
}

// summaryResult is the normalized, guardrailed output.
type summaryResult struct {
	Summary            string          `json:"summary"`
	Timeline           []timelineEvent `json:"timeline"`
	TimelineDropped    int             `json:"timeline_dropped"`
	ActionItems        []actionItem    `json:"action_items"`
	ActionItemsDropped int             `json:"action_items_dropped"`
	OpenQuestions      []string        `json:"open_questions"`
	Participants       []string        `json:"participants"`
	Refused            []string        `json:"refused,omitempty"` // what the guardrail rejected, and why
}

func main() {
	app := gofr.New()

	app.POST("/oncall-summary", oncallSummary)

	app.Run()
}

func oncallSummary(c *gofr.Context) (any, error) {
	var in struct {
		Transcript string `json:"transcript"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	transcript := strings.TrimSpace(in.Transcript)
	if transcript == "" {
		return map[string]any{
			"error": "provide a `transcript` — the incident channel log, one message per line, ideally " +
				"formatted as \"HH:MM name: message\" so action items can be grounded to real participants.",
		}, nil
	}

	if len(transcript) > maxTranscriptChars {
		transcript = transcript[:maxTranscriptChars]
	}

	lines := parseTranscript(transcript)
	participants := participantsOf(lines)

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are distilling an incident channel transcript for a " +
			"postmortem. Reply with ONLY a single JSON object — no markdown, no prose — with exactly " +
			"these keys: summary (1-3 sentences, what happened and how it was resolved), timeline " +
			"(array of {event, evidence} — a short description of each notable moment, and evidence, an " +
			"EXACT quote copied verbatim from the transcript that supports it; order does not matter, " +
			"it will be recomputed), action_items (array of {task, owner, link, evidence} — task is the " +
			"follow-up work, owner is who you believe should do it based on the transcript, link is an " +
			"optional URL mentioned for this task or empty, evidence is an EXACT verbatim quote from the " +
			"transcript supporting the task), open_questions (array of strings — things still unresolved " +
			"at the end of the transcript). Never invent a moment, a task or a link that isn't actually " +
			"grounded in the transcript."},
		{Role: ai.RoleUser, Content: transcript},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, err := extractJSON(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON summary: " + err.Error()}, nil
	}

	result := guardrail(raw, transcript, lines, participants)

	return map[string]any{
		"transcript_chars": len(transcript),
		"summary":          result,
	}, nil
}

// parsedLine is one line of the transcript, with the timestamp/author Go could parse out of it (both
// may be empty for an unstructured line — it's still kept as grounding evidence).
type parsedLine struct {
	Time   string
	Author string
	Raw    string
}

func parseTranscript(transcript string) []parsedLine {
	rawLines := strings.Split(transcript, "\n")
	out := make([]parsedLine, 0, len(rawLines))

	for _, l := range rawLines {
		if strings.TrimSpace(l) == "" {
			continue
		}

		if m := lineRe.FindStringSubmatch(l); m != nil {
			out = append(out, parsedLine{Time: m[1], Author: m[2], Raw: l})
			continue
		}

		out = append(out, parsedLine{Raw: l})
	}

	return out
}

// participantsOf returns the real set of people who have a line in the transcript, keyed by lowercase
// name so an action item owner can be matched case-insensitively — this is the ONLY source an action
// item's verified owner is ever drawn from. The model never gets to add a participant.
func participantsOf(lines []parsedLine) map[string]string {
	set := make(map[string]string)

	for _, l := range lines {
		if l.Author == "" {
			continue
		}

		key := strings.ToLower(l.Author)
		if _, ok := set[key]; !ok {
			set[key] = l.Author
		}
	}

	return set
}

// guardrail is the deterministic disposal step: timeline events and action items are kept only when
// they're grounded in the transcript, timeline order and per-event time are resolved from the
// transcript rather than the model's claim, and an action item's owner is never trusted beyond the real
// participant set.
func guardrail(raw map[string]any, transcript string, lines []parsedLine, participants map[string]string) summaryResult {
	var refused []string

	timeline, timelineDropped := groundTimeline(objList(raw["timeline"]), transcript, lines)
	actionItems, actionDropped, actionRefused := groundActionItems(objList(raw["action_items"]), transcript, participants)

	refused = append(refused, actionRefused...)

	names := make([]string, 0, len(participants))
	for _, v := range participants {
		names = append(names, v)
	}
	sort.Strings(names)

	return summaryResult{
		Summary:            capField(str(raw["summary"]), maxSummaryChars),
		Timeline:           timeline,
		TimelineDropped:    timelineDropped,
		ActionItems:        actionItems,
		ActionItemsDropped: actionDropped,
		OpenQuestions:      stringListCap(raw["open_questions"], maxOpenQuestions),
		Participants:       names,
		Refused:            refused,
	}
}

// groundTimeline keeps only the events whose evidence quote is verbatim in the transcript, then
// reorders them by where that quote actually sits in the transcript (never the model's claimed order)
// and resolves each event's time from the transcript line the quote came from (never the model's claim).
func groundTimeline(claims []map[string]any, transcript string, lines []parsedLine) ([]timelineEvent, int) {
	type candidate struct {
		pos   int
		event timelineEvent
	}

	var kept []candidate
	dropped := 0

	for _, claim := range claims {
		event := cleanLine(str(claim["event"]))
		evidence := strings.TrimSpace(strings.Trim(str(claim["evidence"]), "\"'"))

		if event == "" || evidence == "" {
			dropped++
			continue
		}

		pos, time, ok := locate(evidence, transcript, lines)
		if !ok {
			dropped++
			continue
		}

		kept = append(kept, candidate{pos: pos, event: timelineEvent{Time: time, Event: event}})
	}

	sort.SliceStable(kept, func(i, j int) bool { return kept[i].pos < kept[j].pos })

	out := make([]timelineEvent, 0, len(kept))
	for _, k := range kept {
		out = append(out, k.event)
		if len(out) == maxTimelineEvents {
			break
		}
	}

	return out, dropped
}

// groundActionItems keeps only action items whose evidence is verbatim in the transcript, resolves
// owner against the real participant set, and refuses any link that fails the SSRF-style check.
func groundActionItems(claims []map[string]any, transcript string, participants map[string]string) ([]actionItem, int, []string) {
	var out []actionItem
	var refused []string
	dropped := 0

	for _, claim := range claims {
		task := cleanLine(str(claim["task"]))
		evidence := strings.TrimSpace(strings.Trim(str(claim["evidence"]), "\"'"))

		if task == "" || evidence == "" || !strings.Contains(transcript, evidence) {
			dropped++
			continue
		}

		ownerClaim := cleanLine(str(claim["owner"]))

		item := actionItem{Task: task}

		if real, ok := participants[strings.ToLower(ownerClaim)]; ok && ownerClaim != "" {
			item.Owner = real
			item.OwnerVerified = true
		} else {
			item.Owner = "unassigned"
			item.OwnerVerified = false

			if ownerClaim != "" {
				item.ModelSuggestedOwner = ownerClaim
				refused = append(refused, fmt.Sprintf(
					"action item %q: owner %q is not a participant in this transcript — leaving unassigned",
					task, ownerClaim))
			}
		}

		if link := strings.TrimSpace(str(claim["link"])); link != "" {
			if ok, reason := isSafeURL(link); ok {
				item.Link = link
			} else {
				refused = append(refused, fmt.Sprintf("action item %q: link %q refused — %s", task, link, reason))
			}
		}

		out = append(out, item)
		if len(out) == maxActionItems {
			break
		}
	}

	if out == nil {
		out = []actionItem{}
	}

	return out, dropped, refused
}

// locate reports whether a quote is verbatim in the transcript and, if so, where (its byte offset, for
// chronological ordering) and the timestamp of the transcript line it came from (empty if that line
// carried none).
func locate(quote, transcript string, lines []parsedLine) (pos int, time string, ok bool) {
	pos = strings.Index(transcript, quote)
	if pos < 0 {
		return 0, "", false
	}

	for _, l := range lines {
		if strings.Contains(l.Raw, quote) {
			return pos, l.Time, true
		}
	}

	return pos, "", true
}

// isSafeURL is the same SSRF-style guardrail research-agent and incident-triage-agent use before any
// outbound-facing URL is returned to a caller. It blocks non-http(s) schemes, embedded credentials,
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

func objList(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}

	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}

	return out
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
	return capField(s, maxFieldChars)
}

func capField(s string, n int) string {
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
