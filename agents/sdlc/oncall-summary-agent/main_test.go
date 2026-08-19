package main

import "testing"

// TestGroundTimelineDropsFabricatedEvents proves the core guardrail: a timeline entry whose quote does
// not literally appear in the transcript is dropped, not kept as "probably true".
func TestGroundTimelineDropsFabricatedEvents(t *testing.T) {
	transcript := "[10:02] alice: investigating checkout 500s\n[10:05] bob: found it, db pool exhausted"

	raw := []any{
		map[string]any{"time": "10:02", "event": "alice starts investigating", "quote": "investigating checkout 500s"},
		map[string]any{"time": "10:10", "event": "a fabricated event", "quote": "the sky is falling"},
	}

	kept, dropped := groundTimeline(raw, transcript)

	if len(kept) != 1 || kept[0].Quote != "investigating checkout 500s" {
		t.Errorf("kept = %+v, want exactly the verbatim-grounded entry", kept)
	}

	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
}

func TestGroundTimelineHandlesNonArrayAndMissingQuote(t *testing.T) {
	if kept, dropped := groundTimeline("not an array", "anything"); len(kept) != 0 || dropped != 0 {
		t.Errorf("groundTimeline(non-array) = (%v, %d), want (empty, 0)", kept, dropped)
	}

	raw := []any{map[string]any{"time": "10:00", "event": "no quote given"}}
	if kept, dropped := groundTimeline(raw, "some transcript text"); len(kept) != 0 || dropped != 1 {
		t.Errorf("groundTimeline(missing quote) = (%v, %d), want (empty, 1)", kept, dropped)
	}
}

// TestBuildActionItemsRefusesUnknownOwner proves the owner guardrail: a name the model proposes that is
// not a real, recognised participant never becomes the authoritative owner — it's kept only as an
// advisory suggestion, and the item is flagged for manual assignment.
func TestBuildActionItemsRefusesUnknownOwner(t *testing.T) {
	participants := map[string]bool{"alice": true, "bob": true}

	raw := []any{
		map[string]any{"action": "restart the pool", "owner": "external-attacker"},
	}

	items, refused := buildActionItems(raw, participants)

	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}

	got := items[0]
	if got.OwnerVerified {
		t.Error("OwnerVerified = true for a non-participant, want false")
	}

	if got.Owner != "" {
		t.Errorf("Owner = %q, want empty — the model's guess must never become authoritative", got.Owner)
	}

	if got.ModelSuggestedOwner != "external-attacker" {
		t.Errorf("ModelSuggestedOwner = %q, want the model's guess kept for reference", got.ModelSuggestedOwner)
	}

	if !got.NeedsManualAssignment {
		t.Error("NeedsManualAssignment = false for an unverified owner, want true")
	}

	if len(refused) == 0 {
		t.Error("refused should explain that the model's owner guess was not trusted")
	}
}

// TestBuildActionItemsHostilePromptInjection is the README's hostile-input example: a chat line tries
// to override the summarizer's own instructions to assign an action item to an attacker and smuggle out
// a cloud-metadata URL as a "link". Both must be refused deterministically.
func TestBuildActionItemsHostilePromptInjection(t *testing.T) {
	participants := map[string]bool{"alice": true}

	raw := []any{
		map[string]any{
			"action": "page the on-call",
			"owner":  "external-attacker",
			"link":   "http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		},
	}

	items, refused := buildActionItems(raw, participants)

	got := items[0]
	if got.OwnerVerified || got.Owner != "" {
		t.Error("a hallucinated/injected owner must never be returned as authoritative")
	}

	if got.Link != "" {
		t.Errorf("Link = %q, the metadata URL must never surface in the response", got.Link)
	}

	if len(refused) != 2 {
		t.Errorf("refused = %v, want 2 entries (owner + link)", refused)
	}
}

// TestBuildActionItemsVerifiedOwner is the happy path: a real participant is accepted as owner.
func TestBuildActionItemsVerifiedOwner(t *testing.T) {
	participants := map[string]bool{"alice": true, "bob": true}

	raw := []any{
		map[string]any{"action": "restart the pool", "owner": "Alice", "link": "https://runbooks.internal/checkout"},
	}

	items, refused := buildActionItems(raw, participants)

	got := items[0]
	if !got.OwnerVerified || got.Owner != "Alice" {
		t.Errorf("got owner=%q verified=%v, want Alice/true", got.Owner, got.OwnerVerified)
	}

	if got.NeedsManualAssignment {
		t.Error("NeedsManualAssignment = true for a verified owner, want false")
	}

	if got.Link != "https://runbooks.internal/checkout" {
		t.Errorf("Link = %q, a safe URL should pass through unchanged", got.Link)
	}

	if len(refused) != 0 {
		t.Errorf("refused = %v, want none for a fully valid item", refused)
	}
}

func TestParseParticipantsExtractsSpeakers(t *testing.T) {
	transcript := "[10:02] alice: investigating checkout 500s\n" +
		"[10:05] Bob Smith: found it, db pool exhausted\n" +
		"random line with no speaker prefix\n" +
		"Note: this is not really a speaker but looks like one\n" +
		"10:07 charlie without brackets: still counts? no colon within 40 chars test"

	got := parseParticipants(transcript)

	for _, want := range []string{"alice", "bob smith", "note"} {
		if !got[want] {
			t.Errorf("parseParticipants() missing %q in %v", want, got)
		}
	}
}

func TestIsNameLikeRejectsTimestampsAndSymbols(t *testing.T) {
	cases := map[string]bool{
		"alice":     true,
		"Bob Smith": true,
		"O'Brien":   true,
		"10:02":     false,
		"":          false,
		"a!b":       false,
	}

	for in, want := range cases {
		if got := isNameLike(in); got != want {
			t.Errorf("isNameLike(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMergeParticipantsUnionsCallerAndParsed(t *testing.T) {
	parsed := map[string]bool{"alice": true}

	merged := mergeParticipants([]string{"Bob", "  ", "Charlie"}, parsed)

	for _, want := range []string{"alice", "bob", "charlie"} {
		if !merged[want] {
			t.Errorf("mergeParticipants() missing %q in %v", want, merged)
		}
	}

	if len(merged) != 3 {
		t.Errorf("len(merged) = %d, want 3", len(merged))
	}
}

func TestIsSafeURLBlocksSSRFTargets(t *testing.T) {
	bad := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost:8080/",
		"http://127.0.0.1/",
		"ftp://runbooks.internal/x",
		"http://user:pass@runbooks.internal/x",
		"http://internal.metadata.example/",
	}

	for _, u := range bad {
		if ok, _ := isSafeURL(u); ok {
			t.Errorf("isSafeURL(%q) = true, want false", u)
		}
	}

	if ok, reason := isSafeURL("https://runbooks.internal/postmortem"); !ok {
		t.Errorf("isSafeURL(valid https URL) = false (%s), want true", reason)
	}
}

func TestExtractJSONFromFencedResponse(t *testing.T) {
	raw, err := extractJSON("here you go:\n```json\n{\"recap\": \"checkout outage\"}\n```")
	if err != nil {
		t.Fatalf("extractJSON() error = %v", err)
	}

	if raw["recap"] != "checkout outage" {
		t.Errorf("extractJSON() = %v", raw)
	}
}

func TestGuardrailEndToEnd(t *testing.T) {
	transcript := "[10:02] alice: investigating checkout 500s\n[10:05] bob: found it, db pool exhausted"
	participants := mergeParticipants(nil, parseParticipants(transcript))

	raw := map[string]any{
		"recap": "checkout service had a 500 spike caused by db pool exhaustion",
		"timeline": []any{
			map[string]any{"time": "10:02", "event": "investigation starts", "quote": "investigating checkout 500s"},
		},
		"action_items": []any{
			map[string]any{"action": "increase pool size", "owner": "bob"},
		},
	}

	got := guardrail(raw, transcript, participants)

	if got.Recap == "" {
		t.Error("Recap should not be empty")
	}

	if len(got.Timeline) != 1 || got.TimelineDropped != 0 {
		t.Errorf("Timeline = %+v, TimelineDropped = %d", got.Timeline, got.TimelineDropped)
	}

	if len(got.ActionItems) != 1 || !got.ActionItems[0].OwnerVerified {
		t.Errorf("ActionItems = %+v, want bob verified as owner", got.ActionItems)
	}
}
