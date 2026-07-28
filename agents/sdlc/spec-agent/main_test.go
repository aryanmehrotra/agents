package main

import (
	"strings"
	"testing"
)

// TestStringList is the list guardrail: an array in, trimmed + de-blanked + de-duplicated (case
// insensitively, first-seen order kept) + capped, and a non-array yields an empty list, not a panic.
func TestStringList(t *testing.T) {
	got := stringList([]any{"  build the API  ", "", "Build the API", "ship it"})
	if len(got) != 2 || got[0] != "build the API" || got[1] != "ship it" {
		t.Fatalf("stringList dedupe/trim = %#v", got)
	}

	if l := stringList("not an array"); len(l) != 0 {
		t.Errorf("stringList(non-array) = %#v, want empty", l)
	}

	// A number mixed into the list is rendered, not dropped.
	if l := stringList([]any{42.0}); len(l) != 1 || l[0] != "42" {
		t.Errorf("stringList([42]) = %#v, want [\"42\"]", l)
	}

	// The cap holds.
	big := make([]any, maxItems+5)
	for i := range big {
		big[i] = string(rune('a'+i)) + "-item"
	}

	if l := stringList(big); len(l) != maxItems {
		t.Errorf("stringList over cap = %d items, want %d", len(l), maxItems)
	}
}

// TestTaskList accepts both {title, detail} objects and bare strings, drops untitled tasks, and
// de-duplicates by title.
func TestTaskList(t *testing.T) {
	got := taskList([]any{
		map[string]any{"title": "wire the route", "detail": "add to orchestrator"},
		map[string]any{"detail": "no title — dropped"},
		"flatten me", // a bare string becomes a titled task
		"Wire the route",
	})

	if len(got) != 2 {
		t.Fatalf("taskList = %#v, want 2 tasks", got)
	}

	if got[0].Title != "wire the route" || got[0].Detail != "add to orchestrator" {
		t.Errorf("taskList[0] = %#v", got[0])
	}

	if got[1].Title != "flatten me" {
		t.Errorf("taskList[1] = %#v, want a titled task from the bare string", got[1])
	}
}

// TestGate enforces the minimum bar: a real summary, ≥1 acceptance criterion, ≥1 task.
func TestGate(t *testing.T) {
	full := spec{
		Summary:            "add a spec endpoint",
		AcceptanceCriteria: []string{"POST /spec returns a spec"},
		Tasks:              []task{{Title: "write handler"}},
	}
	if ok, reasons := gate(full); !ok || len(reasons) != 0 {
		t.Errorf("gate(full) = %v, %v; want complete", ok, reasons)
	}

	thin := spec{Summary: ""}

	ok, reasons := gate(thin)
	if ok {
		t.Error("gate(thin) should be incomplete")
	}

	if len(reasons) != 3 {
		t.Errorf("gate(thin) reasons = %v, want 3 (summary, criteria, tasks)", reasons)
	}
}

// TestNormalize threads the caller's title in as a fallback and cleans the model's raw object.
func TestNormalize(t *testing.T) {
	s := normalize("Caller Title", map[string]any{
		"summary":             "  do the thing  ",
		"acceptance_criteria": []any{"it works", "it works"},
		"tasks":               []any{map[string]any{"title": "build"}},
	})

	if s.Title != "Caller Title" {
		t.Errorf("title fallback = %q, want caller's", s.Title)
	}

	if s.Summary != "do the thing" {
		t.Errorf("summary = %q, want trimmed", s.Summary)
	}

	if len(s.AcceptanceCriteria) != 1 {
		t.Errorf("acceptance_criteria = %#v, want deduped to 1", s.AcceptanceCriteria)
	}
}

// TestCleanLine truncates an over-long line with an ellipsis.
func TestCleanLine(t *testing.T) {
	if got := cleanLine(strings.Repeat("x", maxFieldChars+50)); !strings.HasSuffix(got, "…") {
		t.Errorf("cleanLine should truncate an over-long line, got len %d", len(got))
	}
}

// TestExtractJSON pulls the object out of a fenced / prose-wrapped response and errors when there's none.
func TestExtractJSON(t *testing.T) {
	m, err := extractJSON("here you go:\n```json\n{\"summary\": \"hi {not a brace}\"}\n```")
	if err != nil || m["summary"] != "hi {not a brace}" {
		t.Fatalf("extractJSON(fenced) = %#v, %v", m, err)
	}

	if _, err := extractJSON("no object here"); err == nil {
		t.Error("extractJSON(no object) should error")
	}
}
