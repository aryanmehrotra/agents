package main

import (
	"strings"
	"testing"
)

// TestDiffText produces a line diff with correct add/remove counts, and collapses an unchanged run
// that sits between two changes.
func TestDiffText(t *testing.T) {
	// Two changes (top and bottom) with a long unchanged middle → the middle collapses to "...".
	old := "func getUser() {}\nc\nd\ne\nf\ng\nh\ni\nreturn OLD\n"
	neu := "func fetchUser() {}\nc\nd\ne\nf\ng\nh\ni\nreturn NEW\n"

	diff, added, removed := diffText(old, neu)
	if added != 2 || removed != 2 {
		t.Fatalf("diff counts = +%d/-%d, want +2/-2:\n%s", added, removed, diff)
	}

	if !strings.Contains(diff, "- func getUser() {}") || !strings.Contains(diff, "+ func fetchUser() {}") {
		t.Errorf("diff missing the rename:\n%s", diff)
	}

	if !strings.Contains(diff, "- return OLD") || !strings.Contains(diff, "+ return NEW") {
		t.Errorf("diff missing the second change:\n%s", diff)
	}

	// The unchanged middle (e/f/g between the two changes' context windows) is collapsed.
	if !strings.Contains(diff, "...") {
		t.Errorf("diff should collapse the unchanged run between the two changes:\n%s", diff)
	}
}

// TestDiffTextIdentical reports no changes for identical input.
func TestDiffTextIdentical(t *testing.T) {
	_, added, removed := diffText("a\nb\nc\n", "a\nb\nc\n")
	if added != 0 || removed != 0 {
		t.Errorf("identical diff = +%d/-%d, want 0/0", added, removed)
	}
}

// TestVerifyType re-parses rewritten files by type and reports a broken one.
func TestVerifyType(t *testing.T) {
	if ch, ok, _ := verifyType("x.go", "package x\nfunc F(){}"); !ch || !ok {
		t.Error("valid go should verify")
	}

	if ch, ok, msg := verifyType("x.go", "package x\nfunc F( {"); !ch || ok || msg == "" {
		t.Error("broken go should be checked+invalid with a message")
	}

	if ch, ok, _ := verifyType("c.json", `{"a":1}`); !ch || !ok {
		t.Error("valid json should verify")
	}

	if ch, ok, _ := verifyType("c.json", `{"a":}`); !ch || ok {
		t.Error("broken json should be invalid")
	}

	// A type we can't parse: not checked, not flagged.
	if ch, ok, _ := verifyType("app.py", "def f(: pass"); ch || !ok {
		t.Error("python should be unchecked (checked=false, valid=true)")
	}
}

// TestApplyRewrites is the core guarantee: a valid rewrite is applied + diffed; a rewrite that breaks
// a parseable file is REJECTED and the original is kept; an unchanged file passes through.
func TestApplyRewrites(t *testing.T) {
	in := []inFile{
		{Path: "a.go", Content: "package a\nfunc Old(){}\n"},
		{Path: "b.go", Content: "package b\nfunc Keep(){}\n"},
		{Path: "c.go", Content: "package c\nfunc Fine(){}\n"},
	}
	originals := map[string]string{
		"a.go": "package a\nfunc Old(){}\n",
		"b.go": "package b\nfunc Keep(){}\n",
		"c.go": "package c\nfunc Fine(){}\n",
	}
	rewrites := map[string]string{
		"a.go": "package a\nfunc New(){}\n",   // valid change
		"b.go": "package b\nfunc Broken( {\n", // rewrite breaks the file → must be rejected
		// c.go not rewritten → unchanged
	}

	files, changed, rejected := applyRewrites(in, originals, rewrites)

	if changed != 1 || rejected != 1 {
		t.Fatalf("applyRewrites = %d changed, %d rejected; want 1 and 1", changed, rejected)
	}

	byPath := map[string]outFile{}
	for _, f := range files {
		byPath[f.Path] = f
	}

	if a := byPath["a.go"]; !a.Changed || a.Rejected || !strings.Contains(a.Content, "func New()") {
		t.Errorf("a.go should be changed to the new content, got %+v", a)
	}

	if b := byPath["b.go"]; !b.Rejected || b.Changed || b.Content != originals["b.go"] {
		t.Errorf("b.go broken rewrite must be rejected with the ORIGINAL kept, got %+v", b)
	}

	if c := byPath["c.go"]; c.Changed || c.Rejected {
		t.Errorf("c.go should be unchanged, got %+v", c)
	}
}

// TestCollectFiles validates/dedupes inputs and reports what was skipped.
func TestCollectFiles(t *testing.T) {
	originals, skipped := collectFiles([]inFile{
		{Path: "ok.go", Content: "x"},
		{Path: "../escape.go", Content: "x"},
		{Path: "ok.go", Content: "dup"},
	})

	if len(originals) != 1 || originals["ok.go"] != "x" {
		t.Errorf("collectFiles kept = %#v, want just ok.go=x", originals)
	}

	if len(skipped) != 2 {
		t.Errorf("collectFiles skipped = %#v, want 2 (escape, duplicate)", skipped)
	}
}

// TestCollectFilesCap confirms the file cap is on ACCEPTED files, not input position: a valid file
// after many skipped (invalid) entries must still be accepted.
func TestCollectFilesCap(t *testing.T) {
	in := []inFile{}
	for i := 0; i < maxFiles+5; i++ {
		in = append(in, inFile{Path: "/abs/skip.go", Content: "x"}) // all rejected (absolute)
	}

	in = append(in, inFile{Path: "real.go", Content: "package x"})

	originals, _ := collectFiles(in)
	if _, ok := originals["real.go"]; !ok {
		t.Error("a valid file after many skipped entries must still be accepted (cap is on accepted count)")
	}
}

// TestIgnoredRewrites reports model-returned paths that weren't in the input set, sorted.
func TestIgnoredRewrites(t *testing.T) {
	ig := ignoredRewrites(
		map[string]string{"a.go": ""},
		map[string]string{"a.go": "x", "z.go": "w", "new.go": "y"},
	)
	if len(ig) != 2 || ig[0] != "new.go" || ig[1] != "z.go" {
		t.Errorf("ignoredRewrites = %v, want [new.go z.go]", ig)
	}
}

// TestSafePath rejects traversal/absolute paths (no extension policy — codemods touch any source).
func TestSafePath(t *testing.T) {
	if _, r := safePath("pkg/handler.go"); r != "" {
		t.Errorf("safePath(relative) rejected: %q", r)
	}

	for _, bad := range []string{"/etc/x", "../x", "a/../../b", "C:\\x"} {
		if _, r := safePath(bad); r == "" {
			t.Errorf("safePath(%q) should be rejected", bad)
		}
	}
}
