// breaking-change-agent — a GoFr 1.58 service that reads a code diff and decides whether it breaks a
// public API/contract: a removed function, a field that changed type, a new required parameter, a
// dropped enum value. This is the "review & release" gate teams most want automated — the review step
// where a human reviewer's attention is most valuable and most often skipped under deadline pressure.
//
// A model is good at *spotting* an API break but cannot be trusted to *judge* it: asked whether a diff
// is breaking, it will confidently say yes or no about a line it never actually saw, and it has no
// fixed idea of which categories of change are breaking versus cosmetic. So the model only proposes —
// for each candidate break it names the file, line, and a category from a closed set — and Go disposes:
//
//   - every finding's file+line is checked against lines Go itself parsed out of the diff; a finding
//     that points at a line the diff never touched is dropped from the verdict, not trusted;
//   - every category maps to a severity through a fixed table (mirroring estimation-agent's
//     size→points ladder) — the model's own "breaking or not" label is never read, only its category;
//   - the overall verdict is computed in Go: BREAKING iff at least one verified finding's category is
//     breaking. If the model volunteers its own top-level verdict, it's recorded next to the one Go
//     computed and then ignored, so a mismatch is visible rather than silently trusted.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const (
	maxDiffChars = 60000
	maxFindings  = 40
	maxFieldLen  = 300
)

// categorySeverity is the fixed, deterministic table a finding's category maps to. The model picks a
// category (from this exact set, given in the prompt); Go — never the model — decides whether that
// category counts as breaking. A category not in this table is not guessed at; it's reported invalid.
var categorySeverity = map[string]string{
	"removed_symbol":       "breaking", // a public func/method/type/endpoint/field was deleted
	"signature_changed":    "breaking", // a parameter was added/removed/reordered/retyped
	"type_changed":         "breaking", // a field's or return value's type changed incompatibly
	"required_field_added": "breaking", // a new required param/field with no default
	"removed_enum_value":   "breaking", // a caller could have depended on the removed value
	"visibility_reduced":   "breaking", // exported → unexported, public → private
	"renamed_symbol":       "breaking", // a rename with no compatibility alias breaks existing callers
	"behavior_changed":     "advisory", // same shape, different behavior — worth a human look, not a hard break
	"deprecated":           "advisory", // marked deprecated but still present — not yet breaking
	"other":                "advisory",
}

// diffLine is one added or removed line, with the line number it occupies in its own file version (new
// file for '+', old file for '-') — Go's ground truth for what the diff actually changed.
type diffLine struct {
	File string
	Line int
	Kind byte // '+' or '-'
	Text string
}

// lineKey identifies a changed line for lookup, independent of its diff hunk.
type lineKey struct {
	File string
	Line int
}

// finding is one candidate breaking change after the guardrail has run: the model supplied File/Line/
// Category/Symbol/Description; Severity and Verified are filled in deterministically by Go.
type finding struct {
	File        string `json:"file"`
	Line        int    `json:"line"`
	Category    string `json:"category"`
	Symbol      string `json:"symbol,omitempty"`
	Description string `json:"description"`
	Severity    string `json:"severity"`
	Verified    bool   `json:"verified"`
}

// invalidFinding is a finding the model returned with a category Go's table doesn't recognize — never
// silently folded into "advisory".
type invalidFinding struct {
	File     string `json:"file"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

func main() {
	app := gofr.New()

	app.POST("/check", checkBreaking)

	app.Run()
}

const breakingSystem = `You are a precise API compatibility reviewer. You are given a unified diff. Find changes to a
PUBLIC API or data contract (exported functions/methods/types, struct/JSON fields, HTTP endpoints,
enum values, config keys) that would break an existing caller. Only report lines that actually appear
in the diff as a + or - line — never invent a line you did not see.

Reply with ONLY a JSON object: {"findings":[{"file":"path","line":123,"category":"...","symbol":"name",
"description":"one sentence"}]}. "line" is the line number in the file version the change is anchored
to (the NEW file for an added/changed line, the OLD file for a removed line). "category" MUST be one of
exactly: removed_symbol, signature_changed, type_changed, required_field_added, removed_enum_value,
visibility_reduced, renamed_symbol, behavior_changed, deprecated, other. Do not invent other category
names and do not include a top-level verdict — categorization is your job, judging severity is not.
Return "findings":[] if nothing in the diff touches a public API.`

func checkBreaking(c *gofr.Context) (any, error) {
	var in struct {
		Title string `json:"title"`
		Diff  string `json:"diff"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	diff := strings.TrimSpace(in.Diff)
	if diff == "" {
		return map[string]any{
			"error": "provide a `diff` (unified diff text) to check for breaking API changes; an " +
				"optional `title` gives the model context.",
		}, nil
	}

	if len(diff) > maxDiffChars {
		diff = diff[:maxDiffChars]
	}

	lines := parseDiff(diff)
	if len(lines) == 0 {
		return map[string]any{
			"error": "no +/- changed lines found in `diff` — provide a unified diff (git diff / PR patch format)",
		}, nil
	}

	idx := diffLineIndex(lines)

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: breakingSystem},
		{Role: ai.RoleUser, Content: "Title: " + in.Title + "\n\nDiff:\n" + diff},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, claimedVerdict, err := extractFindings(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON result: " + err.Error()}, nil
	}

	verified, unverified, invalid := normalizeFindings(raw, idx)
	breaking, breakingCount := verdict(verified)

	out := map[string]any{
		"title":                in.Title,
		"changed_lines":        len(lines),
		"findings":             verified,
		"unverified_findings":  unverified, // referenced a file:line the diff never touched — dropped from the verdict
		"invalid_findings":     invalid,    // an unrecognized category — dropped from the verdict
		"breaking":             breaking,
		"breaking_count":       breakingCount,
		"verdict":              verdictLabel(breaking),
		"note": "severity is computed in Go from a fixed category table and a finding only counts toward " +
			"the verdict if its file:line is a real changed line in the diff — the model's own " +
			"judgment of severity or of what changed is never trusted directly.",
	}

	// Surface the guardrail concretely: if the model volunteered its own verdict anyway, show it next
	// to the one Go actually computed, so a mismatch is visible rather than hidden.
	if claimedVerdict != nil {
		out["model_claimed_verdict"] = *claimedVerdict
	}

	return out, nil
}

func verdictLabel(breaking bool) string {
	if breaking {
		return "BREAKING"
	}

	return "SAFE"
}

// verdict is the arithmetic the model is not trusted with: BREAKING iff at least one VERIFIED finding
// has a breaking severity. Unverified and invalid findings never influence it.
func verdict(verified []finding) (breaking bool, breakingCount int) {
	for _, f := range verified {
		if f.Severity == "breaking" {
			breakingCount++
		}
	}

	return breakingCount > 0, breakingCount
}

// normalizeFindings is the guardrail: it maps each raw finding's category to a Go-computed severity
// (dropping any with an unrecognized category into `invalid`), then splits the rest into `verified`
// (file:line is a real changed line in the diff) and `unverified` (it isn't — a hallucinated
// reference, never counted toward the verdict).
func normalizeFindings(raw []map[string]any, idx map[lineKey]bool) (verified, unverified []finding, invalid []invalidFinding) {
	verified = []finding{}
	unverified = []finding{}
	invalid = []invalidFinding{}

	for _, e := range raw {
		file := cleanField(str(e["file"]))
		line := intFrom(e["line"])
		category := strings.ToLower(strings.TrimSpace(str(e["category"])))

		sev, ok := categorySeverity[category]
		if !ok {
			invalid = append(invalid, invalidFinding{
				File:     file,
				Category: category,
				Reason:   fmt.Sprintf("unrecognized category %q", category),
			})

			continue
		}

		f := finding{
			File:        file,
			Line:        line,
			Category:    category,
			Symbol:      cleanField(str(e["symbol"])),
			Description: cleanField(str(e["description"])),
			Severity:    sev,
		}

		if idx[lineKey{File: file, Line: line}] {
			f.Verified = true
			verified = append(verified, f)
		} else {
			unverified = append(unverified, f)
		}

		if len(verified)+len(unverified) >= maxFindings {
			break
		}
	}

	return verified, unverified, invalid
}

// parseDiff walks a unified diff and returns every added ('+') and removed ('-') content line with the
// line number it occupies in its own file version. This is Go's ground truth for "what actually
// changed" — a finding that references any other file:line is a hallucination, not a real break.
func parseDiff(diff string) []diffLine {
	hunkRE := regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

	var out []diffLine

	var oldFile, newFile string

	var oldLine, newLine int

	inHunk := false

	for _, raw := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(raw, "diff --git") || strings.HasPrefix(raw, "index "):
			inHunk = false
		case strings.HasPrefix(raw, "--- "):
			inHunk = false
			oldFile = diffPath(raw[4:], "a/")
		case strings.HasPrefix(raw, "+++ "):
			inHunk = false
			newFile = diffPath(raw[4:], "b/")
		case strings.HasPrefix(raw, "@@"):
			if m := hunkRE.FindStringSubmatch(raw); m != nil {
				oldLine, _ = strconv.Atoi(m[1])
				newLine, _ = strconv.Atoi(m[2])
				inHunk = true
			}
		case inHunk && strings.HasPrefix(raw, "+"):
			out = append(out, diffLine{File: newFile, Line: newLine, Kind: '+', Text: raw[1:]})
			newLine++
		case inHunk && strings.HasPrefix(raw, "-"):
			out = append(out, diffLine{File: oldFile, Line: oldLine, Kind: '-', Text: raw[1:]})
			oldLine++
		case inHunk:
			// a context line (starts with a space, or blank) — advances both counters, no finding anchors here
			oldLine++
			newLine++
		}
	}

	return out
}

// diffPath cleans a "--- a/path\t<timestamp>" style header down to the bare path and strips the
// standard git a/ or b/ prefix, tolerating a diff with no such prefix (e.g. from `diff -u`).
func diffPath(s, prefix string) string {
	s = strings.TrimSpace(s)
	if fields := strings.Fields(s); len(fields) > 0 {
		s = fields[0]
	}

	return strings.TrimPrefix(s, prefix)
}

// diffLineIndex builds the file:line lookup set a finding is verified against.
func diffLineIndex(lines []diffLine) map[lineKey]bool {
	idx := make(map[lineKey]bool, len(lines))
	for _, l := range lines {
		idx[lineKey{File: l.File, Line: l.Line}] = true
	}

	return idx
}

var verdictRE = regexp.MustCompile(`"verdict"\s*:\s*"([^"]{0,40})"`)

// sniffClaimedVerdict looks for a top-level "verdict" string the model volunteered despite not being
// asked for one, so it can be shown next to (and ignored in favor of) the one Go computed.
func sniffClaimedVerdict(s string) *string {
	m := verdictRE.FindStringSubmatch(s)
	if m == nil {
		return nil
	}

	return &m[1]
}

func cleanField(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxFieldLen {
		s = strings.TrimSpace(s[:maxFieldLen]) + "…"
	}

	return s
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}

	return ""
}

// intFrom reads a JSON number (always float64 after unmarshal) as an int; anything else is 0, which
// will simply fail to match any real diff line and land the finding in `unverified`.
func intFrom(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}

	return 0
}

// extractFindings pulls {"findings":[...]} out of a model response that may wrap it in fences or
// prose, and separately sniffs any top-level "verdict" the model volunteered.
func extractFindings(s string) (findings []map[string]any, claimedVerdict *string, err error) {
	claimedVerdict = sniffClaimedVerdict(s)

	obj, err := extractJSON(s)
	if err != nil {
		return nil, claimedVerdict, err
	}

	arr, _ := obj["findings"].([]any)

	return coerceFindings(arr), claimedVerdict, nil
}

func coerceFindings(raw []any) []map[string]any {
	out := make([]map[string]any, 0, len(raw))

	for _, e := range raw {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}

	return out
}

// extractJSON pulls a single JSON object out of a model response that may wrap it in fences or prose.
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
