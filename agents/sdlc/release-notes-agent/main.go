// release-notes-agent — a GoFr 1.58 service that drafts a changelog / release notes doc from a set of
// merged PRs. Release-notes drafting is one of the narrow, easy-to-measure workflows teams are
// starting agent adoption with in 2026 — alongside task breakdown, test generation and incident
// summaries — because the input (a PR list) and the output (a changelog) are both cheap to check
// (HackerNoon, "How AI Agents Are Reshaping Software Delivery in 2026").
//
// The model is good at the judgment call a changelog needs — is this PR a feature, a fix, a breaking
// change? — but cannot be trusted to keep the *shape* of a changelog: it invents categories, drops
// PRs, or free-forms prose instead of a list. So the model only proposes a category and a clean
// one-line entry per PR; Go disposes — every category is checked against a fixed, closed set (an
// unrecognized or missing category is never trusted, and falls back to a deterministic label/title
// keyword classifier instead), every line is trimmed and length-capped, duplicate lines within a
// category are dropped, and the final Markdown is assembled deterministically in a fixed section
// order. If the model is unavailable entirely, every PR is still categorized and the changelog still
// renders — by the same deterministic keyword fallback — so a model outage produces a rougher
// changelog, never a failed request.
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
	maxPRs       = 300 // cap on input PRs per request
	maxLineChars = 200 // cap on a single changelog entry
	maxBodyChars = 400 // cap on PR body sent to the model, per PR
)

// categories is the closed, ordered set a PR may be filed under. Order here is the section order in
// the rendered changelog. The model's category suggestion is only trusted if it's exactly one of
// these; anything else falls back to categorizeByKeyword.
var categories = []struct{ key, heading string }{
	{"breaking", "⚠️ Breaking Changes"},
	{"feature", "✨ Features"},
	{"fix", "🐛 Fixes"},
	{"perf", "⚡ Performance"},
	{"security", "🔒 Security"},
	{"docs", "📚 Docs"},
	{"chore", "🧹 Chores"},
}

// prEntry is one merged PR, as supplied by the caller or parsed from free-form text.
type prEntry struct {
	Number int      `json:"number,omitempty"`
	Title  string   `json:"title"`
	Author string   `json:"author,omitempty"`
	Labels []string `json:"labels,omitempty"`
	Body   string   `json:"body,omitempty"`
}

// notesEntry is one PR after categorization — the deterministic output the changelog is built from.
type notesEntry struct {
	Number   int    `json:"number,omitempty"`
	Title    string `json:"title"`
	Line     string `json:"line"`     // cleaned one-line changelog entry
	Category string `json:"category"` // always one of the closed `categories` set
	Source   string `json:"source"`   // "model" or "keyword-fallback"
}

func main() {
	app := gofr.New()

	app.POST("/release-notes", releaseNotes)

	app.Run()
}

func releaseNotes(c *gofr.Context) (any, error) {
	var in struct {
		PRs     []prEntry `json:"prs"`
		Text    string    `json:"text"`    // alias: free-form PR list, one per line — also the orchestrator's single-string call
		Version string    `json:"version"` // optional heading, e.g. "v1.2.0"
		Range   string    `json:"range"`   // optional, e.g. "since v1.1.0"
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	prs := in.PRs
	if len(prs) == 0 {
		prs = parsePRsFromText(in.Text)
	}

	if len(prs) == 0 {
		return map[string]any{
			"error": "provide `prs`: an array of {number?, title, author?, labels?, body?}, or `text`: " +
				"one merged PR title per line (optionally prefixed with #123).",
		}, nil
	}

	if len(prs) > maxPRs {
		prs = prs[:maxPRs]
	}

	entries := categorize(c, prs)
	notes := buildNotes(entries, in.Version, in.Range)

	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Category]++
	}

	return map[string]any{
		"prs_processed": len(entries),
		"counts":        counts,
		"entries":       entries,
		"notes":         notes,
		"note": "category is validated against a fixed closed set in Go; an unrecognized or missing " +
			"model category falls back to a deterministic label/title keyword classifier, so every PR " +
			"is always categorized even if the model is unavailable.",
	}, nil
}

// parsePRsFromText deterministically parses a free-form, one-PR-per-line block of text into prEntry
// rows — the shape the orchestrator's single-string call (and a quick curl) produce. A leading
// "#123 " or "123: " is read as the PR number; the rest of the line is the title. Blank lines are
// skipped.
func parsePRsFromText(text string) []prEntry {
	var out []prEntry

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		num, rest := 0, line

		if strings.HasPrefix(line, "#") {
			if sp := strings.IndexAny(line, " \t"); sp > 1 {
				if n, err := strconv.Atoi(line[1:sp]); err == nil {
					num, rest = n, strings.TrimSpace(line[sp+1:])
				}
			}
		} else if c := strings.IndexByte(line, ':'); c > 0 {
			if n, err := strconv.Atoi(strings.TrimSpace(line[:c])); err == nil {
				num, rest = n, strings.TrimSpace(line[c+1:])
			}
		}

		if rest == "" {
			continue
		}

		out = append(out, prEntry{Number: num, Title: rest})
	}

	return out
}

// categorize is the model-proposes / Go-disposes core. It asks the model to categorize and clean up
// every PR in one call, then validates each answer: the category must be one of the closed set (else
// the deterministic keyword fallback decides), and the line is cleaned and length-capped. If the model
// call fails outright, every PR is categorized by the keyword fallback alone — a model outage degrades
// the changelog, it never fails the request.
func categorize(c *gofr.Context, prs []prEntry) []notesEntry {
	proposed := map[string]modelPR{}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You write software changelogs. For each PR below, pick the " +
			"single best category from EXACTLY this set: breaking, feature, fix, perf, security, docs, " +
			"chore. Also rewrite the title into a clean, imperative, user-facing one-line changelog " +
			"entry (no PR number, no ticket id, no trailing period). Reply with ONLY a JSON array of " +
			"{\"ref\": string, \"category\": string, \"line\": string} — `ref` must be copied exactly " +
			"from the \"ref:\" given for that PR."},
		{Role: ai.RoleUser, Content: describePRs(prs)},
	}, ai.WithTemperature(0))
	if err != nil {
		c.Logger.Errorf("release-notes: categorization model unavailable, using keyword fallback for all PRs: %v", err)
	} else if arr, perr := parseModelPRs(resp.Content); perr == nil {
		for _, m := range arr {
			proposed[m.Ref] = m
		}
	}

	entries := make([]notesEntry, 0, len(prs))

	for i, p := range prs {
		ref := refFor(p, i)

		m, ok := proposed[ref]

		cat, source := "", "keyword-fallback"
		if ok {
			cat, source = validCategory(m.Category), "model"
		}

		if cat == "" { // no proposal, or the model's category wasn't in the closed set
			cat, source = categorizeByKeyword(p), "keyword-fallback"
		}

		line := ""
		if ok {
			line = cleanLine(m.Line)
		}

		if line == "" {
			line = cleanLine(p.Title)
		}

		entries = append(entries, notesEntry{
			Number: p.Number, Title: p.Title, Line: line, Category: cat, Source: source,
		})
	}

	return dedupe(entries)
}

// modelPR is the model's proposed category + rewritten line for one PR, keyed by the `ref` echoed back
// from the prompt.
type modelPR struct {
	Ref      string `json:"ref"`
	Category string `json:"category"`
	Line     string `json:"line"`
}

// refFor is the stable identifier a PR is described and matched by: its number if it has one, else its
// position in the request.
func refFor(p prEntry, i int) string {
	if p.Number != 0 {
		return "#" + strconv.Itoa(p.Number)
	}

	return "idx" + strconv.Itoa(i)
}

// describePRs renders every PR for the prompt, each tagged with the exact `ref` the model must echo
// back so its answer can be matched deterministically — never by fragile title matching.
func describePRs(prs []prEntry) string {
	var b strings.Builder

	for i, p := range prs {
		fmt.Fprintf(&b, "ref: %s\ntitle: %s\n", refFor(p, i), p.Title)

		if len(p.Labels) > 0 {
			fmt.Fprintf(&b, "labels: %s\n", strings.Join(p.Labels, ", "))
		}

		if body := truncate(strings.TrimSpace(p.Body), maxBodyChars); body != "" {
			fmt.Fprintf(&b, "body: %s\n", body)
		}

		b.WriteString("\n")
	}

	return b.String()
}

// validCategory returns the category if it's exactly one of the closed set, else "" — the guardrail
// that keeps a model's invented or misspelled category from ever reaching the output.
func validCategory(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))

	for _, cat := range categories {
		if cat.key == s {
			return s
		}
	}

	return ""
}

// categorizeByKeyword is the deterministic fallback classifier: it never calls the model, and always
// returns one of the closed category set. Labels are checked first (a maintainer-applied label is a
// stronger signal than title text), then the title.
func categorizeByKeyword(p prEntry) string {
	hay := strings.ToLower(strings.Join(p.Labels, " ") + " " + p.Title)

	switch {
	case containsAny(hay, "breaking", "breaking-change", "breaking change"):
		return "breaking"
	case containsAny(hay, "security", "vuln", "cve"):
		return "security"
	case containsAny(hay, "perf", "performance", "optimi"):
		return "perf"
	case containsAny(hay, "fix", "bug", "bugfix", "hotfix"):
		return "fix"
	case containsAny(hay, "feat", "feature", "enhancement"):
		return "feature"
	case containsAny(hay, "doc", "docs", "documentation"):
		return "docs"
	default:
		return "chore"
	}
}

// cleanLine strips the common conventional-commit prefixes off a title, trims and length-caps it. It
// never returns a non-empty input as empty, so the caller always has a usable fallback line.
func cleanLine(s string) string {
	s = strings.TrimSpace(s)

	for _, prefix := range []string{
		"feat:", "feat!:", "fix:", "fix!:", "chore:", "docs:", "perf:", "refactor:", "test:", "build:", "ci:",
	} {
		if strings.HasPrefix(strings.ToLower(s), prefix) {
			s = strings.TrimSpace(s[len(prefix):])
			break
		}
	}

	s = strings.TrimSuffix(s, ".")

	return truncate(s, maxLineChars)
}

// dedupe drops entries whose (category, lowercased line) has already been seen, keeping the first —
// so a PR the model rewrote to say the same thing twice doesn't double up the changelog.
func dedupe(entries []notesEntry) []notesEntry {
	seen := map[string]bool{}
	out := make([]notesEntry, 0, len(entries))

	for _, e := range entries {
		key := e.Category + "\x00" + strings.ToLower(e.Line)
		if seen[key] {
			continue
		}

		seen[key] = true
		out = append(out, e)
	}

	return out
}

// buildNotes deterministically renders the categorized entries as Markdown, in the fixed category
// order, each entry within a category sorted by PR number (numbered PRs first, ascending; unnumbered
// entries keep their original relative order after).
func buildNotes(entries []notesEntry, version, rangeDesc string) string {
	var b strings.Builder

	if version != "" {
		fmt.Fprintf(&b, "# %s\n\n", version)
	} else {
		b.WriteString("# Release Notes\n\n")
	}

	if rangeDesc != "" {
		fmt.Fprintf(&b, "_%s_\n\n", rangeDesc)
	}

	byCat := map[string][]notesEntry{}
	for _, e := range entries {
		byCat[e.Category] = append(byCat[e.Category], e)
	}

	wrote := false

	for _, cat := range categories {
		group := byCat[cat.key]
		if len(group) == 0 {
			continue
		}

		sort.SliceStable(group, func(i, j int) bool {
			ni, nj := group[i].Number, group[j].Number
			if ni == 0 || nj == 0 {
				return false // unnumbered entries: preserve relative (stable-sort) order
			}

			return ni < nj
		})

		wrote = true

		fmt.Fprintf(&b, "### %s\n\n", cat.heading)

		for _, e := range group {
			if e.Number != 0 {
				fmt.Fprintf(&b, "- %s (#%d)\n", e.Line, e.Number)
			} else {
				fmt.Fprintf(&b, "- %s\n", e.Line)
			}
		}

		b.WriteString("\n")
	}

	if !wrote {
		b.WriteString("_No changes._\n")
	}

	return strings.TrimRight(b.String(), "\n") + "\n"
}

// parseModelPRs pulls the first balanced JSON array out of a model response that may wrap it in fences
// or prose; returns an error only when there's no array to be found at all — any per-element decode
// issue is handled by the caller simply not finding that ref in the map.
func parseModelPRs(s string) ([]modelPR, error) {
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return nil, fmt.Errorf("no JSON array found")
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
				var arr []modelPR

				err := json.Unmarshal([]byte(s[start:i+1]), &arr)

				return arr, err
			}
		}
	}

	return nil, fmt.Errorf("unbalanced JSON array")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return strings.TrimSpace(s[:n]) + "…"
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}

	return false
}
