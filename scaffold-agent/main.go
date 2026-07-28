// scaffold-agent — a GoFr 1.58 service that generates a project skeleton from a spec, in ANY stack.
// Give it a spec ("a REST API that tracks orders") and optionally a stack ("python fastapi", "node
// express", "rust axum", "go gofr"), and it returns a set of files ready to drop into a new project.
// Scaffolding is the build stage of the software-development lifecycle: the repetitive first hour of a
// new service, done in seconds — whatever language you work in.
//
// The catch is that generated files are only worth anything if they're safe and real. A model will
// happily emit a file written to "../../etc/passwd", a binary blob, or truncated source. So the model
// only proposes files; Go disposes of the unsafe and the unusable before anything leaves the service,
// and every part of that is language-agnostic:
//
//   - every path is sanitized — absolute paths, "..", and anything escaping the scaffold root are
//     rejected, along with binary/executable file types; and every file must be valid UTF-8 text;
//   - files we can parse are syntax-checked — Go via go/format.Source (and returned gofmt'd), JSON via
//     encoding/json, YAML via yaml.v3 — and a file that fails is flagged with the parse error. Files in
//     a language we can't parse here are returned untouched and honestly marked unchecked.
//
// It's all in-process — no disk writes, no build, no network — so the agent never touches your repo and
// never blocks. You get back the files (with per-file validity), an honest list of what was rejected,
// and a summary, to write out yourself.
package main

import (
	"encoding/json"
	"fmt"
	"go/format"
	"path"
	"strings"
	"unicode/utf8"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
	"gopkg.in/yaml.v3"
)

const (
	maxSpecChars = 20000
	maxFiles     = 40         // cap on files kept
	maxEntries   = 300        // cap on proposed entries we'll even look at (bounds a runaway response)
	maxFileBytes = 128 * 1024 // cap on a single file
)

// deniedExt are binary/executable file types a text scaffold must never contain — the one extension
// rule that stays even though the agent is language-agnostic (everything else text is allowed).
var deniedExt = map[string]bool{
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".bin": true, ".o": true, ".a": true,
	".class": true, ".pyc": true, ".wasm": true, ".zip": true, ".tar": true, ".gz": true, ".jar": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".pdf": true, ".ico": true, ".woff": true,
}

// genFile is one file in the scaffold after the guardrail has run. Checked reports whether this file
// type could be syntax-verified here; Valid is that check's result (true for a type we don't check,
// since absence of a checker isn't evidence of a problem). For a checked file that failed, Error holds
// the parser's message.
type genFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Bytes   int    `json:"bytes"`
	Checked bool   `json:"checked"`
	Valid   bool   `json:"valid"`
	Error   string `json:"error,omitempty"`
}

// rejectedFile is a file the model proposed that never made it in — an unsafe path, a binary type, or
// non-text content. Reported, never silently written.
type rejectedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func main() {
	app := gofr.New()

	app.POST("/scaffold", scaffold)

	app.Run()
}

func scaffold(c *gofr.Context) (any, error) {
	var in struct {
		Spec   string `json:"spec"`
		Text   string `json:"text"`  // alias, so the orchestrator (single string) can drive it
		Stack  string `json:"stack"` // optional target stack; inferred from the spec when absent
		Module string `json:"module"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	spec := strings.TrimSpace(firstNonEmpty(in.Spec, in.Text))
	if spec == "" {
		return map[string]any{
			"error": "provide a `spec` (a description of the project/module to scaffold); optional " +
				"`stack` (e.g. \"python fastapi\", \"node express\", \"go gofr\") and `module`/project name.",
		}, nil
	}

	if len(spec) > maxSpecChars {
		spec = spec[:maxSpecChars]
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a project scaffolding engine that works in any language " +
			"or framework. Given a spec (and a target stack if provided, otherwise pick a sensible one), " +
			"produce a minimal but runnable project skeleton for THAT stack — entry point, the routes/" +
			"modules the spec implies with stub logic, a dependency/manifest file, and a test. Reply with " +
			"ONLY a JSON object: {\"stack\": string, \"files\": [{\"path\": string, \"content\": string}]}. " +
			"Paths are relative with forward slashes, no leading slash and no \"..\". No prose outside JSON."},
		{Role: ai.RoleUser, Content: stackHint(in.Stack) + moduleHint(in.Module) + "Spec:\n" + spec},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, err := extractJSON(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON scaffold: " + err.Error()}, nil
	}

	files, rejected := process(raw["files"])
	stack := firstNonEmpty(strings.TrimSpace(str(raw["stack"])), strings.TrimSpace(in.Stack), "(inferred)")

	checked, invalid := summarize(files)

	return map[string]any{
		"stack":    stack,
		"module":   strings.TrimSpace(in.Module),
		"files":    files,
		"rejected": rejected,
		"verify": map[string]any{
			"files_generated":   len(files),
			"syntax_checked":    checked,      // files whose type we could parse here
			"syntax_invalid":    invalid,      // of those, how many failed
			"all_checked_valid": invalid == 0, // nothing we could check was broken
		},
		"note": "language-agnostic: every path is sanitized against traversal/escape, binary types are " +
			"rejected, and every file must be valid UTF-8 text. Files we can parse (Go, JSON, YAML) are " +
			"syntax-checked; others are returned as-is and marked unchecked. In-process only — nothing is " +
			"written to disk and no repo is touched.",
		"complete": len(files) > 0 && invalid == 0,
	}, nil
}

// stackHint / moduleHint thread caller-supplied hints into the prompt when present.
func stackHint(stack string) string {
	if s := strings.TrimSpace(stack); s != "" {
		return "Target stack: " + s + "\n"
	}

	return ""
}

func moduleHint(module string) string {
	if m := strings.TrimSpace(module); m != "" {
		return "Project/module name: " + m + "\n"
	}

	return ""
}

// process is the guardrail: it walks the model's proposed files, drops the unsafe/disallowed/non-text
// ones into `rejected`, and syntax-verifies each surviving file whose type it can parse. It caps the
// number of entries it will even consider, the files it keeps, and the per-file size, so a runaway
// response can't blow up the output.
func process(v any) (files []genFile, rejected []rejectedFile) {
	files = []genFile{}
	rejected = []rejectedFile{}

	arr, ok := v.([]any)
	if !ok {
		return files, rejected
	}

	seen := make(map[string]bool)

	for i, e := range arr {
		if i >= maxEntries {
			break
		}

		m, ok := e.(map[string]any)
		if !ok {
			continue
		}

		rawPath := str(m["path"])

		clean, reason := safePath(rawPath)
		if reason != "" {
			rejected = append(rejected, rejectedFile{Path: rawPath, Reason: reason})
			continue
		}

		if seen[clean] {
			rejected = append(rejected, rejectedFile{Path: rawPath, Reason: "duplicate path"})
			continue
		}

		seen[clean] = true

		content := str(m["content"])

		if len(content) > maxFileBytes {
			rejected = append(rejected, rejectedFile{Path: clean, Reason: fmt.Sprintf(
				"file exceeds %d bytes", maxFileBytes)})

			continue
		}

		if !utf8.ValidString(content) {
			rejected = append(rejected, rejectedFile{Path: clean, Reason: "content is not valid UTF-8 text"})
			continue
		}

		files = append(files, verifyFile(clean, content))
		if len(files) == maxFiles {
			break
		}
	}

	return files, rejected
}

// verifyFile builds a genFile, syntax-checking the file when its type is one this service can parse:
// Go through go/format.Source (which parses it, and returns it gofmt'd), JSON through encoding/json,
// YAML through yaml.v3. A file whose type isn't one of those is returned untouched with Checked=false
// — honestly unchecked, not silently blessed.
func verifyFile(cleanPath, content string) genFile {
	f := genFile{Path: cleanPath, Content: content, Bytes: len(content), Valid: true}

	switch strings.ToLower(path.Ext(cleanPath)) {
	case ".go":
		f.Checked = true

		formatted, err := format.Source([]byte(content))
		if err != nil {
			f.Valid = false
			f.Error = err.Error()

			return f
		}

		f.Content = string(formatted)
		f.Bytes = len(f.Content)

	case ".json":
		f.Checked = true

		var js any
		if err := json.Unmarshal([]byte(content), &js); err != nil {
			f.Valid = false
			f.Error = err.Error()
		}

	case ".yaml", ".yml":
		f.Checked = true

		var y any
		if err := yaml.Unmarshal([]byte(content), &y); err != nil {
			f.Valid = false
			f.Error = err.Error()
		}
	}

	return f
}

// summarize counts how many files could be syntax-checked and how many of those failed.
func summarize(files []genFile) (checked, invalid int) {
	for _, f := range files {
		if !f.Checked {
			continue
		}

		checked++

		if !f.Valid {
			invalid++
		}
	}

	return checked, invalid
}

// safePath sanitizes a model-proposed file path so a scaffold can never write outside its own root or
// drop a binary. It rejects absolute paths and anything that escapes via "..", normalizes slashes, and
// blocks binary/executable extensions. Language-agnostic: any text source/config file is allowed.
func safePath(p string) (clean, reason string) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", "empty path"
	}

	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") || isWindowsAbs(p) {
		return "", "absolute path not allowed"
	}

	// Clean the RELATIVE path (no leading slash): interior ".." that stays in-root collapses away
	// (a/b/../x → a/x), but a path that climbs out keeps a leading "..". Prepending "/" first would
	// wrongly *absorb* the traversal (/../x → /x) and let an escape through as an in-root file.
	clean = path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if clean == "" || clean == "." {
		return "", "empty path after cleaning"
	}

	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "path escapes the scaffold root"
	}

	if ext := strings.ToLower(path.Ext(path.Base(clean))); deniedExt[ext] {
		return "", fmt.Sprintf("disallowed binary/executable file type %q", ext)
	}

	return clean, ""
}

// isWindowsAbs catches a drive-letter absolute path (e.g. C:\...) so it can't slip past the '/' prefix
// check on a non-Windows host.
func isWindowsAbs(p string) bool {
	return len(p) >= 2 && p[1] == ':' &&
		((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z'))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}

	return ""
}

// str renders a JSON scalar as text so a path/content that arrived as a non-string still yields
// something to validate (and be rejected if unusable).
func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}

	return ""
}

// extractJSON pulls a single JSON object out of a model response that may wrap it in markdown fences
// or prose. It finds the first '{' and its balanced closing '}' (ignoring braces inside string
// literals), then unmarshals that slice.
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
