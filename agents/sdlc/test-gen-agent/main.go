// test-gen-agent — a GoFr 1.58 service that writes unit tests for a piece of code, and only keeps
// them if they actually compile and pass. Give it a source file and it returns a generated test —
// but, crucially, for Go it doesn't just hand back what the model wrote: it compiles and RUNS the test
// against the source in an isolated temp module, and reports whether it built and passed. Test
// generation is a stage of the software-development lifecycle where an unverified answer is worse than
// none — a test that doesn't compile, or one that passes vacuously, is a trap.
//
// So the model only proposes a test; Go disposes: the test is written next to the source in a throwaway
// module and run with `go test`, fully offline (GOPROXY=off, a hard timeout, no toolchain download), so
// the agent can't hang or reach the network. A test that fails to build or fails to pass is returned
// with its output and marked NOT kept — you see exactly why, and you never get a green result you can't
// trust. Other languages get a generated test too, honestly marked "not executed" (only Go is run
// here). Nothing is written to your repo; the temp module is created and removed per request.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const (
	maxCodeChars = 30000
	testTimeout  = 30 * time.Second
)

func main() {
	app := gofr.New()

	app.POST("/testgen", testgen)

	app.Run()
}

func testgen(c *gofr.Context) (any, error) {
	var in struct {
		Code     string `json:"code"`
		Text     string `json:"text"` // alias (orchestrator single-string)
		Language string `json:"language"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	code := strings.TrimSpace(firstNonEmpty(in.Code, in.Text))
	if code == "" {
		return map[string]any{
			"error": "provide `code` (a source file to generate tests for); optional `language` " +
				"(defaults to Go, which is compiled and run; other languages are generated but not executed).",
		}, nil
	}

	if len(code) > maxCodeChars {
		code = code[:maxCodeChars]
	}

	lang := detectLang(in.Language, code)

	resp, err := c.LLM().Chat(c, []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a precise test engineer. Write a thorough unit test for " +
			"the given code in the SAME language. Cover the main behaviour and the obvious edge cases. " +
			"Reply with ONLY the test file's source — no prose, no markdown fences. For Go, use the same " +
			"package as the code and the standard `testing` package; do not add third-party imports."},
		{Role: ai.RoleUser, Content: fmt.Sprintf("Language: %s\nPackage: %s\n\nCode:\n%s", lang, goPackage(code), code)},
	}, ai.WithTemperature(0))
	if err != nil {
		return nil, err
	}

	testContent := stripFences(resp.Content)

	out := map[string]any{
		"language": lang,
		"test": map[string]any{
			"filename": testFilename(lang),
			"content":  testContent,
		},
	}

	if lang != "go" {
		out["verify"] = map[string]any{"executed": false}
		out["kept"] = false
		out["note"] = "test generated but NOT executed — only Go tests are compiled and run here. " +
			"Review before use."

		return out, nil
	}

	compiled, passed, output := runGoTest(code, testContent)

	out["verify"] = map[string]any{
		"executed": true,
		"compiled": compiled,
		"passed":   passed,
		"output":   trimOutput(output),
	}
	out["kept"] = compiled && passed
	out["note"] = "the test was compiled and run against the source in an isolated, offline temp " +
		"module; it is only 'kept' if it built and passed. Nothing was written to your repo."

	return out, nil
}

// runGoTest is the guardrail: it writes the source and the generated test into a throwaway module and
// runs `go test`, fully offline and time-bounded, then reports whether it compiled and passed. This is
// what makes a green result trustworthy — the test was actually executed, not just parsed.
func runGoTest(sourceContent, testContent string) (compiled, passed bool, output string) {
	dir, err := os.MkdirTemp("", "testgen-*")
	if err != nil {
		return false, false, "could not create temp dir: " + err.Error()
	}
	defer os.RemoveAll(dir)

	files := map[string]string{
		"go.mod":          "module testgen\n\ngo 1.24\n",
		"subject.go":      sourceContent,
		"subject_test.go": testContent,
	}
	for name, content := range files {
		if werr := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); werr != nil {
			return false, false, "could not write " + name + ": " + werr.Error()
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "test", "./...")
	cmd.Dir = dir
	// Fully offline and self-contained: no module downloads, no toolchain switch, no parent workspace,
	// and no cgo (one less way for generated code to reach into C). NOTE: this bounds module-fetching
	// and wall-time, NOT what the compiled test can do — it runs with this process's privileges and is
	// NOT sandboxed. Only run this on source you trust. See the README's security note.
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "GOWORK=off", "CGO_ENABLED=0")
	// The generated test runs as a child of `go test`; put it in its own process group and, on timeout,
	// kill the whole GROUP so a runaway test binary can't outlive the deadline as an orphan.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}

		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	raw, runErr := cmd.CombinedOutput()
	output = string(raw)

	if ctx.Err() == context.DeadlineExceeded {
		return false, false, fmt.Sprintf("test run timed out after %s (possible infinite loop in the generated test)", testTimeout)
	}

	compiled, passed = classifyGoTest(output, runErr)

	return compiled, passed, output
}

// classifyGoTest turns `go test` output + exit error into (compiled, passed). A build failure is
// tagged "[build failed]" by the go tool; a missing import offline (GOPROXY=off) fails before the build
// with a module/package-resolution error. If none of those appear, the package compiled and a non-nil
// exit means a test failed.
func classifyGoTest(output string, runErr error) (compiled, passed bool) {
	if runErr == nil {
		return true, true // built and every test passed
	}

	buildFail := strings.Contains(output, "[build failed]") ||
		strings.Contains(output, "build constraints exclude") ||
		strings.Contains(output, "no required module provides") ||
		strings.Contains(output, "cannot find package") ||
		strings.Contains(output, "cannot find module")

	return !buildFail, false
}

// detectLang uses an explicit language when given, else infers Go from the code's shape, else marks it
// unknown (still generates a test, but won't try to run it).
func detectLang(explicit, code string) string {
	if l := strings.ToLower(strings.TrimSpace(explicit)); l != "" {
		return l
	}

	if strings.Contains(code, "package ") && strings.Contains(code, "func ") {
		return "go"
	}

	return "unknown"
}

// goPackage extracts the package name from Go source so the generated test lands in the same package.
func goPackage(code string) string {
	for _, line := range strings.Split(code, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "package "))
		}
	}

	return "main"
}

func testFilename(lang string) string {
	switch lang {
	case "go":
		return "subject_test.go"
	case "python":
		return "test_subject.py"
	case "javascript", "typescript", "node":
		return "subject.test.js"
	default:
		return "subject_test." + lang
	}
}

// stripFences removes a leading/trailing markdown code fence if the model added one despite being
// asked not to, so the returned content is clean source.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}

	lines := strings.Split(s, "\n")
	if len(lines) > 0 {
		lines = lines[1:] // drop the opening ```lang line
	}

	if n := len(lines); n > 0 && strings.TrimSpace(lines[n-1]) == "```" {
		lines = lines[:n-1]
	}

	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// trimOutput bounds the captured test output so a noisy run can't dominate the response.
func trimOutput(s string) string {
	s = strings.TrimSpace(s)

	const max = 4000
	if len(s) > max {
		return s[:max] + "\n… (truncated)"
	}

	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}

	return ""
}
