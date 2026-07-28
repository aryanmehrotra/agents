// migration-agent — a GoFr 1.58 service that applies a mechanical codemod across a set of files, in
// any language, and verifies it didn't break them. Give it an instruction ("rename getUser to
// fetchUser", "replace the deprecated ioutil calls", "add this license header") and the files, and it
// returns the rewritten files with a per-file diff. Migrations/codemods are the maintenance work of
// the software-development lifecycle — the tedious, repo-wide, mechanical edits.
//
// A model is good at *proposing* the edit but can't be trusted that the result still holds together:
// it drops a brace, mangles a string, or "helpfully" rewrites more than you asked. So the model only
// proposes new file content; Go disposes:
//
//   - it computes a deterministic diff between the original and the rewrite, so you see exactly what
//     changed (a model that quietly rewrote an unrelated line is visible, not hidden);
//   - for the file types it can parse (Go, JSON, YAML) it re-verifies the rewrite — if the codemod
//     produced something that no longer parses, the change is REJECTED and the original file is kept.
//
// That last rule is the whole point: a migration must never leave a file worse than it found it. It's
// all in-process (no disk writes, no repo touched) — you get back the rewritten content and diffs to
// apply yourself.
package main

import (
	"encoding/json"
	"fmt"
	"go/format"
	"path"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gopkg.in/yaml.v3"
)

const (
	maxInstruction = 4000
	maxFiles       = 40
	maxFileBytes   = 128 * 1024
	maxDiffLines   = 1500 // above this (per side) we skip the detailed diff and just report counts
	diffContext    = 2    // lines of unchanged context kept around each change in the diff
)

// inFile is a file the caller wants the codemod applied to.
type inFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// outFile is a file after the codemod + guardrail. When the model's rewrite broke a parseable file,
// Rejected is true, RejectReason explains, and Content is the ORIGINAL (a migration never corrupts).
type outFile struct {
	Path         string `json:"path"`
	Changed      bool   `json:"changed"`
	Checked      bool   `json:"checked"`
	Valid        bool   `json:"valid"`
	Rejected     bool   `json:"rejected,omitempty"`
	RejectReason string `json:"reject_reason,omitempty"`
	Added        int    `json:"added_lines"`
	Removed      int    `json:"removed_lines"`
	Diff         string `json:"diff,omitempty"`
	Content      string `json:"content"`
}

func main() {
	app := gofr.New()

	app.POST("/migrate", migrate)

	app.Run()
}

func migrate(c *gofr.Context) (any, error) {
	var in struct {
		Instruction string   `json:"instruction"`
		Text        string   `json:"text"` // alias for the instruction (orchestrator single-string)
		Files       []inFile `json:"files"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	instruction := strings.TrimSpace(firstNonEmpty(in.Instruction, in.Text))
	if instruction == "" {
		return map[string]any{"error": "provide an `instruction` (the codemod to apply) and `files`."}, nil
	}

	if len(instruction) > maxInstruction {
		instruction = instruction[:maxInstruction]
	}

	originals, skipped := collectFiles(in.Files)
	if len(originals) == 0 {
		return map[string]any{
			"error":   "provide `files` (an array of {path, content}) to apply the codemod to.",
			"skipped": skipped,
		}, nil
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a careful codemod engine. Apply the user's instruction " +
			"to the given files and reply with ONLY a JSON object {\"files\":[{\"path\":string," +
			"\"content\":string}]} containing the FULL new content of every file you changed. Omit files " +
			"you did not change. Make the smallest edit that satisfies the instruction — do not reformat " +
			"or touch unrelated lines. Preserve each file's language and structure. No prose outside JSON."},
		{Role: ai.RoleUser, Content: "Instruction: " + instruction + "\n\n" + renderFiles(in.Files, originals)},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, err := extractJSON(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON result: " + err.Error()}, nil
	}

	rewrites := indexRewrites(raw["files"])
	files, changed, rejected := applyRewrites(in.Files, originals, rewrites)

	return map[string]any{
		"instruction": instruction,
		"files":       files,
		"skipped":     skipped, // inputs dropped before the codemod (unsafe path / too big / dup)
		"verify": map[string]any{
			"files_changed":  changed,
			"files_rejected": rejected, // rewrites that broke a parseable file → original kept
			"clean":          rejected == 0,
		},
		"note": "the diff is computed deterministically in Go; for Go/JSON/YAML files a rewrite that no " +
			"longer parses is rejected and the original is kept, so a codemod never corrupts a file. " +
			"In-process only — nothing is written to disk and no repo is touched.",
		"complete": changed > 0 && rejected == 0,
	}, nil
}

// collectFiles validates the input paths and de-dupes them, returning a path→content map of the files
// we'll actually touch plus a list of inputs skipped (with the reason).
func collectFiles(in []inFile) (originals map[string]string, skipped []map[string]string) {
	originals = map[string]string{}
	skipped = []map[string]string{}

	for i, f := range in {
		if i >= maxFiles {
			skipped = append(skipped, map[string]string{"path": f.Path, "reason": "exceeds file cap"})
			continue
		}

		clean, reason := safePath(f.Path)
		if reason != "" {
			skipped = append(skipped, map[string]string{"path": f.Path, "reason": reason})
			continue
		}

		if _, dup := originals[clean]; dup {
			skipped = append(skipped, map[string]string{"path": clean, "reason": "duplicate path"})
			continue
		}

		if len(f.Content) > maxFileBytes {
			skipped = append(skipped, map[string]string{"path": clean, "reason": "file too large"})
			continue
		}

		originals[clean] = f.Content
	}

	return originals, skipped
}

// renderFiles builds the file section of the prompt, using the cleaned paths so the model refers to
// them the same way we index them back.
func renderFiles(in []inFile, originals map[string]string) string {
	var b strings.Builder
	b.WriteString("Files:\n")

	for _, f := range in {
		clean, reason := safePath(f.Path)
		if reason != "" {
			continue
		}

		if _, ok := originals[clean]; !ok {
			continue
		}

		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", clean, originals[clean])
	}

	return b.String()
}

// indexRewrites pulls the model's returned files into a clean path→content map, dropping any with an
// unsafe path (a rewrite can't introduce a new unsafe file).
func indexRewrites(v any) map[string]string {
	out := map[string]string{}

	arr, ok := v.([]any)
	if !ok {
		return out
	}

	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}

		clean, reason := safePath(str(m["path"]))
		if reason != "" {
			continue
		}

		out[clean] = str(m["content"])
	}

	return out
}

// applyRewrites is the guardrail: for each original file it looks up the model's rewrite, diffs it,
// and — for a parseable type — rejects a rewrite that no longer parses, keeping the original. It walks
// the inputs in their given order so the output is stable.
func applyRewrites(in []inFile, originals, rewrites map[string]string) (files []outFile, changed, rejected int) {
	files = []outFile{}
	done := map[string]bool{}

	for _, f := range in {
		clean, reason := safePath(f.Path)
		if reason != "" || done[clean] {
			continue
		}

		orig, ok := originals[clean]
		if !ok {
			continue
		}

		done[clean] = true

		newContent, wasRewritten := rewrites[clean]
		if !wasRewritten || newContent == orig {
			files = append(files, outFile{Path: clean, Changed: false, Content: orig})
			continue
		}

		of := outFile{Path: clean, Content: newContent}
		checked, valid, verr := verifyType(clean, newContent)
		of.Checked = checked
		of.Valid = valid

		// A rewrite that broke a parseable file is rejected — keep the original.
		if checked && !valid {
			of.Rejected = true
			of.RejectReason = "rewrite no longer parses: " + verr
			of.Content = orig
			of.Changed = false
			of.Valid = false
			rejected++

			files = append(files, of)

			continue
		}

		of.Changed = true
		of.Diff, of.Added, of.Removed = diffText(orig, newContent)
		changed++

		files = append(files, of)
	}

	return files, changed, rejected
}

// verifyType re-parses a rewritten file when its type is one we can check: Go via go/format.Source,
// JSON via encoding/json, YAML via yaml.v3. Returns whether it was checked, whether it's valid, and
// the parser error when not. Unparseable types report checked=false, valid=true.
func verifyType(cleanPath, content string) (checked, valid bool, errMsg string) {
	switch strings.ToLower(path.Ext(cleanPath)) {
	case ".go":
		if _, err := format.Source([]byte(content)); err != nil {
			return true, false, err.Error()
		}

		return true, true, ""

	case ".json":
		var js any
		if err := json.Unmarshal([]byte(content), &js); err != nil {
			return true, false, err.Error()
		}

		return true, true, ""

	case ".yaml", ".yml":
		var y any
		if err := yaml.Unmarshal([]byte(content), &y); err != nil {
			return true, false, err.Error()
		}

		return true, true, ""

	default:
		return false, true, ""
	}
}

// safePath sanitizes a file path: relative, in-root, no traversal. (No extension policy here — a
// codemod operates on whatever source files the caller already has.)
func safePath(p string) (clean, reason string) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", "empty path"
	}

	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") || isWindowsAbs(p) {
		return "", "absolute path not allowed"
	}

	clean = path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if clean == "" || clean == "." {
		return "", "empty path after cleaning"
	}

	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "path escapes the root"
	}

	return clean, ""
}

func isWindowsAbs(p string) bool {
	return len(p) >= 2 && p[1] == ':' &&
		((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z'))
}

// diffText computes a compact, deterministic line diff between old and new: an LCS edit script with a
// few lines of context around each change, and runs of unchanged lines collapsed to "...". Very large
// files skip the detailed hunk and report only the +/- counts.
func diffText(oldText, newText string) (diff string, added, removed int) {
	a := strings.Split(oldText, "\n")
	b := strings.Split(newText, "\n")

	if len(a) > maxDiffLines || len(b) > maxDiffLines {
		added, removed = countChanges(a, b)
		return "(diff omitted — file too large)", added, removed
	}

	ops := lcsOps(a, b)
	for _, o := range ops {
		switch o.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}

	return collapse(ops, diffContext), added, removed
}

type editOp struct {
	kind byte // ' ' common, '+' added, '-' removed
	line string
}

// lcsOps builds the edit script via a longest-common-subsequence table over lines.
func lcsOps(a, b []string) []editOp {
	m, n := len(a), len(b)

	lcs := make([][]int, m+1)
	for i := range lcs {
		lcs[i] = make([]int, n+1)
	}

	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	ops := []editOp{}
	i, j := 0, 0

	for i < m && j < n {
		switch {
		case a[i] == b[j]:
			ops = append(ops, editOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, editOp{'-', a[i]})
			i++
		default:
			ops = append(ops, editOp{'+', b[j]})
			j++
		}
	}

	for ; i < m; i++ {
		ops = append(ops, editOp{'-', a[i]})
	}

	for ; j < n; j++ {
		ops = append(ops, editOp{'+', b[j]})
	}

	return ops
}

// collapse renders the edit script, keeping `ctx` unchanged lines around each change and replacing
// longer unchanged runs with a "..." marker.
func collapse(ops []editOp, ctx int) string {
	keep := make([]bool, len(ops))

	for idx, o := range ops {
		if o.kind == ' ' {
			continue
		}

		for k := idx - ctx; k <= idx+ctx; k++ {
			if k >= 0 && k < len(ops) {
				keep[k] = true
			}
		}
	}

	var b strings.Builder

	gap := false

	for idx, o := range ops {
		if !keep[idx] {
			gap = true
			continue
		}

		if gap {
			b.WriteString("   ...\n")

			gap = false
		}

		b.WriteByte(o.kind)
		b.WriteByte(' ')
		b.WriteString(o.line)
		b.WriteByte('\n')
	}

	return b.String()
}

// countChanges is the cheap fallback for oversized files: how many lines differ, without a full diff.
func countChanges(a, b []string) (added, removed int) {
	ac := map[string]int{}
	for _, l := range a {
		ac[l]++
	}

	bc := map[string]int{}
	for _, l := range b {
		bc[l]++
	}

	for l, n := range bc {
		if extra := n - ac[l]; extra > 0 {
			added += extra
		}
	}

	for l, n := range ac {
		if extra := n - bc[l]; extra > 0 {
			removed += extra
		}
	}

	return added, removed
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}

	return ""
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}

	return ""
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
