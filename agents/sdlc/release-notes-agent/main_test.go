package main

import (
	"strings"
	"testing"
)

// TestParsePRsFromText covers the deterministic free-text parse: a leading "#123 " or "123: " reads as
// the PR number, blank lines are skipped, and a line with neither is just a title.
func TestParsePRsFromText(t *testing.T) {
	prs := parsePRsFromText("#123 Add dark mode\n\n456: Fix crash on login\nJust a plain title\n")

	if len(prs) != 3 {
		t.Fatalf("len(prs) = %d, want 3", len(prs))
	}

	if prs[0].Number != 123 || prs[0].Title != "Add dark mode" {
		t.Errorf("prs[0] = %+v", prs[0])
	}

	if prs[1].Number != 456 || prs[1].Title != "Fix crash on login" {
		t.Errorf("prs[1] = %+v", prs[1])
	}

	if prs[2].Number != 0 || prs[2].Title != "Just a plain title" {
		t.Errorf("prs[2] = %+v", prs[2])
	}
}

// TestValidCategoryClosedSet is the core guardrail: only the exact closed set of category keys is
// trusted; anything the model invents or misspells is rejected (empty), never passed through.
func TestValidCategoryClosedSet(t *testing.T) {
	for _, ok := range []string{"breaking", "feature", "fix", "perf", "security", "docs", "chore", "FIX", " docs "} {
		if validCategory(ok) == "" {
			t.Errorf("validCategory(%q) rejected, want accepted", ok)
		}
	}

	for _, bad := range []string{"", "improvement", "misc", "new-feature", "urgent"} {
		if validCategory(bad) != "" {
			t.Errorf("validCategory(%q) = %q, want rejected", bad, validCategory(bad))
		}
	}
}

// TestCategorizeByKeywordAlwaysReturnsClosedSet proves the deterministic fallback never needs the
// model and always lands on one of the closed categories, driven by labels first, then title.
func TestCategorizeByKeywordAlwaysReturnsClosedSet(t *testing.T) {
	cases := []struct {
		pr   prEntry
		want string
	}{
		{prEntry{Title: "drop the legacy v1 API", Labels: []string{"breaking-change"}}, "breaking"},
		{prEntry{Title: "patch a stored XSS in comments", Labels: []string{"security"}}, "security"},
		{prEntry{Title: "shave 40ms off cold start", Labels: []string{"perf"}}, "perf"},
		{prEntry{Title: "fix crash on null user"}, "fix"},
		{prEntry{Title: "add a feature: dark mode toggle"}, "feature"},
		{prEntry{Title: "update the docs README"}, "docs"},
		{prEntry{Title: "bump go.mod dependencies"}, "chore"},
	}

	for _, tc := range cases {
		got := categorizeByKeyword(tc.pr)

		if got != tc.want {
			t.Errorf("categorizeByKeyword(%+v) = %q, want %q", tc.pr, got, tc.want)
		}

		if validCategory(got) == "" {
			t.Errorf("categorizeByKeyword(%+v) = %q is not in the closed set", tc.pr, got)
		}
	}
}

// TestCleanLine strips conventional-commit prefixes, trims a trailing period, and never turns a
// non-empty input into an empty line (the caller relies on this to always have a usable fallback).
func TestCleanLine(t *testing.T) {
	if got := cleanLine("feat: add dark mode."); got != "add dark mode" {
		t.Errorf("cleanLine(feat:) = %q", got)
	}

	if got := cleanLine("  Fix!: crash on login  "); got != "crash on login" {
		t.Errorf("cleanLine(fix!:) = %q", got)
	}

	if got := cleanLine("plain title, no prefix"); got != "plain title, no prefix" {
		t.Errorf("cleanLine(plain) = %q", got)
	}

	long := strings.Repeat("x", maxLineChars+50)
	if got := cleanLine(long); len(got) > maxLineChars+len("…") { // ellipsis is a multi-byte rune
		t.Errorf("cleanLine did not cap length: len=%d", len(got))
	}
}

// TestDedupe drops a repeated (category, line) pair, keeping the first occurrence — so the model
// rewriting two PRs to the same wording doesn't double up a changelog section.
func TestDedupe(t *testing.T) {
	in := []notesEntry{
		{Number: 1, Category: "fix", Line: "fix the login crash"},
		{Number: 2, Category: "fix", Line: "Fix The Login Crash"},     // same, different case
		{Number: 3, Category: "feature", Line: "fix the login crash"}, // different category, kept
	}

	out := dedupe(in)
	if len(out) != 2 {
		t.Fatalf("len(dedupe) = %d, want 2", len(out))
	}

	if out[0].Number != 1 || out[1].Category != "feature" {
		t.Errorf("dedupe kept the wrong entries: %+v", out)
	}
}

// TestBuildNotesOrderAndSections proves the Markdown is assembled deterministically: fixed category
// section order regardless of input order, and entries within a section sorted by PR number.
func TestBuildNotesOrderAndSections(t *testing.T) {
	entries := []notesEntry{
		{Number: 10, Category: "fix", Line: "fix b"},
		{Number: 5, Category: "fix", Line: "fix a"},
		{Number: 1, Category: "feature", Line: "add x"},
	}

	notes := buildNotes(entries, "v1.0.0", "since v0.9.0")

	if !strings.HasPrefix(notes, "# v1.0.0\n\n_since v0.9.0_\n\n") {
		t.Errorf("buildNotes header wrong: %q", notes[:min(60, len(notes))])
	}

	featIdx := strings.Index(notes, "Features")
	fixIdx := strings.Index(notes, "Fixes")

	if featIdx < 0 || fixIdx < 0 || featIdx > fixIdx {
		t.Errorf("Features section should come before Fixes (fixed category order): %q", notes)
	}

	if strings.Index(notes, "fix a") > strings.Index(notes, "fix b") {
		t.Errorf("fix entries not sorted by PR number: %q", notes)
	}
}

// TestBuildNotesEmpty never returns a blank body even with no entries.
func TestBuildNotesEmpty(t *testing.T) {
	notes := buildNotes(nil, "", "")
	if !strings.Contains(notes, "No changes") {
		t.Errorf("buildNotes(empty) = %q, want a No changes note", notes)
	}
}

// TestParseModelPRsFencedAndAbsent recovers a fenced JSON array from a model response and reports an
// error only when there is truly no array to find.
func TestParseModelPRsFencedAndAbsent(t *testing.T) {
	arr, err := parseModelPRs("here:\n```json\n[{\"ref\":\"#1\",\"category\":\"fix\",\"line\":\"x\"}]\n```")
	if err != nil || len(arr) != 1 || arr[0].Ref != "#1" {
		t.Errorf("parseModelPRs(fenced) = %v, %v", arr, err)
	}

	if _, err := parseModelPRs("no array here"); err == nil {
		t.Error("parseModelPRs(none) should error")
	}
}

// TestRefFor is the deterministic id a PR is matched by: its number when present, else its position.
func TestRefFor(t *testing.T) {
	if got := refFor(prEntry{Number: 42}, 3); got != "#42" {
		t.Errorf("refFor(numbered) = %q", got)
	}

	if got := refFor(prEntry{}, 3); got != "idx3" {
		t.Errorf("refFor(unnumbered) = %q", got)
	}
}
