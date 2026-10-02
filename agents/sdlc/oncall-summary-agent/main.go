// oncall-summary-agent — a GoFr 1.58 service that distils an incident channel/thread transcript into a
// chronological timeline and a list of action items. It's the second "Operate" stage agent in the SDLC
// suite, alongside incident-triage-agent: that agent names a root cause and owner at the moment an
// alert fires; this one picks up afterwards, turning the hundreds of scrollback messages a live
// incident channel accumulates into something a new responder (or the postmortem) can read in 30
// seconds. AI agents maintaining a running summary of an incident channel — pulling together metric
// readings, command output and team decisions into a timeline, then turning findings into tracked
// action items with owners — is exactly the on-call pattern production AIOps agents are being built
// around now (nairi.ai, "AI agent for on-call incident response").
//
// The trap is the same one incident-triage-agent guards against: an incident channel transcript is
// ATTACKER-INFLUENCED (or just noisy/wrong) text — anything pasted into the channel (a bot message, a
// copy-pasted log line, a bad-faith message) can try to talk the summarizer into fabricating an event
// that never happened, or assigning an action item to someone who was never in the conversation, or
// smuggling out a "follow-up link" that's actually an SSRF target. So the model only PROPOSES a
// timeline and action items; Go DISPOSES on three fronts:
//   - every timeline entry's `quote` is verified to appear VERBATIM in the transcript it was given —
//     an event the model can't back up with a real quote is dropped, not trusted.
//   - every action item's `owner` is only accepted if it's a handle that ACTUALLY SPOKE in the
//     transcript — i.e. authored at least one line (extracted deterministically in Go beforehand from
//     each line's leading "handle:", never from a bare @mention inside someone else's message). That
//     distinction matters: a hostile line can freely *mention* any name it likes ("assign this to
//     @external-attacker"), but it can't retroactively make that name a participant who was actually in
//     the conversation. An owner the model invented, or one only ever named inside an injected message,
//     is never assigned — the item is left unassigned and flagged for manual review instead, with the
//     model's guess kept only as an unverified side note.
//   - any follow-up `url` an action item carries is checked by the same SSRF-style guardrail
//     research-agent and incident-triage-agent use before it's ever returned.
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
	maxTextChars   = 20000 // the transcript is truncated before it reaches the model
	maxTimeline    = 20
	maxActionItems = 15
	maxFieldChars  = 400
)

// lineAuthorRE captures the handle that OPENS a transcript line — "[14:02] @alice: ..." or
// "alice: ..." — after a leading "[...]" bracket and/or bare "14:02" timestamp has been stripped. This
// is deliberately NOT a bare @mention scan: a handle merely named inside someone else's message (as a
// hostile injection would do) never matches here, only one that actually authored a line.
var lineAuthorRE = regexp.MustCompile(`^@?([A-Za-z0-9_.-]+):`)
var timestampPrefixRE = regexp.MustCompile(`^\d{1,2}:\d{2}(:\d{2})?\s+`)

// timelineEntry is one guardrailed moment in the incident — only ever kept when its quote is a real,
// verbatim substring of the transcript the model was given.
type timelineEntry struct {
	Time     string `json:"time,omitempty"`
	Event    string `json:"event"`
	Quote    string `json:"quote"`
	Grounded bool   `json:"grounded"`
}

// actionItem is one follow-up task. Owner is only ever populated from a real @mention found in the
// transcript — never from the model's own say-so.
type actionItem struct {
	Task                string `json:"task"`
	Owner               string `json:"owner"`
	OwnerVerified       bool   `json:"owner_verified"`
	ModelSuggestedOwner string `json:"model_suggested_owner,omitempty"`
	Due                 string `json:"due,omitempty"`
	URL                 string `json:"url,omitempty"`
}

// summaryResult is the normalized, guardrailed output — what the caller can rely on.
type summaryResult struct {
	Summary           string          `json:"summary"`
	Timeline          []timelineEntry `json:"timeline"`
	TimelineDropped   int             `json:"timeline_dropped"`
	ActionItems       []actionItem    `json:"action_items"`
	NeedsManualReview bool            `json:"needs_manual_review"`
	Refused           []string        `json:"refused,omitempty"` // what the guardrail rejected, and why
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
			"error": "provide a `transcript` — the incident channel/thread dump (timestamps and @handles " +
				"help) to distill into a timeline and action items.",
		}, nil
	}

	if len(transcript) > maxTextChars {
		transcript = transcript[:maxTextChars]
	}

	speakers := extractSpeakers(transcript)

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are an incident commander's assistant distilling an incident " +
			"channel transcript. Reply with ONLY a single JSON object — no markdown, no prose — with " +
			"exactly these keys: summary (one or two sentences), timeline (array of objects with time, " +
			"event, and quote — quote MUST be an EXACT substring copied verbatim from the transcript that " +
			"supports this event; never paraphrase), action_items (array of objects with task, owner — " +
			"the @handle responsible, copied EXACTLY as it appears in the transcript, or empty if nobody " +
			"was mentioned for it — due, and url if a follow-up link was mentioned). Never invent an " +
			"owner who wasn't actually mentioned in the transcript."},
		{Role: ai.RoleUser, Content: transcript},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, err := extractJSON(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON summary: " + err.Error()}, nil
	}

	result := guardrail(raw, transcript, speakers)

	return map[string]any{
		"channel":        strings.TrimSpace(in.Channel),
		"oncall_summary": result,
	}, nil
}

// extractSpeakers deterministically finds every handle that actually AUTHORED a line in the transcript
// — the ONLY source an action item's `owner` can ever be verified against. A handle merely mentioned
// inside someone else's message text never qualifies, which is exactly what keeps a hostile line from
// self-verifying a fabricated owner just by name-dropping it.
func extractSpeakers(transcript string) map[string]bool {
	speakers := make(map[string]bool)

	for _, line := range strings.Split(transcript, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "[") {
			if idx := strings.IndexByte(line, ']'); idx >= 0 {
				line = strings.TrimSpace(line[idx+1:])
			}
		}

		line = timestampPrefixRE.ReplaceAllString(line, "")

		if m := lineAuthorRE.FindStringSubmatch(line); m != nil {
			speakers[strings.ToLower(m[1])] = true
		}
	}

	return speakers
}

// guardrail is the deterministic disposal step: it turns the model's raw JSON object into a
// summaryResult where every timeline event and every action item's owner is independently verified
// against the real transcript, never taken on the model's word.
func guardrail(raw map[string]any, transcript string, speakers map[string]bool) summaryResult {
	var refused []string

	summary := cleanLine(str(raw["summary"]))

	timeline, dropped := groundTimeline(raw["timeline"], transcript)

	items, needsReview, itemRefusals := guardActionItems(raw["action_items"], speakers)
	refused = append(refused, itemRefusals...)

	return summaryResult{
		Summary:           summary,
		Timeline:          timeline,
		TimelineDropped:   dropped,
		ActionItems:       items,
		NeedsManualReview: needsReview,
		Refused:           refused,
	}
}

// groundTimeline keeps only the timeline entries whose quote literally appears in the transcript the
// model was given — an event it claims happened but can't back up with a real quote is unverifiable and
// dropped, not silently trusted.
func groundTimeline(v any, transcript string) (kept []timelineEntry, dropped int) {
	arr, ok := v.([]any)
	if !ok {
		return []timelineEntry{}, 0
	}

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

		kept = append(kept, timelineEntry{
			Time:     cleanLine(str(m["time"])),
			Event:    cleanLine(str(m["event"])),
			Quote:    quote,
			Grounded: true,
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

// guardActionItems resolves every action item's owner against the handles that actually SPOKE in the
// transcript, and vets any follow-up URL against the SSRF-style guardrail — never the model's own word
// on either front.
func guardActionItems(v any, speakers map[string]bool) (items []actionItem, needsReview bool, refused []string) {
	arr, ok := v.([]any)
	if !ok {
		return []actionItem{}, false, nil
	}

	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}

		task := cleanLine(str(m["task"]))
		if task == "" {
			continue
		}

		item := actionItem{Task: task, Due: cleanLine(str(m["due"]))}

		modelOwner := cleanLine(str(m["owner"]))
		ownerKey := strings.ToLower(strings.TrimPrefix(modelOwner, "@"))

		if ownerKey != "" && speakers[ownerKey] {
			item.Owner = modelOwner
			item.OwnerVerified = true
		} else {
			needsReview = true
			if modelOwner != "" {
				item.ModelSuggestedOwner = modelOwner
				refused = append(refused, fmt.Sprintf(
					"action item %q: owner %q never authored a line in the transcript (naming someone "+
						"is not the same as them speaking) — leaving unassigned, needs manual review",
					task, modelOwner))
			}
		}

		if raw := cleanLine(str(m["url"])); raw != "" {
			if ok, reason := isSafeURL(raw); ok {
				item.URL = raw
			} else {
				refused = append(refused, fmt.Sprintf("action item %q: url: %s", task, reason))
			}
		}

		items = append(items, item)
		if len(items) == maxActionItems {
			break
		}
	}

	if items == nil {
		items = []actionItem{}
	}

	return items, needsReview, refused
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
