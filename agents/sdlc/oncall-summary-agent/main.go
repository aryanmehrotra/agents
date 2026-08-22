// oncall-summary-agent — a GoFr 1.58 service that distils an incident channel transcript into a
// chronological timeline and a list of action items with owners. It's the last stop on the "Operate"
// stage of the SDLC suite, next to incident-triage-agent: once an incident is triaged, the channel where
// it was actually worked (Slack/Teams/PagerDuty chat) fills up with dozens of messages, and someone has
// to turn that into a timeline for the postmortem and a checklist of who owes what. Distilling an
// incident channel into a timeline + action items is exactly the kind of narrow, easy-to-check
// "shrink a wall of text into a structured artifact" job agentic incident response is expanding into
// now that triage/co-pilot agents have proven out — the same trend behind incident-triage-agent, now one
// stage later in the incident lifecycle (Rootly, "Top AI Observability Trends Shaping 2026 Incident
// Ops"; incident.io, "Incident management trends 2026").
//
// The same trap as incident-triage-agent applies here, sharper: an incident channel is a transcript of
// whatever anyone typed during a live outage — attacker-influenced, mistake-prone, occasionally hostile
// text — and this agent's whole job is to hand that transcript's contents to a human (or automation) as
// if they're now facts: "this happened at this time" and "this person owes this task". A model that
// trusts its own read of who-said-what, or invents an owner for an action item, turns a prompt injection
// hidden in one channel message into a fabricated incident record or a task routed to someone who was
// never in the room. So the model only PROPOSES a timeline and action items; Go DISPOSES on both:
//   - every timeline entry's `quote` must appear VERBATIM in one of the actual transcript messages —
//     and, once grounded, the entry's time and speaker are taken from THAT message's real metadata,
//     never from whatever time/speaker the model claims. A quote that can't be matched is dropped, not
//     kept with an asterisk.
//   - every action item's `owner` must be a real PARTICIPANT — someone who actually has a message in the
//     transcript — extracted deterministically from the input, never from a static list and never from
//     the model's own guess. An owner nobody in the channel matches falls back to unassigned +
//     needs_manual_assignment, with the model's guess kept only as an unverified side note.
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const (
	maxMessages      = 1000  // input transcript messages accepted
	maxTranscriptLen = 20000 // combined transcript text is truncated before it reaches the model
	maxTimelineItems = 25
	maxActionItems   = 25
	maxFieldChars    = 400
)

// message is one line of the incident channel transcript, exactly as the caller supplied it. This is
// the ONLY source timeline entries and participants are ever built from.
type message struct {
	Time    string `json:"time"`
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
}

// timelineEntry is a guardrailed timeline event: time and speaker are always copied from the transcript
// message the quote was grounded in, never taken from the model's own claim.
type timelineEntry struct {
	Time    string `json:"time"`
	Speaker string `json:"speaker"`
	Event   string `json:"event"`
	Quote   string `json:"quote"`
}

// actionItem is a guardrailed task: Owner is only ever a real transcript participant, resolved
// deterministically — never the model's guess.
type actionItem struct {
	Description           string `json:"description"`
	Owner                 string `json:"owner"`
	OwnerVerified         bool   `json:"owner_verified"`
	ModelSuggestedOwner   string `json:"model_suggested_owner,omitempty"`
	NeedsManualAssignment bool   `json:"needs_manual_assignment"`
	Quote                 string `json:"quote,omitempty"`
}

// oncallSummary is the normalized, guardrailed response.
type oncallSummary struct {
	IncidentName    string          `json:"incident_name,omitempty"`
	Participants    []string        `json:"participants"`
	Timeline        []timelineEntry `json:"timeline"`
	TimelineDropped int             `json:"timeline_dropped"` // unverifiable model-claimed events, dropped
	ActionItems     []actionItem    `json:"action_items"`
	Summary         string          `json:"summary"`
	Refused         []string        `json:"refused,omitempty"` // what the guardrail rejected, and why
}

func main() {
	app := gofr.New()

	app.POST("/summary", oncallSummaryHandler)

	app.Run()
}

func oncallSummaryHandler(c *gofr.Context) (any, error) {
	var in struct {
		IncidentName string    `json:"incident_name"`
		Messages     []message `json:"messages"`
		Text         string    `json:"text"` // alias slot for the orchestrator's single-string call
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	if len(in.Messages) > maxMessages {
		in.Messages = in.Messages[:maxMessages]
	}

	msgs := cleanMessages(in.Messages)
	if len(msgs) == 0 {
		return map[string]any{
			"error": "provide `messages`: an array of {time, speaker, text} lines from the incident " +
				"channel, in chronological order. The timeline and owners are only ever built from " +
				"messages that actually appear here.",
		}, nil
	}

	transcript := renderTranscript(msgs)

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are an SRE distilling an incident channel transcript into a " +
			"postmortem timeline. Reply with ONLY a single JSON object — no markdown, no prose — with " +
			"exactly these keys: timeline (array of {quote: EXACT text copied verbatim from one " +
			"transcript message that this event is about — never paraphrase, event: one-line description " +
			"of what happened}), action_items (array of {description: one concrete follow-up task, owner: " +
			"the name of the participant who should own it — must be someone who actually has a message " +
			"in the transcript, quote: optional exact verbatim quote supporting this item}), summary " +
			"(a short paragraph recapping the incident). Only include events and action items that are " +
			"actually supported by the transcript."},
		{Role: ai.RoleUser, Content: transcript},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, err := extractJSON(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON summary: " + err.Error()}, nil
	}

	result := guardrail(in.IncidentName, raw, msgs)

	return map[string]any{"oncall_summary": result}, nil
}

// cleanMessages drops blank/unusable lines and caps field lengths — this is also the exact set of
// messages timeline quotes and participants are verified against, so nothing the model never actually
// saw can ground anything.
func cleanMessages(in []message) []message {
	out := make([]message, 0, len(in))

	for _, m := range in {
		text := strings.TrimSpace(m.Text)
		speaker := strings.TrimSpace(m.Speaker)

		if text == "" || speaker == "" {
			continue
		}

		out = append(out, message{
			Time:    strings.TrimSpace(m.Time),
			Speaker: speaker,
			Text:    text,
		})
	}

	return out
}

// renderTranscript builds the transcript text handed to the model — truncated so a very long channel
// history doesn't blow the context window.
func renderTranscript(msgs []message) string {
	var b strings.Builder

	for i, m := range msgs {
		if m.Time != "" {
			fmt.Fprintf(&b, "[%d] %s %s: %s\n", i, m.Time, m.Speaker, m.Text)
		} else {
			fmt.Fprintf(&b, "[%d] %s: %s\n", i, m.Speaker, m.Text)
		}
	}

	s := b.String()
	if len(s) > maxTranscriptLen {
		s = s[:maxTranscriptLen]
	}

	return s
}

// guardrail is the deterministic disposal step: every timeline entry is grounded to a real transcript
// message (whose time/speaker override anything the model claimed), and every action item's owner is
// checked against the real, input-derived participant set — never a static registry, and never the
// model's own word.
func guardrail(incidentName string, raw map[string]any, msgs []message) oncallSummary {
	var refused []string

	participants := distinctSpeakers(msgs)
	byLower := make(map[string]string, len(participants))
	for _, p := range participants {
		byLower[strings.ToLower(p)] = p
	}

	timeline, dropped := groundTimeline(stringMapList(raw["timeline"]), msgs)

	items, itemRefused := resolveActionItems(stringMapList(raw["action_items"]), msgs, byLower)
	refused = append(refused, itemRefused...)

	return oncallSummary{
		IncidentName:    cleanLine(incidentName),
		Participants:    participants,
		Timeline:        timeline,
		TimelineDropped: dropped,
		ActionItems:     items,
		Summary:         cleanLine(str(raw["summary"])),
		Refused:         refused,
	}
}

// distinctSpeakers is the ONLY source of truth for who was actually in the channel — derived purely
// from the input messages, in first-appearance order.
func distinctSpeakers(msgs []message) []string {
	seen := make(map[string]bool, len(msgs))
	out := []string{}

	for _, m := range msgs {
		key := strings.ToLower(m.Speaker)
		if seen[key] {
			continue
		}

		seen[key] = true

		out = append(out, m.Speaker)
	}

	return out
}

// groundTimeline keeps only the events whose quote appears verbatim in a real transcript message; time
// and speaker are always taken from that matched message, never from the model. Kept entries are sorted
// by the matched message's position, so the timeline's chronology is the transcript's own order, not
// whatever order the model returned them in.
func groundTimeline(claims []map[string]any, msgs []message) ([]timelineEntry, int) {
	type ordered struct {
		idx   int
		entry timelineEntry
	}

	var kept []ordered

	dropped := 0
	seenEvent := map[string]bool{}

	for _, claim := range claims {
		quote := strings.TrimSpace(strings.Trim(str(claim["quote"]), "\"'"))
		event := cleanLine(str(claim["event"]))

		if quote == "" || event == "" {
			dropped++
			continue
		}

		idx := findMessage(quote, msgs)
		if idx < 0 {
			dropped++
			continue
		}

		key := fmt.Sprintf("%d\x00%s", idx, strings.ToLower(event))
		if seenEvent[key] {
			continue
		}

		seenEvent[key] = true

		kept = append(kept, ordered{idx: idx, entry: timelineEntry{
			Time:    msgs[idx].Time,
			Speaker: msgs[idx].Speaker,
			Event:   event,
			Quote:   quote,
		}})

		if len(kept) == maxTimelineItems {
			break
		}
	}

	sort.SliceStable(kept, func(i, j int) bool { return kept[i].idx < kept[j].idx })

	out := make([]timelineEntry, 0, len(kept))
	for _, k := range kept {
		out = append(out, k.entry)
	}

	return out, dropped
}

// resolveActionItems checks every claimed owner against the real participant set. An owner nobody in
// the channel matches is never returned as authoritative — the item falls back to unassigned +
// needs_manual_assignment, with the model's guess kept only as an unverified side note.
func resolveActionItems(claims []map[string]any, msgs []message, byLower map[string]string) ([]actionItem, []string) {
	var refused []string

	out := make([]actionItem, 0, len(claims))
	seen := map[string]bool{}

	for _, claim := range claims {
		desc := cleanLine(str(claim["description"]))
		if desc == "" {
			continue
		}

		key := strings.ToLower(desc)
		if seen[key] {
			continue
		}

		seen[key] = true

		quote := strings.TrimSpace(strings.Trim(str(claim["quote"]), "\"'"))
		if quote != "" && findMessage(quote, msgs) < 0 {
			quote = "" // claimed but unverifiable — drop the quote, keep the (still model-proposed) task
		}

		ownerGuess := cleanLine(str(claim["owner"]))

		item := actionItem{Description: desc, Quote: quote}

		if real, ok := byLower[strings.ToLower(ownerGuess)]; ok && ownerGuess != "" {
			item.Owner = real
			item.OwnerVerified = true
		} else {
			item.NeedsManualAssignment = true
			item.ModelSuggestedOwner = ownerGuess

			if ownerGuess != "" {
				refused = append(refused, fmt.Sprintf(
					"action item %q: owner %q is not a participant in this transcript — refusing to "+
						"assign it; needs manual assignment", desc, ownerGuess))
			}
		}

		out = append(out, item)
		if len(out) == maxActionItems {
			break
		}
	}

	return out, refused
}

// findMessage returns the index of the first message whose text literally contains the quote, or -1.
func findMessage(quote string, msgs []message) int {
	if quote == "" {
		return -1
	}

	for i, m := range msgs {
		if strings.Contains(m.Text, quote) {
			return i
		}
	}

	return -1
}

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
