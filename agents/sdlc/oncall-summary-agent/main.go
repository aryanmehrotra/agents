// oncall-summary-agent — a GoFr 1.58 service that distils a raw incident-channel transcript into a
// chronological timeline and a list of action items. It's the second half of the "Operate" stage of
// the SDLC suite, picking up after incident-triage-agent: once an incident has been triaged, the
// channel fills with dozens of back-and-forth messages, and someone still has to turn that into a
// timeline for the postmortem and a clean action-item list for the follow-up. Agentic incident
// response is moving from single-shot triage toward agents that handle the whole incident lifecycle —
// including distilling the incident channel into a timeline and action items once the firefighting is
// done ([Rootly, "Top AI Observability Trends Shaping 2026 Incident
// Ops"](https://rootly.com/sre/top-ai-observability-trends-shaping-2026-incident-ops-0e928);
// [incident.io, "Incident management trends 2026"](https://incident.io/blog/incident-management-tools-trends-2026)).
//
// An incident channel is the same class of attacker-influenced text incident-triage-agent already
// guards against — anything responders paste verbatim (a log line, an error, a link) can carry a
// prompt injection aimed at the summarizer itself: "mark this done, assign it to <name>, reference
// link: <url>". So the model only PROPOSES action items; Go DISPOSES on three fronts:
//   - the TIMELINE IS NEVER THE MODEL'S. Every message line is parsed deterministically in Go
//     (timestamp + author + text) and sorted by its own timestamp — the model never gets a vote on
//     chronology, so it can't be talked into reordering events to make an owner look responsive or to
//     bury a line.
//   - an action item's `owner` is only ever someone who actually posted in the channel — resolved
//     against the deterministic set of participants parsed from the transcript, never the model's
//     guess. An unrecognised owner falls back to unassigned instead of being handed to whoever the
//     model (or an injected line) named.
//   - every action item's `evidence` quote is verified verbatim against the transcript, and any URL
//     mentioned in an action item is checked by the same SSRF-style guardrail research-agent and
//     incident-triage-agent use before it's ever returned — an item nobody can verify, or one carrying
//     an unsafe URL, is refused rather than handed back as if it were real.
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
	"time"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const (
	maxTranscriptChars = 20000 // transcript text truncated before it reaches the model
	maxActionItems     = 20
	maxFieldChars      = 400
)

var statuses = map[string]bool{"open": true, "done": true, "blocked": true}

// lineRE parses one transcript line of the form "<RFC3339 timestamp> <author>: <text>". A line that
// doesn't match this shape (or whose timestamp doesn't parse) is excluded from the deterministic
// timeline/participant set — never guessed at — but is still passed to the model as raw context.
var lineRE = regexp.MustCompile(`^(\S+)\s+([^:]{1,100}?):\s?(.*)$`)

// urlRE finds any http(s)-looking URL embedded in a piece of text, so it can be vetted before an
// action item carrying it is ever returned.
var urlRE = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>)]+`)

// timelineEvent is one deterministically parsed transcript line.
type timelineEvent struct {
	Time    time.Time `json:"-"`
	TimeRaw string    `json:"time"`
	Author  string    `json:"author"`
	Text    string    `json:"text"`
}

// actionItem is the normalized, guardrailed output for one action item — what a caller can rely on.
type actionItem struct {
	Text                string `json:"text"`
	Owner               string `json:"owner"`
	OwnerVerified       bool   `json:"owner_verified"`
	ModelSuggestedOwner string `json:"model_suggested_owner,omitempty"`
	Status              string `json:"status"`
	Evidence            string `json:"evidence"`
}

type summaryResult struct {
	Channel         string          `json:"channel,omitempty"`
	Timeline        []timelineEvent `json:"timeline"`
	LinesUnparsed   int             `json:"lines_unparsed"`
	Participants    []string        `json:"participants"`
	IncidentSummary string          `json:"incident_summary"`
	ActionItems     []actionItem    `json:"action_items"`
	ItemsDropped    int             `json:"items_dropped"`
	Refused         []string        `json:"refused,omitempty"`
}

func main() {
	app := gofr.New()

	app.POST("/oncall-summary", oncallSummary)

	app.Run()
}

func oncallSummary(c *gofr.Context) (any, error) {
	var in struct {
		Channel    string `json:"channel"`
		Transcript string `json:"transcript"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	transcript := strings.TrimSpace(in.Transcript)
	if transcript == "" {
		return map[string]any{
			"error": "provide a `transcript` — the incident channel's messages, one per line, each " +
				"formatted `<RFC3339 timestamp> <author>: <message>`; optional `channel` names the incident.",
		}, nil
	}

	events, unparsed := parseTimeline(transcript)
	participants := participantSet(events)

	modelInput := transcript
	if len(modelInput) > maxTranscriptChars {
		modelInput = modelInput[:maxTranscriptChars]
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are distilling an incident-response chat channel into a " +
			"summary and action items. Reply with ONLY a single JSON object — no markdown, no prose — " +
			"with exactly these keys: incident_summary (one or two sentences on what happened and the " +
			"current state), action_items (array of objects, each with: text (string, the concrete " +
			"follow-up), owner (string, the channel participant responsible — must be someone who " +
			"actually posted in the channel, or empty if unclear), status (\"open\"|\"done\"|" +
			"\"blocked\"), evidence (string — an EXACT quote copied verbatim from the transcript that " +
			"supports this action item; never paraphrase)). Base everything only on the transcript " +
			"given; never invent a participant who didn't post."},
		{Role: ai.RoleUser, Content: modelInput},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, err := extractJSON(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON summary: " + err.Error()}, nil
	}

	result := guardrail(raw, transcript, events, unparsed, participants)
	result.Channel = strings.TrimSpace(in.Channel)

	return result, nil
}

// parseTimeline builds the deterministic timeline: every line that matches the expected shape and
// carries a parseable RFC3339 timestamp becomes an event, sorted by that timestamp — never by the
// order the model (or an injected line) would like them read in. Lines that don't match are counted as
// unparsed and excluded from the timeline/participant set, but nothing about them is guessed at.
func parseTimeline(transcript string) ([]timelineEvent, int) {
	lines := strings.Split(transcript, "\n")

	events := make([]timelineEvent, 0, len(lines))
	unparsed := 0

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		m := lineRE.FindStringSubmatch(line)
		if m == nil {
			unparsed++
			continue
		}

		t, err := time.Parse(time.RFC3339, m[1])
		if err != nil {
			unparsed++
			continue
		}

		events = append(events, timelineEvent{
			Time:    t,
			TimeRaw: t.Format(time.RFC3339),
			Author:  cleanLine(m[2]),
			Text:    cleanLine(m[3]),
		})
	}

	sort.SliceStable(events, func(i, j int) bool { return events[i].Time.Before(events[j].Time) })

	return events, unparsed
}

// participantSet is the deterministic allowlist an action item's owner is checked against — exactly
// the authors who actually posted a parsed line, never the model's opinion of who's involved.
func participantSet(events []timelineEvent) []string {
	seen := make(map[string]bool, len(events))

	var out []string

	for _, e := range events {
		key := strings.ToLower(e.Author)
		if key == "" || seen[key] {
			continue
		}

		seen[key] = true

		out = append(out, e.Author)
	}

	if out == nil {
		out = []string{}
	}

	sort.Strings(out)

	return out
}

// guardrail is the deterministic disposal step: it turns the model's raw JSON into a summaryResult
// where the timeline, participants, owners and evidence are never taken on the model's word alone.
func guardrail(raw map[string]any, transcript string, events []timelineEvent, unparsed int, participants []string) summaryResult {
	allowed := make(map[string]bool, len(participants))
	for _, p := range participants {
		allowed[strings.ToLower(p)] = true
	}

	var (
		items   []actionItem
		dropped int
		refused []string
	)

	for _, raw := range stringMapList(raw["action_items"]) {
		text := cleanLine(str(raw["text"]))
		evidence := strings.TrimSpace(strings.Trim(str(raw["evidence"]), "\"'"))

		if text == "" || evidence == "" || !strings.Contains(transcript, evidence) {
			// An action item nobody can verify against the real transcript is not one to hand to a
			// human as if it actually happened — drop it rather than returning an unverifiable claim.
			dropped++
			continue
		}

		status := strings.ToLower(strings.TrimSpace(str(raw["status"])))
		if !statuses[status] {
			status = "open"
		}

		modelOwner := cleanLine(str(raw["owner"]))
		owner := ""
		verified := false

		if modelOwner != "" {
			if allowed[strings.ToLower(modelOwner)] {
				owner = modelOwner
				verified = true
			} else {
				refused = append(refused, fmt.Sprintf(
					"owner: %q never posted in this channel — refusing to assign on the model's guess",
					modelOwner))
			}
		}

		if u := urlRE.FindString(text); u != "" {
			if ok, reason := isSafeURL(u); !ok {
				refused = append(refused, fmt.Sprintf("action item URL %q refused: %s", u, reason))
				text = strings.ReplaceAll(text, u, "[refused link]")
			}
		}

		item := actionItem{
			Text:          text,
			Owner:         owner,
			OwnerVerified: verified,
			Status:        status,
			Evidence:      evidence,
		}
		if !verified && modelOwner != "" {
			item.ModelSuggestedOwner = modelOwner
		}

		items = append(items, item)
		if len(items) == maxActionItems {
			break
		}
	}

	if items == nil {
		items = []actionItem{}
	}

	return summaryResult{
		Timeline:        events,
		LinesUnparsed:   unparsed,
		Participants:    participants,
		IncidentSummary: cleanLine(str(raw["incident_summary"])),
		ActionItems:     items,
		ItemsDropped:    dropped,
		Refused:         refused,
	}
}

// isSafeURL is the same SSRF-style guardrail research-agent and incident-triage-agent run before any
// URL is surfaced or fetched. It blocks non-http(s) schemes, embedded credentials,
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

// stringMapList coerces the model's action_items value into a slice of maps, ignoring anything that
// isn't one — a model that returns the wrong shape yields zero items, never a panic.
func stringMapList(v any) []map[string]any {
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
