// spec-agent — a GoFr 1.58 service that turns a raw ticket or issue into a structured engineering
// spec: a one-line summary, what's in and out of scope, testable acceptance criteria, risks, a task
// breakdown, and the open questions that still block the work. Turning a vague request into a
// scoped, agreed spec is the first stage of the software-development lifecycle, and the one where an
// LLM most reliably saves a human time — it reads the ticket, drafts the shape, and a person edits.
//
// But a spec is only useful if it is actually a spec. A model asked for one will happily return prose,
// invent a shape, leave the acceptance criteria empty, or bury a task list inside a sentence. So the
// model only proposes — every section it returns is normalized deterministically in Go: blanks and
// duplicates are dropped, each task must carry a non-empty title, lists are capped, and the result is
// gated on a hard rule (a real summary, at least one acceptance criterion, at least one task) before
// it is called complete. The caller gets a spec it can hand to a person, with an honest note about
// what the model failed to fill in — never the model's raw guess dressed up as a plan.
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
	maxTicketChars = 20000 // a ticket longer than this is truncated before it reaches the model
	maxItems       = 12    // cap on each string list (scope, criteria, risks, questions)
	maxTasks       = 20    // cap on the task breakdown
	maxFieldChars  = 400   // cap on a single normalized line, so one runaway item can't dominate
)

// task is one entry in the breakdown. Title is required (an untitled task is dropped); Detail is
// optional context the model added.
type task struct {
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// spec is the normalized, validated output shape — what the caller can rely on. Every field here has
// already been through the Go guardrail; the model's raw object is never returned as-is.
type spec struct {
	Title              string   `json:"title"`
	Summary            string   `json:"summary"`
	InScope            []string `json:"in_scope"`
	OutOfScope         []string `json:"out_of_scope"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Risks              []string `json:"risks"`
	Tasks              []task   `json:"tasks"`
	OpenQuestions      []string `json:"open_questions"`
}

func main() {
	app := gofr.New()

	app.POST("/spec", buildSpec)

	app.Run()
}

func buildSpec(c *gofr.Context) (any, error) {
	var in struct {
		Ticket string `json:"ticket"`
		Title  string `json:"title"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	ticket := strings.TrimSpace(in.Ticket)
	if ticket == "" {
		return map[string]any{
			"error": "provide a `ticket` (the issue / feature request to turn into a spec); an " +
				"optional `title` seeds the spec's title.",
		}, nil
	}

	if len(ticket) > maxTicketChars {
		ticket = ticket[:maxTicketChars]
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a precise engineering lead who turns a raw ticket into " +
			"a structured spec. Reply with ONLY a single JSON object — no markdown, no prose — with " +
			"exactly these keys: title (string), summary (one sentence), in_scope (string[]), " +
			"out_of_scope (string[]), acceptance_criteria (string[] — each a concrete, testable " +
			"condition), risks (string[]), tasks (array of {title, detail}), open_questions (string[] — " +
			"things you need answered before this can be built). Base everything on the ticket; do not " +
			"invent requirements it doesn't imply. Use [] for a section the ticket gives you nothing for."},
		{Role: ai.RoleUser, Content: seedTitle(in.Title) + "Ticket:\n" + ticket},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, err := extractJSON(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON spec: " + err.Error()}, nil
	}

	s := normalize(in.Title, raw)
	complete, reasons := gate(s)

	return map[string]any{
		"ticket_chars":       len(ticket),
		"spec":               s,
		"complete":           complete, // false when the guardrail's minimum bar isn't met
		"incomplete_reasons": reasons,  // what the model failed to produce, so a human knows what to add
	}, nil
}

// seedTitle threads a caller-supplied title into the prompt when present, so the model anchors the
// spec on it rather than inventing one.
func seedTitle(title string) string {
	if t := strings.TrimSpace(title); t != "" {
		return "Proposed title: " + t + "\n\n"
	}

	return ""
}

// normalize is the guardrail: it turns the model's raw JSON object — whatever shape it actually came
// back as — into a clean, typed spec. Every list is trimmed, de-blanked, de-duplicated and capped;
// every task must have a title; the title falls back to the caller's when the model omitted it.
func normalize(callerTitle string, raw map[string]any) spec {
	s := spec{
		Title:              firstNonEmpty(cleanLine(str(raw["title"])), strings.TrimSpace(callerTitle)),
		Summary:            cleanLine(str(raw["summary"])),
		InScope:            stringList(raw["in_scope"]),
		OutOfScope:         stringList(raw["out_of_scope"]),
		AcceptanceCriteria: stringList(raw["acceptance_criteria"]),
		Risks:              stringList(raw["risks"]),
		Tasks:              taskList(raw["tasks"]),
		OpenQuestions:      stringList(raw["open_questions"]),
	}

	return s
}

// gate enforces the minimum bar for a spec to count as usable: a real summary, at least one
// acceptance criterion, and at least one task. It returns whether the spec cleared the bar and a
// human-readable list of what's missing — the honest note that goes back with a thin spec.
func gate(s spec) (complete bool, reasons []string) {
	reasons = []string{}

	if s.Summary == "" {
		reasons = append(reasons, "no summary")
	}

	if len(s.AcceptanceCriteria) == 0 {
		reasons = append(reasons, "no acceptance criteria — nothing to verify the work against")
	}

	if len(s.Tasks) == 0 {
		reasons = append(reasons, "no task breakdown")
	}

	return len(reasons) == 0, reasons
}

// stringList coerces the model's value for a list section into clean strings: it accepts a JSON array
// (rendering each scalar element as text), trims and drops blanks, de-duplicates case-insensitively
// while keeping first-seen order, truncates over-long lines, and caps the count.
func stringList(v any) []string {
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
		if len(out) == maxItems {
			break
		}
	}

	return out
}

// taskList coerces the model's "tasks" value into titled tasks. It accepts either an array of
// {title, detail} objects or an array of plain strings (a model that flattened the breakdown); a task
// with no usable title is dropped, titles are de-duplicated, and the list is capped.
func taskList(v any) []task {
	arr, ok := v.([]any)
	if !ok {
		return []task{}
	}

	out := make([]task, 0, len(arr))
	seen := make(map[string]bool, len(arr))

	for _, e := range arr {
		t := oneTask(e)
		if t.Title == "" {
			continue
		}

		key := strings.ToLower(t.Title)
		if seen[key] {
			continue
		}

		seen[key] = true

		out = append(out, t)
		if len(out) == maxTasks {
			break
		}
	}

	return out
}

// oneTask normalizes a single task element, whether it arrived as an object or a bare string.
func oneTask(e any) task {
	switch t := e.(type) {
	case string:
		return task{Title: cleanLine(t)}
	case map[string]any:
		return task{
			Title:  cleanLine(str(t["title"])),
			Detail: cleanLine(str(t["detail"])),
		}
	default:
		return task{}
	}
}

// cleanLine trims a line and truncates it to maxFieldChars so one runaway item can't dominate output.
func cleanLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxFieldChars {
		s = strings.TrimSpace(s[:maxFieldChars]) + "…"
	}

	return s
}

// str renders a JSON scalar (string, number, bool) as text, so a list that mixed a number into its
// strings still yields a usable line instead of being dropped.
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
