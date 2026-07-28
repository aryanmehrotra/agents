package main

import (
	"strings"
	"testing"
)

// TestSafePath is the filesystem guardrail: relative in-root paths pass (cleaned); absolute,
// traversal, drive-letter and disallowed-type paths are rejected.
func TestSafePath(t *testing.T) {
	ok := map[string]string{
		"main.go":                "main.go",
		"go.mod":                 "go.mod",
		"handler/orders.go":      "handler/orders.go",
		"./configs/.env":         "configs/.env",
		"a/b/../orders.go":       "a/orders.go", // interior ".." that stays in-root is fine
		"README.md":              "README.md",
	}
	for in, want := range ok {
		if got, reason := safePath(in); reason != "" || got != want {
			t.Errorf("safePath(%q) = %q, %q; want %q", in, got, reason, want)
		}
	}

	bad := []string{
		"/etc/passwd",         // absolute
		"../escape.go",        // climbs out
		"../../x.go",          // climbs out
		"a/../../b.go",        // climbs out after interior up
		"C:\\Windows\\x.go",   // windows absolute
		"secrets.pem",         // disallowed extension
		"Makefile",            // no extension, not go.mod
		"",                    // empty
	}
	for _, in := range bad {
		if got, reason := safePath(in); reason == "" {
			t.Errorf("safePath(%q) = %q with no rejection; expected rejection", in, got)
		}
	}
}

// TestVerifyFile gofmt-verifies Go source: valid code is reformatted and flagged valid; broken code
// is flagged invalid with the parse error; non-Go files pass through untouched.
func TestVerifyFile(t *testing.T) {
	valid := verifyFile("main.go", "package main\nfunc main(){x:=1;_=x}")
	if !valid.ValidGo || valid.Error != "" {
		t.Fatalf("verifyFile(valid) = %+v", valid)
	}

	if !strings.Contains(valid.Content, "func main()") || strings.Contains(valid.Content, ";_=x") {
		t.Errorf("verifyFile(valid) content not gofmt'd: %q", valid.Content)
	}

	broken := verifyFile("bad.go", "package main\nfunc main( {")
	if broken.ValidGo || broken.Error == "" {
		t.Errorf("verifyFile(broken) should be invalid with an error, got %+v", broken)
	}

	// content preserved so the caller can see what the model produced
	if broken.Content != "package main\nfunc main( {" {
		t.Errorf("verifyFile(broken) should keep raw content, got %q", broken.Content)
	}

	nonGo := verifyFile("go.mod", "module x\n\ngo 1.24\n")
	if !nonGo.ValidGo {
		t.Errorf("verifyFile(go.mod) should pass through as valid, got %+v", nonGo)
	}
}

// TestProcess rejects unsafe/disallowed/duplicate files and keeps the good ones, verifying Go along
// the way.
func TestProcess(t *testing.T) {
	files, rejected := process([]any{
		map[string]any{"path": "main.go", "content": "package main\nfunc main(){}"},
		map[string]any{"path": "../evil.go", "content": "package x"},
		map[string]any{"path": "secrets.pem", "content": "..."},
		map[string]any{"path": "main.go", "content": "package main"}, // duplicate
	})

	if len(files) != 1 || files[0].Path != "main.go" {
		t.Fatalf("process kept = %#v, want just main.go", files)
	}

	if len(rejected) != 3 {
		t.Errorf("process rejected = %#v, want 3 (escape, bad ext, duplicate)", rejected)
	}
}

// TestSummarize reports overall validity and the presence of the two files a skeleton needs.
func TestSummarize(t *testing.T) {
	allValid, hasMain, hasGoMod := summarize([]genFile{
		{Path: "main.go", ValidGo: true},
		{Path: "go.mod", ValidGo: true},
		{Path: "broken.go", ValidGo: false},
	})

	if allValid {
		t.Error("summarize: a broken .go file should make all_go_valid false")
	}

	if !hasMain || !hasGoMod {
		t.Errorf("summarize: hasMain=%v hasGoMod=%v, want both true", hasMain, hasGoMod)
	}
}

// TestExtractJSON recovers the object from a fenced/prose-wrapped response.
func TestExtractJSON(t *testing.T) {
	m, err := extractJSON("sure:\n```json\n{\"module\":\"x\",\"files\":[]}\n```")
	if err != nil || m["module"] != "x" {
		t.Fatalf("extractJSON = %#v, %v", m, err)
	}

	if _, err := extractJSON("no object"); err == nil {
		t.Error("extractJSON(no object) should error")
	}
}
