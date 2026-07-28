// estimation-agent — a GoFr 1.58 service that sizes a piece of work. Give it a task breakdown (for
// example the one spec-agent produced) or a raw description, and it returns a point estimate with an
// optimistic/likely/pessimistic range, and — if you tell it the team's velocity — a duration in days.
// Estimation is the second stage of the software-development lifecycle, and the one teams most want a
// second opinion on before they commit to a sprint.
//
// The division of labour is the whole point. A language model is genuinely useful at the *judgment*
// part — sizing one task relative to another and saying how confident it is — but it cannot be trusted
// with the *arithmetic*: models miscount, drop tasks from a sum, and confidently return a total that
// doesn't match their own per-task sizes. So the model only proposes a size and a confidence per task;
// every number after that is computed deterministically in Go from a fixed size→points table. The
// model's own "total", if it volunteers one, is recorded and then ignored — the estimate you get back
// is arithmetic Go did, over judgments the model made.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const (
	maxTextChars  = 20000
	maxTasks      = 30
	defSprintDays = 10 // working days per sprint, when the caller doesn't say
)

// sizePoints is the fixed, deterministic ladder the model's T-shirt sizes map to. A Fibonacci-ish
// scale is standard for relative estimation — the gaps widen because larger work is inherently less
// certain. The model never sees these numbers; it only picks a size, and Go does the mapping.
var sizePoints = map[string]int{
	"xs": 1, "s": 2, "m": 3, "l": 5, "xl": 8, "xxl": 13,
}

// confBand turns a task's confidence into how wide its own point range is: a low-confidence task
// swings ±50% around its nominal points, a high-confidence one only ±10%. Summing these per-task
// bands (rather than applying one blanket fudge factor) means the range reflects *which* tasks are
// uncertain, not just how many there are.
var confBand = map[string]float64{
	"high": 0.10, "medium": 0.25, "low": 0.50,
}

// sizedTask is one task after the guardrail has run: the model supplied Title/Size/Confidence/
// Rationale; Points and the Low/High band are filled in deterministically by Go.
type sizedTask struct {
	Title      string  `json:"title"`
	Size       string  `json:"size"`
	Confidence string  `json:"confidence"`
	Rationale  string  `json:"rationale,omitempty"`
	Points     int     `json:"points"`
	LowPoints  float64 `json:"low_points"`
	HighPoints float64 `json:"high_points"`
}

// invalidTask is a task the model returned with a size Go can't map to points — reported, never
// silently counted as zero.
type invalidTask struct {
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

func main() {
	app := gofr.New()

	app.POST("/estimate", estimate)

	app.Run()
}

func estimate(c *gofr.Context) (any, error) {
	var in struct {
		Tasks      []map[string]any `json:"tasks"`       // e.g. spec-agent's tasks: [{title, detail}]
		Text       string           `json:"text"`        // or a raw description to break down + size
		Velocity   float64          `json:"velocity"`    // team points per sprint (optional → duration)
		SprintDays float64          `json:"sprint_days"` // working days per sprint (optional)
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	titles := taskTitles(in.Tasks)
	text := strings.TrimSpace(in.Text)

	if len(titles) == 0 && text == "" {
		return map[string]any{
			"error": "provide either `tasks` (an array of {title, detail} — e.g. spec-agent's output) " +
				"or `text` (a description to break down and size); optional `velocity` (points per " +
				"sprint) turns the point estimate into a duration.",
		}, nil
	}

	if len(text) > maxTextChars {
		text = text[:maxTextChars]
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are an experienced engineer estimating work. Assign each " +
			"task a RELATIVE size and a confidence — do NOT add anything up, arithmetic is handled for " +
			"you. Reply with ONLY a JSON array; each element is {\"title\": string, \"size\": one of " +
			"\"XS\",\"S\",\"M\",\"L\",\"XL\",\"XXL\", \"confidence\": one of \"high\",\"medium\",\"low\", " +
			"\"rationale\": a short reason}. Size is relative effort/complexity (XS≈trivial, XXL≈a large " +
			"multi-day effort); confidence is how sure you are of that size. No prose, no totals."},
		{Role: ai.RoleUser, Content: taskPrompt(titles, text)},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	arr, claimedTotal, err := extractTaskArray(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON array of sized tasks: " + err.Error()}, nil
	}

	sized, invalid := normalize(arr)
	est := aggregate(sized, in.Velocity, sprintDays(in.SprintDays))

	out := map[string]any{
		"tasks":    sized,
		"invalid":  invalid,
		"estimate": est,
		"note": "points are computed in Go from a fixed size→points table; the model only picks each " +
			"task's size and confidence, and its own arithmetic is never trusted.",
		"complete": len(sized) > 0,
	}

	// Surface the guardrail concretely: if the model volunteered a total, show it next to the one Go
	// actually computed, so a mismatch is visible rather than hidden.
	if claimedTotal != nil {
		out["model_claimed_total"] = *claimedTotal
	}

	return out, nil
}

// taskTitles pulls a clean title from each provided task object, tolerating either {title, detail} or
// a bare string that arrived as a map value.
func taskTitles(tasks []map[string]any) []string {
	out := make([]string, 0, len(tasks))

	for _, t := range tasks {
		title := strings.TrimSpace(str(t["title"]))
		if title == "" {
			continue
		}

		if d := strings.TrimSpace(str(t["detail"])); d != "" {
			title += " — " + d
		}

		out = append(out, title)
		if len(out) == maxTasks {
			break
		}
	}

	return out
}

// taskPrompt builds the user turn: size a provided list of tasks, or break a raw description into
// tasks and size those.
func taskPrompt(titles []string, text string) string {
	if len(titles) > 0 {
		var b strings.Builder
		b.WriteString("Size each of these tasks:\n")

		for i, t := range titles {
			fmt.Fprintf(&b, "%d. %s\n", i+1, t)
		}

		return b.String()
	}

	return "Break this work into concrete tasks and size each one:\n\n" + text
}

// normalize is the guardrail: it turns the model's raw array into sized tasks with Go-computed
// points. A task whose size isn't in the ladder is moved to `invalid` (never counted as zero);
// confidence defaults to medium when the model gave something unrecognized, since a missing
// confidence shouldn't discard an otherwise-usable size.
func normalize(arr []map[string]any) (sized []sizedTask, invalid []invalidTask) {
	sized = []sizedTask{}
	invalid = []invalidTask{}

	for _, e := range arr {
		title := strings.TrimSpace(str(e["title"]))
		if title == "" {
			continue
		}

		size := strings.ToLower(strings.TrimSpace(str(e["size"])))

		pts, ok := sizePoints[size]
		if !ok {
			invalid = append(invalid, invalidTask{
				Title:  title,
				Reason: fmt.Sprintf("unrecognized size %q (want XS/S/M/L/XL/XXL)", str(e["size"])),
			})

			continue
		}

		conf := strings.ToLower(strings.TrimSpace(str(e["confidence"])))

		band, ok := confBand[conf]
		if !ok {
			conf = "medium"
			band = confBand["medium"]
		}

		sized = append(sized, sizedTask{
			Title:      title,
			Size:       strings.ToUpper(size),
			Confidence: conf,
			Rationale:  strings.TrimSpace(str(e["rationale"])),
			Points:     pts,
			LowPoints:  round1(float64(pts) * (1 - band)),
			HighPoints: round1(float64(pts) * (1 + band)),
		})

		if len(sized) == maxTasks {
			break
		}
	}

	return sized, invalid
}

// aggregate is the arithmetic the model is not trusted with: sum the per-task nominal points and the
// per-task confidence bands into an optimistic/likely/pessimistic point range, and — when a velocity
// is given — divide through it to get a duration in working days.
func aggregate(sized []sizedTask, velocity, sprintDays float64) map[string]any {
	var likely, low, high float64

	for _, t := range sized {
		likely += float64(t.Points)
		low += t.LowPoints
		high += t.HighPoints
	}

	points := map[string]any{
		"optimistic":  round1(low),
		"likely":      round1(likely),
		"pessimistic": round1(high),
	}

	est := map[string]any{"points": points}

	if velocity > 0 {
		est["duration"] = map[string]any{
			"optimistic_days":  round1(low / velocity * sprintDays),
			"likely_days":      round1(likely / velocity * sprintDays),
			"pessimistic_days": round1(high / velocity * sprintDays),
			"assumes":          fmt.Sprintf("%g points/sprint, %g days/sprint", velocity, sprintDays),
		}
	}

	return est
}

func sprintDays(d float64) float64 {
	if d > 0 {
		return d
	}

	return defSprintDays
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// str renders a JSON scalar as text so a size/confidence that arrived as a number still yields a
// usable string to validate (and be rejected if it isn't a real size).
func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == math.Trunc(t) {
			return fmt.Sprintf("%d", int64(t))
		}

		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		return ""
	}
}

// extractTaskArray pulls the JSON array of tasks out of a model response that may wrap it in fences or
// prose, and — as a side effect — reports any top-level "total"/"total_points" the model volunteered,
// so the caller can see the number Go ignored. It scans for the first '[' and its balanced ']'.
func extractTaskArray(s string) (tasks []map[string]any, claimedTotal *float64, err error) {
	claimedTotal = sniffClaimedTotal(s)

	start := strings.IndexByte(s, '[')
	if start < 0 {
		return nil, claimedTotal, fmt.Errorf("no JSON array found")
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
		case ch == '[':
			depth++
		case ch == ']':
			depth--
			if depth == 0 {
				var raw []any
				if uErr := json.Unmarshal([]byte(s[start:i+1]), &raw); uErr != nil {
					return nil, claimedTotal, uErr
				}

				return coerceTasks(raw), claimedTotal, nil
			}
		}
	}

	return nil, claimedTotal, fmt.Errorf("unbalanced JSON array")
}

// coerceTasks turns the raw array elements into task maps, tolerating a model that flattened the
// breakdown into bare strings: each string becomes a title-only task, which then fails the size check
// in normalize and is reported as `invalid` — so a few malformed elements don't sink the whole
// response the way unmarshalling straight into []map[string]any would.
func coerceTasks(raw []any) []map[string]any {
	out := make([]map[string]any, 0, len(raw))

	for _, e := range raw {
		switch t := e.(type) {
		case map[string]any:
			out = append(out, t)
		case string:
			out = append(out, map[string]any{"title": t})
		}
	}

	return out
}

// sniffClaimedTotal looks for a "total"/"total_points"/"total_story_points" number in the model's
// response — the arithmetic it wasn't asked to do — so a caller can see it was ignored. It only
// matches the token as an object KEY (a colon must immediately follow it, allowing whitespace), so the
// same word appearing inside a task's rationale value is not mistaken for a claimed total. Returns nil
// when the model behaved and gave no total.
func sniffClaimedTotal(s string) *float64 {
	for _, key := range []string{"\"total_story_points\"", "\"total_points\"", "\"total\""} {
		idx := strings.Index(s, key)
		if idx < 0 {
			continue
		}

		// A real key is followed by ':'; a value (e.g. "rationale":"total") is followed by ',' or '}'.
		rest := strings.TrimLeft(s[idx+len(key):], " \t\n")
		if !strings.HasPrefix(rest, ":") {
			continue
		}

		num := strings.Builder{}

		for _, r := range strings.TrimLeft(rest[1:], " \t\n\"") { // tolerate a quoted number too
			if (r >= '0' && r <= '9') || r == '.' {
				num.WriteRune(r)

				continue
			}

			break
		}

		var f float64
		if _, e := fmt.Sscanf(num.String(), "%f", &f); e == nil {
			return &f
		}
	}

	return nil
}
