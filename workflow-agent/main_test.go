package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParsePlan is the guardrail on the model's plan: a strict JSON array of 1..maxSteps non-empty
// task strings (with or without a markdown fence), else refused.
func TestParsePlan(t *testing.T) {
	ok, err := parsePlan(`["research the release", "summarise it in 3 bullets"]`)
	if err != nil || len(ok) != 2 || ok[0] != "research the release" {
		t.Fatalf("parsePlan(valid) = %v, %v", ok, err)
	}

	fenced, err := parsePlan("```json\n[\"a\", \"\", \"  b  \"]\n```")
	if err != nil || len(fenced) != 2 || fenced[1] != "b" {
		t.Errorf("parsePlan(fenced, blanks trimmed) = %v, %v", fenced, err)
	}

	bad := []string{
		"sure, here's a plan!",          // not JSON
		`{"step":"x"}`,                  // not an array
		`[]`,                            // empty
		`["   ", ""]`,                   // no usable steps
		`["1","2","3","4","5","6","7"]`, // exceeds maxSteps (6)
	}
	for _, in := range bad {
		if _, err := parsePlan(in); err == nil {
			t.Errorf("parsePlan(%q) expected error, got nil", in)
		}
	}
}

// TestParseAssistant unwraps the orchestrator's response envelope into routed_to + a readable output,
// handles the graceful-degradation case as an error, and tolerates an unexpected shape.
func TestParseAssistant(t *testing.T) {
	body := []byte(`{"data":{"route":"research","routed_to":"research-agent",
		"response":{"data":{"answer":"Go 1.26 shipped generics improvements","sources":[]}}}}`)

	routed, out, err := parseAssistant(body)
	if err != nil {
		t.Fatalf("parseAssistant() error = %v", err)
	}
	if routed != "research-agent" {
		t.Errorf("routed_to = %q, want research-agent", routed)
	}
	if !strings.Contains(out, "generics improvements") {
		t.Errorf("output = %q, want the specialist's answer", out)
	}

	// Degraded specialist → surfaced as an error.
	deg := []byte(`{"data":{"routed_to":"sql-agent","degraded":true,"error":"the sql-agent is temporarily unavailable"}}`)
	if _, _, err := parseAssistant(deg); err == nil {
		t.Error("parseAssistant(degraded) should return an error")
	}

	// Unexpected shape → hand back the raw text, no crash.
	if _, out, err := parseAssistant([]byte("not json")); err != nil || out != "not json" {
		t.Errorf("parseAssistant(raw) = %q, %v", out, err)
	}
}

// TestReadableOutput prefers common answer-bearing keys, else compact JSON.
func TestReadableOutput(t *testing.T) {
	if got := readableOutput(json.RawMessage(`{"tl_dr":"short summary","key_points":["a"]}`)); got != "short summary" {
		t.Errorf("readableOutput(tl_dr) = %q", got)
	}

	got := readableOutput(json.RawMessage(`{"count":3,"items":["x"]}`))
	if !strings.Contains(got, "count") {
		t.Errorf("readableOutput(no answer key) should fall back to JSON, got %q", got)
	}
}

// TestAppendContext threads step output forward but truncates a long excerpt and stops at the cap.
func TestAppendContext(t *testing.T) {
	var b strings.Builder
	appendContext(&b, 1, strings.Repeat("x", maxExcerpt+500))
	if !strings.Contains(b.String(), "…") {
		t.Error("appendContext should truncate an over-long output with an ellipsis")
	}

	var b2 strings.Builder
	b2.WriteString(strings.Repeat("y", maxContextChars))
	appendContext(&b2, 2, "this should be dropped, context is already full")
	if strings.Contains(b2.String(), "dropped") {
		t.Error("appendContext should not exceed the context cap")
	}
}
