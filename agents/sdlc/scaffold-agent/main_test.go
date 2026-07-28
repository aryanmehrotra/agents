package main

import (
	"strings"
	"testing"
)

// TestSafePath is the language-agnostic filesystem guardrail: relative in-root paths pass in any
// language; absolute, traversal, drive-letter and binary-type paths are rejected.
func TestSafePath(t *testing.T) {
	ok := map[string]string{
		"main.go":           "main.go",
		"app/main.py":       "app/main.py", // any language, not just Go
		"src/index.ts":      "src/index.ts",
		"Cargo.toml":        "Cargo.toml",
		"Dockerfile":        "Dockerfile", // extensionless is fine now
		"Makefile":          "Makefile",
		"a/b/../handler.rb": "a/handler.rb", // interior ".." that stays in-root is fine
		"./configs/.env":    "configs/.env",
	}
	for in, want := range ok {
		if got, reason := safePath(in); reason != "" || got != want {
			t.Errorf("safePath(%q) = %q, %q; want %q", in, got, reason, want)
		}
	}

	bad := []string{
		"/etc/passwd",       // absolute
		"../escape.py",      // climbs out
		"../../x.go",        // climbs out
		"a/../../b.js",      // climbs out after interior up
		"C:\\Windows\\x.go", // windows absolute
		"payload.exe",       // binary/executable
		"lib.so",            // binary
		"logo.png",          // binary asset
		"",                  // empty
	}
	for _, in := range bad {
		if got, reason := safePath(in); reason == "" {
			t.Errorf("safePath(%q) = %q with no rejection; expected rejection", in, got)
		}
	}
}

// TestVerifyFile syntax-checks the types it can (Go/JSON/YAML) and honestly marks the rest unchecked.
func TestVerifyFile(t *testing.T) {
	// Go: valid is reformatted + Checked+Valid; broken is Checked but not Valid, with an error.
	goOK := verifyFile("main.go", "package main\nfunc main(){x:=1;_=x}")
	if !goOK.Checked || !goOK.Valid || goOK.Error != "" || strings.Contains(goOK.Content, ";_=x") {
		t.Errorf("verifyFile(valid go) = %+v", goOK)
	}

	goBad := verifyFile("bad.go", "package main\nfunc main( {")
	if !goBad.Checked || goBad.Valid || goBad.Error == "" || goBad.Content != "package main\nfunc main( {" {
		t.Errorf("verifyFile(broken go) = %+v", goBad)
	}

	// JSON: parsed.
	if f := verifyFile("package.json", `{"name":"x"}`); !f.Checked || !f.Valid {
		t.Errorf("verifyFile(valid json) = %+v", f)
	}

	if f := verifyFile("bad.json", `{"name":}`); !f.Checked || f.Valid || f.Error == "" {
		t.Errorf("verifyFile(broken json) = %+v", f)
	}

	// YAML: parsed.
	if f := verifyFile("compose.yml", "services:\n  web:\n    image: x\n"); !f.Checked || !f.Valid {
		t.Errorf("verifyFile(valid yaml) = %+v", f)
	}

	if f := verifyFile("bad.yaml", "a:\n  - b\n c: broken indent"); !f.Checked || f.Valid {
		t.Errorf("verifyFile(broken yaml) should be invalid, got %+v", f)
	}

	// A language we don't parse here: returned untouched, honestly marked unchecked-but-not-flagged.
	py := verifyFile("app.py", "def main():\n    pass\n")
	if py.Checked || !py.Valid || py.Content != "def main():\n    pass\n" {
		t.Errorf("verifyFile(python) = %+v; want Checked=false, Valid=true, untouched", py)
	}
}

// TestProcess rejects unsafe/binary/non-text/duplicate files across languages and keeps the good ones.
func TestProcess(t *testing.T) {
	files, rejected := process([]any{
		map[string]any{"path": "app/main.py", "content": "print('hi')\n"},
		map[string]any{"path": "../evil.py", "content": "x"},               // escapes root
		map[string]any{"path": "a.out.bin", "content": "x"},                // binary type
		map[string]any{"path": "notes.txt", "content": "ok\xff\xfebinary"}, // invalid UTF-8 content
		map[string]any{"path": "app/main.py", "content": "dup"},            // duplicate
	})

	if len(files) != 1 || files[0].Path != "app/main.py" {
		t.Fatalf("process kept = %#v, want just app/main.py", files)
	}

	// escape, binary type, non-UTF-8 content, and the duplicate → 4 rejections.
	if len(rejected) != 4 {
		t.Errorf("process rejected = %#v, want 4 (escape/binary/non-text/duplicate)", rejected)
	}
}

// TestSummarize counts checked files and how many failed.
func TestSummarize(t *testing.T) {
	checked, invalid := summarize([]genFile{
		{Path: "main.go", Checked: true, Valid: true},
		{Path: "package.json", Checked: true, Valid: false},
		{Path: "app.py", Checked: false, Valid: true}, // unchecked — not counted
	})

	if checked != 2 || invalid != 1 {
		t.Errorf("summarize = checked %d, invalid %d; want 2, 1", checked, invalid)
	}
}

// TestExtractJSON recovers the object from a fenced/prose-wrapped response.
func TestExtractJSON(t *testing.T) {
	m, err := extractJSON("sure:\n```json\n{\"stack\":\"python fastapi\",\"files\":[]}\n```")
	if err != nil || m["stack"] != "python fastapi" {
		t.Fatalf("extractJSON = %#v, %v", m, err)
	}

	if _, err := extractJSON("no object"); err == nil {
		t.Error("extractJSON(no object) should error")
	}
}
