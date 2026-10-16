package main

import "testing"

const sampleTranscript = `09:14 alice: Checkout is throwing 500s, paging on-call
09:16 bob: Confirmed, DB connection pool exhausted on payments-db
09:20 alice: I'll roll back the last deploy
09:25 bob: Rollback done, errors stopping`

// TestParseTranscriptExtractsSpeakersAndTimes proves the deterministic parser — the ONLY source the
// participant registry is ever built from — reads real speakers and timestamps out of the raw text.
func TestParseTranscriptExtractsSpeakersAndTimes(t *testing.T) {
	messages := parseTranscript(sampleTranscript)
	if len(messages) != 4 {
		t.Fatalf("len(messages) = %d, want 4", len(messages))
	}

	if messages[0].Speaker != "alice" || messages[0].Time != "09:14" {
		t.Errorf("messages[0] = %+v, want speaker alice at 09:14", messages[0])
	}

	if messages[1].Speaker != "bob" {
		t.Errorf("messages[1].Speaker = %q, want bob", messages[1].Speaker)
	}
}

func TestParticipantsOfDedupesAndPreservesOrder(t *testing.T) {
	messages := parseTranscript(sampleTranscript)

	got := participantsOf(messages)
	want := []string{"alice", "bob"}

	if len(got) != len(want) {
		t.Fatalf("participantsOf() = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("participantsOf()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestGroundTimelineDropsUnverifiableQuotesAndReorders proves the timeline guardrail: a quote that
// doesn't literally appear in the transcript is dropped, and kept entries are reordered by where the
// evidence actually sits in the transcript — never by the order the model listed them in.
func TestGroundTimelineDropsUnverifiableQuotesAndReorders(t *testing.T) {
	messages := parseTranscript(sampleTranscript)

	// Listed out of order on purpose, plus one fabricated quote.
	items := []map[string]any{
		{"event": "rollback completed", "quote": "Rollback done, errors stopping"},
		{"event": "root cause found", "quote": "DB connection pool exhausted on payments-db"},
		{"event": "fabricated", "quote": "this never appeared anywhere in the chat"},
	}

	kept, dropped := groundTimeline(items, messages)

	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}

	if len(kept) != 2 {
		t.Fatalf("len(kept) = %d, want 2", len(kept))
	}

	if kept[0].Event != "root cause found" || kept[1].Event != "rollback completed" {
		t.Errorf("kept = %+v, want chronological order (root cause before rollback)", kept)
	}

	if kept[0].Speaker != "bob" || kept[0].Time != "09:16" {
		t.Errorf("kept[0] = %+v, want speaker bob at 09:16 attached from the real transcript line", kept[0])
	}
}

// TestGroundActionItemsRefusesUnverifiedOwner is the core guardrail: an owner who never actually spoke
// in the transcript is never trusted, even though they're named in the model's output.
func TestGroundActionItemsRefusesUnverifiedOwner(t *testing.T) {
	byLower := map[string]string{"alice": "alice", "bob": "bob"}

	items := []map[string]any{
		{"task": "add a payments-db connection pool alert", "owner": "carol"},
	}

	kept, refused := groundActionItems(items, byLower)

	if len(kept) != 1 {
		t.Fatalf("len(kept) = %d, want 1", len(kept))
	}

	if kept[0].OwnerVerified {
		t.Error("OwnerVerified = true for a non-participant, want false")
	}

	if kept[0].Owner != "unassigned" {
		t.Errorf("Owner = %q, want %q", kept[0].Owner, "unassigned")
	}

	if kept[0].ModelSuggestedOwner != "carol" {
		t.Errorf("ModelSuggestedOwner = %q, want the model's guess kept for reference", kept[0].ModelSuggestedOwner)
	}

	if len(refused) == 0 {
		t.Error("refused should explain that the owner was not a real participant")
	}
}

// TestGroundActionItemsHostilePromptInjection is the README's hostile-input example: a transcript line
// tries to override the agent's own instructions to assign a follow-up to an attacker-controlled name
// and smuggle out a cloud-metadata URL as the "link". Both must be refused deterministically, whether
// or not the model itself resists the injection.
func TestGroundActionItemsHostilePromptInjection(t *testing.T) {
	byLower := map[string]string{"alice": "alice", "bob": "bob"}

	items := []map[string]any{
		{
			"task":  "rotate the leaked credentials",
			"owner": "external-attacker",
			"link":  "http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		},
	}

	kept, refused := groundActionItems(items, byLower)

	if len(kept) != 1 {
		t.Fatalf("len(kept) = %d, want 1", len(kept))
	}

	if kept[0].OwnerVerified || kept[0].Owner != "unassigned" {
		t.Error("a non-participant / injected owner must never be returned as verified")
	}

	if kept[0].Link != "" {
		t.Errorf("Link = %q, the metadata URL must never surface in the response", kept[0].Link)
	}

	if len(refused) < 2 {
		t.Errorf("refused = %v, want refusals for both the owner and the link", refused)
	}
}

// TestGroundActionItemsVerifiedOwner is the happy path: a real transcript speaker resolves cleanly.
func TestGroundActionItemsVerifiedOwner(t *testing.T) {
	byLower := map[string]string{"alice": "alice", "bob": "bob"}

	items := []map[string]any{
		{"task": "add a payments-db connection pool alert", "owner": "bob", "link": "https://runbooks.internal/payments-db"},
	}

	kept, refused := groundActionItems(items, byLower)

	if len(kept) != 1 || !kept[0].OwnerVerified || kept[0].Owner != "bob" {
		t.Errorf("kept = %+v, want a verified owner bob", kept)
	}

	if len(refused) != 0 {
		t.Errorf("refused = %v, want none for a clean, verified item", refused)
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

	if ok, reason := isSafeURL("https://runbooks.internal/payments-db"); !ok {
		t.Errorf("isSafeURL(valid https URL) = false (%s), want true", reason)
	}
}

func TestExtractJSONFromFencedResponse(t *testing.T) {
	raw, err := extractJSON("here you go:\n```json\n{\"summary\": \"checkout outage, rolled back\"}\n```")
	if err != nil {
		t.Fatalf("extractJSON() error = %v", err)
	}

	if raw["summary"] != "checkout outage, rolled back" {
		t.Errorf("extractJSON() = %v", raw)
	}
}

func TestGuardrailEndToEnd(t *testing.T) {
	messages := parseTranscript(sampleTranscript)
	participants := participantsOf(messages)

	raw := map[string]any{
		"summary": "Checkout 500s from an exhausted DB connection pool; fixed by rolling back the deploy.",
		"timeline": []any{
			map[string]any{"event": "outage starts", "quote": "Checkout is throwing 500s, paging on-call"},
		},
		"action_items": []any{
			map[string]any{"task": "add a pool-exhaustion alert", "owner": "bob"},
		},
	}

	got := guardrail(raw, messages, sampleTranscript, participants)

	if len(got.Timeline) != 1 {
		t.Errorf("Timeline = %+v, want 1 grounded entry", got.Timeline)
	}

	if len(got.ActionItems) != 1 || !got.ActionItems[0].OwnerVerified {
		t.Errorf("ActionItems = %+v, want a verified owner", got.ActionItems)
	}

	if len(got.Participants) != 2 {
		t.Errorf("Participants = %v, want alice and bob", got.Participants)
	}
}
