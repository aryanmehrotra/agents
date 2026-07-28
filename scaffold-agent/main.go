// scaffold-agent — a GoFr 1.58 service that generates a service/module skeleton from a spec. Give it
// a short spec ("an HTTP service that tracks orders with a /orders endpoint") and it returns a set of
// files — main.go, go.mod, a test — ready to drop into a new module. Scaffolding is the build stage of
// the software-development lifecycle: the repetitive first hour of a new service that a model can do
// in seconds.
//
// The catch is that generated code is only worth anything if it's real code. A model will happily emit
// a file that's truncated, syntactically broken, or written to "../../etc/passwd". So the model only
// proposes files; Go disposes of the unsafe and the unusable before anything leaves the service:
//
//   - every path is sanitized — absolute paths, "..", and anything that escapes the scaffold root are
//     rejected, and only a whitelist of source/config extensions is allowed;
//   - every .go file is run through go/format.Source, which parses it: a file that isn't valid Go is
//     rejected with the parse error, and the ones that survive come back correctly gofmt'd.
//
// This is all in-process — no disk writes, no network, no `go build` — so the agent never touches your
// repo and never blocks. You get back a verified set of files (and an honest list of what was
// rejected), which you can write to a new directory yourself.
package main

import (
	"encoding/json"
	"fmt"
	"go/format"
	"path"
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const (
	maxSpecChars = 20000
	maxFiles     = 25
	maxFileBytes = 64 * 1024
)

// allowedExt is the whitelist of file types a scaffold may contain. go.mod / go.sum are matched by
// name separately (they have no dotted extension of their own to key on here).
var allowedExt = map[string]bool{
	".go": true, ".md": true, ".yml": true, ".yaml": true,
	".env": true, ".txt": true, ".json": true, ".mod": true, ".sum": true,
}

// genFile is one file in the scaffold after the guardrail has run. For a .go file, ValidGo reports
// whether it parsed; Content is the gofmt'd source when it did, the model's raw text (plus Error)
// when it didn't.
type genFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Bytes   int    `json:"bytes"`
	ValidGo bool   `json:"valid_go"`
	Error   string `json:"error,omitempty"`
}

// rejectedFile is a file the model proposed that never made it in — an unsafe path or a disallowed
// type. Reported, never silently written.
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
		Text   string `json:"text"` // alias, so the orchestrator (single string) can drive it
		Module string `json:"module"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	spec := strings.TrimSpace(firstNonEmpty(in.Spec, in.Text))
	if spec == "" {
		return map[string]any{
			"error": "provide a `spec` (a short description of the service/module to scaffold); an " +
				"optional `module` sets the Go module path.",
		}, nil
	}

	if len(spec) > maxSpecChars {
		spec = spec[:maxSpecChars]
	}

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a Go scaffolding engine. Given a spec, produce a minimal " +
			"but runnable GoFr (gofr.dev) service skeleton. Reply with ONLY a JSON object: " +
			"{\"module\": string, \"files\": [{\"path\": string, \"content\": string}]}. Always include " +
			"main.go (an `app := gofr.New()` service with the routes the spec implies and stub handlers), " +
			"a go.mod (module the given path, `go 1.24`, require gofr.dev), and one _test.go. Paths are " +
			"relative with forward slashes, no leading slash and no \"..\". No prose outside the JSON."},
		{Role: ai.RoleUser, Content: moduleHint(in.Module) + "Spec:\n" + spec},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	raw, err := extractJSON(resp.Content)
	if err != nil {
		return map[string]any{"error": "model did not return a JSON scaffold: " + err.Error()}, nil
	}

	files, rejected := process(raw["files"])
	module := firstNonEmpty(strings.TrimSpace(str(raw["module"])), strings.TrimSpace(in.Module), "example.com/service")

	allValid, hasMain, hasGoMod := summarize(files)

	return map[string]any{
		"module":   module,
		"files":    files,
		"rejected": rejected,
		"verify": map[string]any{
			"all_go_valid": allValid, // every .go file parsed via go/format.Source
			"has_main":     hasMain,  // a main.go survived
			"has_go_mod":   hasGoMod, // a go.mod survived
		},
		"note": "every path is sanitized against traversal/escape and every .go file is verified with " +
			"go/format.Source (parsed + gofmt'd); rejected files are reported, never written. This is " +
			"in-process only — nothing is written to disk and no repo is touched.",
		"complete": allValid && hasMain && hasGoMod,
	}, nil
}

// moduleHint threads a caller-supplied module path into the prompt when present.
func moduleHint(module string) string {
	if m := strings.TrimSpace(module); m != "" {
		return "Module path: " + m + "\n\n"
	}

	return ""
}

// process is the guardrail: it walks the model's proposed files, drops the unsafe/disallowed ones
// into `rejected`, and verifies each surviving .go file with go/format.Source. It caps the file count
// and the per-file size so a runaway response can't blow up the output.
func process(v any) (files []genFile, rejected []rejectedFile) {
	files = []genFile{}
	rejected = []rejectedFile{}

	arr, ok := v.([]any)
	if !ok {
		return files, rejected
	}

	seen := make(map[string]bool)

	for _, e := range arr {
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

		files = append(files, verifyFile(clean, content))
		if len(files) == maxFiles {
			break
		}
	}

	return files, rejected
}

// verifyFile builds a genFile, running any .go source through go/format.Source — which parses it, so
// a syntactically invalid file is caught here and reported (ValidGo=false, Error set) rather than
// handed back as if it were real code. Non-Go files are passed through untouched.
func verifyFile(cleanPath, content string) genFile {
	f := genFile{Path: cleanPath, Content: content, Bytes: len(content)}

	if path.Ext(cleanPath) != ".go" {
		f.ValidGo = true // not Go source — nothing to parse, don't flag it
		return f
	}

	formatted, err := format.Source([]byte(content))
	if err != nil {
		f.Error = err.Error()
		return f
	}

	f.Content = string(formatted)
	f.Bytes = len(f.Content)
	f.ValidGo = true

	return f
}

// summarize reports whether every .go file is valid and whether the two files that make a skeleton
// runnable — main.go and go.mod — survived the guardrail.
func summarize(files []genFile) (allValid, hasMain, hasGoMod bool) {
	allValid = true

	for _, f := range files {
		if path.Ext(f.Path) == ".go" && !f.ValidGo {
			allValid = false
		}

		if f.Path == "main.go" && f.ValidGo {
			hasMain = true
		}

		if path.Base(f.Path) == "go.mod" {
			hasGoMod = true
		}
	}

	return allValid, hasMain, hasGoMod
}

// safePath sanitizes a model-proposed file path so a scaffold can never write outside its own root.
// It rejects absolute paths and anything that escapes via "..", normalizes slashes, and enforces the
// extension whitelist. Returns the cleaned relative path, or a reason it was rejected.
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

	base := path.Base(clean)
	if base == "go.mod" || base == "go.sum" {
		return clean, ""
	}

	ext := path.Ext(base)
	if ext == "" || !allowedExt[ext] {
		return "", fmt.Sprintf("disallowed file type %q", ext)
	}

	return clean, ""
}

// isWindowsAbs catches a drive-letter absolute path (e.g. C:\...) so it can't slip past the '/'
// prefix check on a non-Windows host.
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
