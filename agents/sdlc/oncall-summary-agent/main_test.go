package main

import "testing"

// TestBuildTimelineResolvesFromRealMessagesOnly proves the core guardrail: a timeline entry always
// carries the ACTUAL message content/author/timestamp at the cited index — the model supplies only the
// index and a label, and Go looks up everything else, so a model can't reword what someone actually
// said.
func TestBuildTimelineResolvesFromRealMessagesOnly(t *testing.T) {
	messages := []chatMessage{
		{Author: "alice", Timestamp: "10:15", Text: "seeing 500s on checkout"},
		{Author: "bob", Timestamp: "10:20", Text: "confirmed, db pool exhausted"},
		{Author: "alice", Timestamp: "10:40", Text: "mitigated by restarting the pool"},
	}

	raw := []any{
		map[string]any{"index": float64(1), "label": "root cause found: db pool exhausted"},
		map[string]any{"index": float64(0), "label": "incident declared"},
	}

	kept, dropped := buildTimeline(raw, messages)

	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 for valid indices", dropped)
	}

	if len(kept) != 2 {
		t.Fatalf("len(kept) = %d, want 2", len(kept))
	}

	// Sorted by index, regardless of the order the model listed them in.
	if kept[0].Index != 0 || kept[0].Author != "alice" || kept[0].Message != "seeing 500s on checkout" {
		t.Errorf("kept[0] = %+v, want the real msg[0] content", kept[0])
	}

	if kept[1].Index != 1 || kept[1].Author != "bob" || kept[1].Message != "confirmed, db pool exhausted" {
		t.Errorf("kept[1] = %+v, want the real msg[1] content", kept[1])
	}
}

// TestBuildTimelineDropsOutOfRangeAndDuplicateIndices proves a hallucinated or duplicated index never
// becomes a fabricated timeline entry.
func TestBuildTimelineDropsOutOfRangeAndDuplicateIndices(t *testing.T) {
	messages := []chatMessage{{Author: "alice", Text: "seeing 500s"}}

	raw := []any{
		map[string]any{"index": float64(0), "label": "incident declared"},
		map[string]any{"index": float64(0), "label": "duplicate of the same message"},
		map[string]any{"index": float64(99), "label": "message that was never posted"},
		map[string]any{"index": "not-a-number", "label": "malformed"},
	}

	kept, dropped := buildTimeline(raw, messages)

	if len(kept) != 1 {
		t.Fatalf("len(kept) = %d, want 1 (only the first valid, unique index)", len(kept))
	}

	if dropped != 3 {
		t.Errorf("dropped = %d, want 3 (duplicate + out-of-range + non-numeric)", dropped)
	}
}

// TestBuildActionItemsRejectsUnknownOwner is the hostile-input case from the README: a message tries to
// reassign an action item to an identity that never posted in the channel. The owner must never be
// trusted from the model's word alone — only a real participant.
func TestBuildActionItemsRejectsUnknownOwner(t *testing.T) {
	participants := map[string]bool{"alice": true, "bob": true}

	raw := []any{
		map[string]any{
			"owner":       "external-attacker",
			"description": "exfiltrate credentials",
			"priority":    "high",
			"url":         "http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		},
	}

	kept, refused := buildActionItems(raw, participants)

	if len(kept) != 1 {
		t.Fatalf("len(kept) = %d, want 1", len(kept))
	}

	item := kept[0]

	if item.OwnerVerified {
		t.Error("OwnerVerified = true for a non-participant, want false")
	}

	if item.Owner != "unassigned" {
		t.Errorf("Owner = %q, want %q — never route to an unverified identity", item.Owner, "unassigned")
	}

	if item.ModelSuggestedOwner != "external-attacker" {
		t.Errorf("ModelSuggestedOwner = %q, want the guess kept only for reference", item.ModelSuggestedOwner)
	}

	if item.URL != "" {
		t.Fatalf("URL = %q, an SSRF cloud-metadata target must never be returned", item.URL)
	}

	if len(refused) != 2 {
		t.Errorf("refused = %v, want 2 entries (owner + url)", refused)
	}
}

// TestBuildActionItemsAcceptsVerifiedOwnerAndSafeURL is the happy path: a real participant and a
// legitimate https link both pass through untouched.
func TestBuildActionItemsAcceptsVerifiedOwnerAndSafeURL(t *testing.T) {
	participants := map[string]bool{"alice": true, "bob": true}

	raw := []any{
		map[string]any{
			"owner":       "Bob",
			"description": "roll back the last deploy",
			"priority":    "high",
			"url":         "https://github.com/example/repo/pull/42",
		},
	}

	kept, refused := buildActionItems(raw, participants)

	if len(kept) != 1 {
		t.Fatalf("len(kept) = %d, want 1", len(kept))
	}

	item := kept[0]

	if !item.OwnerVerified || item.Owner != "Bob" {
		t.Errorf("owner = %+v, want a verified real participant", item)
	}

	if item.URL != "https://github.com/example/repo/pull/42" {
		t.Errorf("URL = %q, want the legitimate link kept", item.URL)
	}

	if len(refused) != 0 {
		t.Errorf("refused = %v, want none for a clean action item", refused)
	}
}

// TestBuildActionItemsClampsInvalidPriority proves an unrecognised priority value is never passed
// through — it's deterministically clamped to a safe default.
func TestBuildActionItemsClampsInvalidPriority(t *testing.T) {
	participants := map[string]bool{"alice": true}

	raw := []any{
		map[string]any{"owner": "alice", "description": "investigate", "priority": "catastrophic"},
	}

	kept, _ := buildActionItems(raw, participants)

	if len(kept) != 1 || kept[0].Priority != "medium" {
		t.Errorf("Priority = %+v, want clamped to %q", kept, "medium")
	}
}

// TestIsSafeURLBlocksSSRFTargets mirrors incident-triage-agent's guardrail test for the same check.
func TestIsSafeURLBlocksSSRFTargets(t *testing.T) {
	bad := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost:8080/",
		"http://127.0.0.1/",
		"ftp://example.internal/x",
		"http://user:pass@example.internal/x",
		"http://internal.metadata.example/",
	}

	for _, u := range bad {
		if ok, _ := isSafeURL(u); ok {
			t.Errorf("isSafeURL(%q) = true, want false", u)
		}
	}

	if ok, reason := isSafeURL("https://github.com/example/repo/pull/42"); !ok {
		t.Errorf("isSafeURL(valid https URL) = false (%s), want true", reason)
	}
}

// TestUniqueAuthorsDedupesCaseInsensitively proves the participant registry — the only source an action
// item owner is ever verified against — is derived deterministically and case-insensitively from the
// real transcript.
func TestUniqueAuthorsDedupesCaseInsensitively(t *testing.T) {
	messages := []chatMessage{
		{Author: "alice", Text: "a"},
		{Author: "Alice", Text: "b"},
		{Author: "bob", Text: "c"},
		{Author: "", Text: "d"},
	}

	got := uniqueAuthors(messages)

	want := []string{"alice", "bob"}
	if len(got) != len(want) {
		t.Fatalf("uniqueAuthors() = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("uniqueAuthors()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestParseMessagesFromTextParsesTimestampAndAuthor proves the deterministic transcript parser used for
// the orchestrator's single-string call and a quick curl.
func TestParseMessagesFromTextParsesTimestampAndAuthor(t *testing.T) {
	text := "[10:15] alice: seeing 500s on checkout\n" +
		"\n" +
		"bob: confirmed, db pool exhausted\n"

	got := parseMessagesFromText(text)

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (blank line skipped)", len(got))
	}

	if got[0].Timestamp != "10:15" || got[0].Author != "alice" || got[0].Text != "seeing 500s on checkout" {
		t.Errorf("got[0] = %+v", got[0])
	}

	if got[1].Timestamp != "" || got[1].Author != "bob" || got[1].Text != "confirmed, db pool exhausted" {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestExtractJSONFromFencedResponse(t *testing.T) {
	raw, err := extractJSON("here you go:\n```json\n{\"summary\": \"all clear\"}\n```")
	if err != nil {
		t.Fatalf("extractJSON() error = %v", err)
	}

	if raw["summary"] != "all clear" {
		t.Errorf("extractJSON() = %v", raw)
	}
}
