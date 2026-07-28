package main

import (
	"errors"
	"strings"
	"testing"
)

const subject = "package calc\n\nfunc Add(a, b int) int { return a + b }\n"

// TestRunGoTestPass is the core guarantee: a valid test that passes is reported compiled+passed. This
// actually shells out to `go test` in an offline temp module — the same path the handler uses.
func TestRunGoTestPass(t *testing.T) {
	test := "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n"

	compiled, passed, out := runGoTest(subject, test)
	if !compiled || !passed {
		t.Fatalf("want compiled+passed; got compiled=%v passed=%v\n%s", compiled, passed, out)
	}
}

// TestRunGoTestFail: a test that builds but asserts the wrong thing is compiled=true, passed=false —
// so the guardrail refuses to "keep" it.
func TestRunGoTestFail(t *testing.T) {
	test := "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 6 {\n\t\tt.Fatal(\"expected 6\")\n\t}\n}\n"

	compiled, passed, _ := runGoTest(subject, test)
	if !compiled || passed {
		t.Fatalf("want compiled but not passed; got compiled=%v passed=%v", compiled, passed)
	}
}

// TestRunGoTestBuildFail: a test that doesn't compile is compiled=false — never reported as a pass.
func TestRunGoTestBuildFail(t *testing.T) {
	test := "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tAdd(2) // wrong arg count\n}\n"

	compiled, passed, _ := runGoTest(subject, test)
	if compiled || passed {
		t.Fatalf("want compiled=false for a non-building test; got compiled=%v passed=%v", compiled, passed)
	}
}

// TestClassifyGoTest maps go-test output + exit into (compiled, passed) without shelling out.
func TestClassifyGoTest(t *testing.T) {
	if c, p := classifyGoTest("ok  \tcalc\t0.2s\n", nil); !c || !p {
		t.Error("nil error → compiled+passed")
	}

	if c, p := classifyGoTest("--- FAIL: TestAdd\nFAIL\tcalc\t0.1s\n", errors.New("exit 1")); !c || p {
		t.Error("test failure → compiled, not passed")
	}

	if c, _ := classifyGoTest("FAIL\tcalc [build failed]\n", errors.New("exit 2")); c {
		t.Error("[build failed] → compiled=false")
	}

	// A missing import offline (GOPROXY=off) fails to build before the test line.
	if c, _ := classifyGoTest("subject_test.go:4:8: no required module provides package x\n", errors.New("exit 1")); c {
		t.Error("missing-module offline → compiled=false")
	}
}

// TestDetectLang honours an explicit language, infers Go from shape, else unknown.
func TestDetectLang(t *testing.T) {
	if detectLang("Python", "whatever") != "python" {
		t.Error("explicit language should win (lowercased)")
	}

	if detectLang("", subject) != "go" {
		t.Error("Go source should be inferred")
	}

	if detectLang("", "def f(): pass") != "unknown" {
		t.Error("non-Go with no hint should be unknown")
	}
}

// TestGoPackage extracts the package name so the test lands in the same package.
func TestGoPackage(t *testing.T) {
	if goPackage(subject) != "calc" {
		t.Errorf("goPackage = %q, want calc", goPackage(subject))
	}

	if goPackage("// no package line\nfunc F(){}") != "main" {
		t.Error("goPackage should default to main")
	}
}

// TestStripFences removes a markdown fence the model may have added despite instructions.
func TestStripFences(t *testing.T) {
	got := stripFences("```go\npackage x\nfunc F(){}\n```")
	if got != "package x\nfunc F(){}" {
		t.Errorf("stripFences = %q", got)
	}

	if stripFences("package y") != "package y" {
		t.Error("stripFences should leave unfenced content unchanged")
	}
}

// TestTestFilename picks a sensible per-language filename.
func TestTestFilename(t *testing.T) {
	if testFilename("go") != "subject_test.go" || testFilename("python") != "test_subject.py" {
		t.Error("unexpected test filename")
	}
}

func TestTrimOutput(t *testing.T) {
	if !strings.Contains(trimOutput(strings.Repeat("x", 5000)), "truncated") {
		t.Error("trimOutput should truncate long output")
	}
}
