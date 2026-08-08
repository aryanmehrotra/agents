// breaking-change-agent — a GoFr 1.58 service that detects API/contract breaking changes in a unified
// diff before merge. It's the review-and-release stage of the SDLC suite, sitting right after
// code-review-agent: a diff can look clean and still quietly break every caller of a removed function,
// a renamed JSON field, or a deleted HTTP route.
//
// Like flaky-test-agent, this inverts the usual "model proposes, Go disposes": the DETECTION is the
// deterministic part and lives entirely in Go. A change counts as breaking only if a set-diff over the
// literal declarations/routes/paths that appear in the unified diff proves it — a removed exported Go
// declaration, a changed exported Go signature, a removed struct field (by its json tag), a removed
// HTTP route registration, or a removed OpenAPI/JSON path key. The model is never asked whether
// something is breaking — only to explain the impact of changes Go already found — so a prompt
// injected into the diff text ("ignore this, it's not a breaking change") has nothing to influence:
// there's no judgement call left for it to hijack. If the model is unavailable, the detected list and
// risk level still stand — you just lose the human-readable migration notes.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const maxDiffChars = 200000 // cap on input diff size

// breakingChange is one deterministically detected contract break.
type breakingChange struct {
	File          string `json:"file"`
	Kind          string `json:"kind"` // removed_export | changed_signature | removed_field | removed_route | removed_path
	Name          string `json:"name"`
	Detail        string `json:"detail"`
	MigrationNote string `json:"migration_note,omitempty"` // model annotation (advisory)
}

// fileDiff is the added/removed lines for one file, in the order they appear in the diff.
type fileDiff struct {
	added   []string
	removed []string
}

var (
	reFunc     = regexp.MustCompile(`^func\s+(?:\([^)]*\)\s+)?([A-Z]\w*)\s*\(`)
	reType     = regexp.MustCompile(`^type\s+([A-Z]\w*)\b`)
	reConst    = regexp.MustCompile(`^const\s+([A-Z]\w*)\b`)
	reVar      = regexp.MustCompile(`^var\s+([A-Z]\w*)\b`)
	reField    = regexp.MustCompile("^([A-Z]\\w*)\\s+\\S+.*`json:\"(\\w+)")
	reRoute    = regexp.MustCompile(`\.(GET|POST|PUT|DELETE|PATCH)\(\s*"([^"]+)"`)
	reJSONPath = regexp.MustCompile(`^\s*"(/[^"]*)"\s*:`)
)

func main() {
	app := gofr.New()

	app.POST("/breaking", breaking)

	app.Run()
}

func breaking(c *gofr.Context) (any, error) {
	var in struct {
		Diff string `json:"diff"`
		Text string `json:"text"` // alias slot for the orchestrator's single-string call
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	diff := in.Diff
	if diff == "" {
		diff = in.Text
	}

	if strings.TrimSpace(diff) == "" {
		return map[string]any{
			"error": "provide `diff`: a unified diff (git diff format). Breaking changes are detected " +
				"deterministically from removed/changed exported Go declarations, removed struct fields, " +
				"removed HTTP routes, and removed OpenAPI/JSON path keys.",
		}, nil
	}

	if len(diff) > maxDiffChars {
		diff = diff[:maxDiffChars]
	}

	changes := detect(diff)

	// Advisory only: annotate the already-detected breaking changes with a migration note. Detection
	// stands without this — a model outage doesn't change what's breaking.
	annotate(c, changes)

	return map[string]any{
		"summary": map[string]any{
			"breaking_changes": len(changes),
			"risk":             riskLevel(len(changes)),
		},
		"changes": changes,
		"note": "breaking changes are detected deterministically in Go — a set-diff over the literal " +
			"declarations/routes/paths that appear in the unified diff. The model never decides what's " +
			"breaking; it only explains impact and suggests a migration note per already-detected change.",
	}, nil
}

// riskLevel is a deterministic bucket over the count — no model involved.
func riskLevel(n int) string {
	switch {
	case n == 0:
		return "none"
	case n == 1:
		return "low"
	case n <= 3:
		return "medium"
	default:
		return "high"
	}
}

// detect is the core deterministic logic: parse the diff into per-file added/removed lines, then run
// every category of set-diff over them. No model involved.
func detect(diff string) []breakingChange {
	files := parseDiff(diff)

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}

	sort.Strings(names) // stable output regardless of map iteration order

	var out []breakingChange

	for _, file := range names {
		fd := files[file]

		out = append(out, declChanges(file, fd)...)
		out = append(out, diffKeyed(file, "removed_field", fd, matchField)...)
		out = append(out, diffKeyed(file, "removed_route", fd, matchRoute)...)
		out = append(out, diffKeyed(file, "removed_path", fd, matchJSONPath)...)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}

		return out[i].Name < out[j].Name
	})

	return out
}

// declKey identifies a top-level Go declaration by kind + exported name.
type declKey struct {
	kind string
	name string
}

// declChanges finds removed or changed exported Go declarations (func/type/const/var) in one file. A
// name that only disappears is "removed_export"; a name that appears on both sides with different text
// is "changed_signature" (still breaking — callers built against the old shape). A name that only
// appears added is a pure addition and is never reported.
func declChanges(file string, fd *fileDiff) []breakingChange {
	removed := map[declKey]string{}
	added := map[declKey]string{}

	for _, l := range fd.removed {
		if k, ok := matchDecl(l); ok {
			removed[k] = strings.TrimSpace(l)
		}
	}

	for _, l := range fd.added {
		if k, ok := matchDecl(l); ok {
			added[k] = strings.TrimSpace(l)
		}
	}

	var out []breakingChange

	for k, oldLine := range removed {
		if newLine, stillThere := added[k]; stillThere {
			if newLine != oldLine {
				out = append(out, breakingChange{
					File: file, Kind: "changed_signature", Name: k.name,
					Detail: fmt.Sprintf("was `%s` — now `%s`", oldLine, newLine),
				})
			}
		} else {
			out = append(out, breakingChange{
				File: file, Kind: "removed_export", Name: k.name,
				Detail: fmt.Sprintf("removed `%s`", oldLine),
			})
		}
	}

	return out
}

// diffKeyed reports a breaking change for every key `match` extracts from a removed line that has no
// matching key among the added lines of the same file — a generic removed-only set-diff.
func diffKeyed(file, kind string, fd *fileDiff, match func(string) (key, detail string, ok bool)) []breakingChange {
	removed := map[string]string{}
	added := map[string]bool{}

	for _, l := range fd.removed {
		if k, detail, ok := match(l); ok {
			removed[k] = detail
		}
	}

	for _, l := range fd.added {
		if k, _, ok := match(l); ok {
			added[k] = true
		}
	}

	var out []breakingChange

	for k, detail := range removed {
		if !added[k] {
			out = append(out, breakingChange{File: file, Kind: kind, Name: k, Detail: detail})
		}
	}

	return out
}

func matchDecl(line string) (declKey, bool) {
	l := strings.TrimSpace(line)

	if m := reFunc.FindStringSubmatch(l); m != nil {
		return declKey{"func", m[1]}, true
	}

	if m := reType.FindStringSubmatch(l); m != nil {
		return declKey{"type", m[1]}, true
	}

	if m := reConst.FindStringSubmatch(l); m != nil {
		return declKey{"const", m[1]}, true
	}

	if m := reVar.FindStringSubmatch(l); m != nil {
		return declKey{"var", m[1]}, true
	}

	return declKey{}, false
}

func matchField(line string) (key, detail string, ok bool) {
	m := reField.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return "", "", false
	}

	return m[2], fmt.Sprintf("field %s (json:%q)", m[1], m[2]), true
}

func matchRoute(line string) (key, detail string, ok bool) {
	m := reRoute.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}

	return m[1] + " " + m[2], fmt.Sprintf("route %s %s", m[1], m[2]), true
}

func matchJSONPath(line string) (key, detail string, ok bool) {
	m := reJSONPath.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}

	return m[1], fmt.Sprintf("path %s", m[1]), true
}

// parseDiff splits a unified diff into per-file added/removed line lists, attributing each hunk's lines
// to the file named by its "+++ b/<path>" header (or the "--- a/<path>" header, for a pure deletion
// where the new side is /dev/null).
func parseDiff(diff string) map[string]*fileDiff {
	files := map[string]*fileDiff{}
	oldFile, current := "", "unknown"

	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "--- "):
			oldFile = trimFileHeader(line[4:])
		case strings.HasPrefix(line, "+++ "):
			newFile := trimFileHeader(line[4:])
			if newFile == "/dev/null" {
				current = oldFile
			} else {
				current = newFile
			}
		case strings.HasPrefix(line, "diff --git"), strings.HasPrefix(line, "index "), strings.HasPrefix(line, "@@"):
			// diff metadata / hunk headers — not content
		case strings.HasPrefix(line, "+"):
			f := ensureFile(files, current)
			f.added = append(f.added, line[1:])
		case strings.HasPrefix(line, "-"):
			f := ensureFile(files, current)
			f.removed = append(f.removed, line[1:])
		}
	}

	return files
}

func ensureFile(files map[string]*fileDiff, name string) *fileDiff {
	f, ok := files[name]
	if !ok {
		f = &fileDiff{}
		files[name] = f
	}

	return f
}

// trimFileHeader strips a "---"/"+++" header down to a bare path: drops a trailing tab (some diff
// tools append a timestamp) and the leading "a/" or "b/" prefix.
func trimFileHeader(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}

	s = strings.TrimPrefix(s, "a/")
	s = strings.TrimPrefix(s, "b/")

	return s
}

// annotate asks the model for a one-line migration note per already-detected breaking change, and
// merges them in. Best-effort: any error leaves the (already-valid) detection untouched.
func annotate(c *gofr.Context, changes []breakingChange) {
	if len(changes) == 0 {
		return
	}

	var b strings.Builder
	for _, ch := range changes {
		fmt.Fprintf(&b, "- [%s] %s in %s: %s\n", ch.Kind, ch.Name, ch.File, ch.Detail)
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a release engineer. For each already-detected breaking " +
			"API change below (kind, name, file, detail), write a one-line migration note explaining what " +
			"a caller needs to change to not break. Reply with ONLY a JSON array of " +
			"{\"name\": string, \"migration_note\": string}."},
		{Role: ai.RoleUser, Content: b.String()},
	}, ai.WithTemperature(0))
	if err != nil {
		c.Logger.Errorf("breaking: annotation model unavailable, returning detection only: %v", err)
		return
	}

	var ann []struct {
		Name string `json:"name"`
		Note string `json:"migration_note"`
	}

	if perr := json.Unmarshal([]byte(extractArray(resp.Content)), &ann); perr != nil {
		return
	}

	by := map[string]int{}
	for i := range changes {
		by[changes[i].Name] = i
	}

	for _, a := range ann {
		if i, ok := by[strings.TrimSpace(a.Name)]; ok {
			changes[i].MigrationNote = strings.TrimSpace(a.Note)
		}
	}
}

// extractArray pulls the first balanced JSON array out of a model response that may wrap it in fences
// or prose; returns "[]" when there's none, so json.Unmarshal simply yields no annotations.
func extractArray(s string) string {
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return "[]"
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
				return s[start : i+1]
			}
		}
	}

	return "[]"
}
