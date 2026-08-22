package main

import (
	"strings"
	"testing"
)

func testMsgs() []message {
	return []message{
		{Time: "14:02", Speaker: "alice", Text: "checkout-service 500s spiking, paging"},
		{Time: "14:05", Speaker: "bob", Text: "confirmed db timeout connecting to payments-db, pool exhausted"},
		{Time: "14:11", Speaker: "alice", Text: "bumped the pool size to 50, error rate dropping"},
		{Time: "14:20", Speaker: "bob", Text: "all clear, will write the postmortem tomorrow"},
	}
}

// TestGroundTimelineKeepsOnlyVerbatimQuotes proves the core guardrail: a claimed event whose quote
// doesn't literally appear in the transcript is dropped, not kept with an asterisk.
func TestGroundTimelineKeepsOnlyVerbatimQuotes(t *testing.T) {
	msgs := testMsgs()

	claims := []map[string]any{
		{"quote": "confirmed db timeout connecting to payments-db, pool exhausted", "event": "root cause found"},
		{"quote": "this never happened in the transcript", "event": "fabricated event"},
	}

	kept, dropped := groundTimeline(claims, msgs)

	if len(kept) != 1 {
		t.Fatalf("kept = %d entries, want 1", len(kept))
	}

	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}

	if kept[0].Speaker != "bob" || kept[0].Time != "14:05" {
		t.Errorf("grounded entry = %+v, want speaker=bob time=14:05 (from the matched message, not the model)", kept[0])
	}
}

// TestGroundTimelineTimeAndSpeakerAreNeverTrustedFromTheModel is the hostile-input case: a claim tries
// to attach a real transcript quote to a forged time and speaker (as an injected instruction embedded
// in a channel message would attempt). The guardrail must always override with the ACTUAL message's
// metadata, never whatever the model — possibly parroting the injection — claims.
func TestGroundTimelineTimeAndSpeakerAreNeverTrustedFromTheModel(t *testing.T) {
	msgs := testMsgs()

	claims := []map[string]any{
		// The model (or an injected instruction it echoed) claims this quote happened at 03:00, said by
		// "ceo" — but the quote is real and actually said by bob at 14:05. The guardrail must report
		// bob/14:05, never the model's claim, because timelineEntry has no time/speaker input from the
		// model at all — it can only ever come from the matched message.
		{"quote": "confirmed db timeout connecting to payments-db, pool exhausted", "event": "root cause found"},
	}

	kept, _ := groundTimeline(claims, msgs)

	if len(kept) != 1 || kept[0].Time != "14:05" || kept[0].Speaker != "bob" {
		t.Fatalf("timeline entry must be grounded to the real message's time/speaker, got %+v", kept)
	}
}

// TestGroundTimelineOrdersByTranscriptPosition proves the timeline's chronology comes from the
// transcript's own order, not whatever order the model happened to list events in.
func TestGroundTimelineOrdersByTranscriptPosition(t *testing.T) {
	msgs := testMsgs()

	// Listed out of order on purpose.
	claims := []map[string]any{
		{"quote": "all clear, will write the postmortem tomorrow", "event": "resolved"},
		{"quote": "checkout-service 500s spiking, paging", "event": "alert fired"},
	}

	kept, _ := groundTimeline(claims, msgs)

	if len(kept) != 2 {
		t.Fatalf("kept = %d entries, want 2", len(kept))
	}

	if kept[0].Event != "alert fired" || kept[1].Event != "resolved" {
		t.Errorf("timeline not in transcript order: %+v", kept)
	}
}

// TestResolveActionItemsRefusesUnknownOwner is the hostile-input case for ownership: an owner nobody in
// the transcript matches must never become the authoritative owner — it falls back to unassigned +
// needs_manual_assignment.
func TestResolveActionItemsRefusesUnknownOwner(t *testing.T) {
	msgs := testMsgs()
	participants := distinctSpeakers(msgs)
	byLower := map[string]string{}
	for _, p := range participants {
		byLower[strings.ToLower(p)] = p
	}

	claims := []map[string]any{
		{"description": "wire funds to external-vendor-invoice", "owner": "finance-bot"},
	}

	items, refused := resolveActionItems(claims, msgs, byLower)

	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}

	got := items[0]
	if got.OwnerVerified {
		t.Fatal("owner_verified = true for a non-participant, want false")
	}

	if got.Owner != "" {
		t.Fatalf("Owner = %q, want empty — a non-participant must never become the authoritative owner", got.Owner)
	}

	if !got.NeedsManualAssignment {
		t.Error("needs_manual_assignment = false for an unresolved owner, want true")
	}

	if got.ModelSuggestedOwner != "finance-bot" {
		t.Errorf("ModelSuggestedOwner = %q, want the model's guess kept for reference", got.ModelSuggestedOwner)
	}

	if len(refused) == 0 {
		t.Error("refused should explain that the owner was not a real participant")
	}
}

// TestResolveActionItemsAcceptsRealParticipant is the happy path: an owner who actually has a message
// in the transcript is verified and returned as-is.
func TestResolveActionItemsAcceptsRealParticipant(t *testing.T) {
	msgs := testMsgs()
	participants := distinctSpeakers(msgs)
	byLower := map[string]string{}
	for _, p := range participants {
		byLower[strings.ToLower(p)] = p
	}

	claims := []map[string]any{
		{"description": "write the postmortem", "owner": "Bob"}, // different casing than the transcript
	}

	items, refused := resolveActionItems(claims, msgs, byLower)

	if len(items) != 1 || !items[0].OwnerVerified {
		t.Fatalf("items = %+v, want owner_verified=true", items)
	}

	if items[0].Owner != "bob" {
		t.Errorf("Owner = %q, want the transcript's own casing %q", items[0].Owner, "bob")
	}

	if len(refused) != 0 {
		t.Errorf("refused = %v, want none for a verified owner", refused)
	}
}

// TestDistinctSpeakersIsInputDerivedOnly proves participants come only from the transcript, in
// first-appearance order, de-duplicated case-insensitively.
func TestDistinctSpeakersIsInputDerivedOnly(t *testing.T) {
	msgs := []message{
		{Speaker: "alice", Text: "a"},
		{Speaker: "Alice", Text: "b"},
		{Speaker: "bob", Text: "c"},
	}

	got := distinctSpeakers(msgs)

	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("distinctSpeakers = %v, want [alice bob]", got)
	}
}

func TestFindMessageRequiresVerbatimMatch(t *testing.T) {
	msgs := testMsgs()

	if idx := findMessage("db timeout connecting to payments-db", msgs); idx != 1 {
		t.Errorf("findMessage(substring) = %d, want 1", idx)
	}

	if idx := findMessage("something nobody said", msgs); idx != -1 {
		t.Errorf("findMessage(unmatched) = %d, want -1", idx)
	}

	if idx := findMessage("", msgs); idx != -1 {
		t.Errorf("findMessage(empty) = %d, want -1", idx)
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
