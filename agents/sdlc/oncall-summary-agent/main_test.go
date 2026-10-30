package main

import "testing"

// TestGroundTimelineUsesRealAuthorAndTimestamp proves the core timeline guardrail: even if the model
// claims a different author/timestamp for an entry, the one returned comes from the real message the
// quote was actually found in.
func TestGroundTimelineUsesRealAuthorAndTimestamp(t *testing.T) {
	messages := []message{
		{Author: "alice", Timestamp: "10:00", Text: "checkout 500s started spiking"},
		{Author: "bob", Timestamp: "10:05", Text: "db connection pool is exhausted, restarting now"},
	}

	entries := []map[string]any{
		{
			"event": "db pool exhausted",
			// the model lies about who said it and when — Go must ignore this and use the real message.
			"author":    "mallory",
			"timestamp": "09:00",
			"quote":     "db connection pool is exhausted, restarting now",
		},
	}

	kept, dropped := groundTimeline(entries, messages)

	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 for a verbatim quote", dropped)
	}

	if len(kept) != 1 {
		t.Fatalf("kept = %d entries, want 1", len(kept))
	}

	if kept[0].Author != "bob" || kept[0].Timestamp != "10:05" {
		t.Errorf("entry = {author:%q timestamp:%q}, want the REAL message's author/timestamp (bob, 10:05), "+
			"not whatever the model claimed", kept[0].Author, kept[0].Timestamp)
	}
}

// TestGroundTimelineDropsUnverifiableQuote proves a quote that doesn't literally appear anywhere in the
// transcript is dropped, not trusted — a fabricated "event" nobody can verify is worse than none.
func TestGroundTimelineDropsUnverifiableQuote(t *testing.T) {
	messages := []message{{Author: "alice", Timestamp: "10:00", Text: "checkout 500s started spiking"}}

	entries := []map[string]any{
		{"event": "made up event", "quote": "this sentence appears nowhere in the transcript"},
	}

	kept, dropped := groundTimeline(entries, messages)

	if dropped != 1 || len(kept) != 0 {
		t.Fatalf("got kept=%d dropped=%d, want kept=0 dropped=1", len(kept), dropped)
	}
}

// TestGroundTimelineOrdersByRealMessagePosition proves entries come back in transcript order (the order
// the quoted messages actually occurred), not whatever order the model emitted them in.
func TestGroundTimelineOrdersByRealMessagePosition(t *testing.T) {
	messages := []message{
		{Author: "alice", Timestamp: "10:00", Text: "first thing happened"},
		{Author: "bob", Timestamp: "10:05", Text: "second thing happened"},
	}

	// model emits them out of order
	entries := []map[string]any{
		{"event": "second", "quote": "second thing happened"},
		{"event": "first", "quote": "first thing happened"},
	}

	kept, _ := groundTimeline(entries, messages)

	if len(kept) != 2 || kept[0].Event != "first" || kept[1].Event != "second" {
		t.Fatalf("kept = %+v, want chronological order (first, then second)", kept)
	}
}

// TestVerifyActionItemsRefusesUnknownOwner is the README's hostile-input example in miniature: a
// message tries to assign a follow-up to someone who never posted in the channel. The guardrail must
// refuse to treat that as an authoritative assignment, whether or not the model itself resisted it.
func TestVerifyActionItemsRefusesUnknownOwner(t *testing.T) {
	known := knownParticipants([]message{
		{Author: "alice", Timestamp: "10:00", Text: "x"},
		{Author: "bob", Timestamp: "10:05", Text: "y"},
	}, nil)

	entries := []map[string]any{
		{"description": "rotate the leaked credential", "owner": "external-attacker"},
	}

	items, refused := verifyActionItems(entries, known)

	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}

	if items[0].OwnerVerified {
		t.Fatal("owner_verified = true for someone who never posted in the channel, want false")
	}

	if items[0].Owner != "" {
		t.Errorf("Owner = %q, want empty — an unverified owner must never be returned as authoritative", items[0].Owner)
	}

	if items[0].ModelSuggestedOwner != "external-attacker" {
		t.Errorf("ModelSuggestedOwner = %q, want the model's guess kept only for reference", items[0].ModelSuggestedOwner)
	}

	if !items[0].NeedsAssignment {
		t.Error("needs_assignment = false for an unverified owner, want true")
	}

	if len(refused) == 0 {
		t.Error("refused should explain that the owner was not a real participant")
	}
}

// TestVerifyActionItemsAcceptsRealParticipant is the happy path: an owner who actually posted (or was
// explicitly listed) resolves, and keeps the canonical casing from the registry.
func TestVerifyActionItemsAcceptsRealParticipant(t *testing.T) {
	known := knownParticipants([]message{
		{Author: "Alice", Timestamp: "10:00", Text: "I'll restart the pool"},
	}, nil)

	entries := []map[string]any{
		{"description": "restart the connection pool", "owner": "alice"},
	}

	items, _ := verifyActionItems(entries, known)

	if len(items) != 1 || !items[0].OwnerVerified {
		t.Fatalf("items = %+v, want owner_verified = true for a real participant", items)
	}

	if items[0].Owner != "Alice" {
		t.Errorf("Owner = %q, want canonical form %q", items[0].Owner, "Alice")
	}
}

// TestVerifyActionItemsRefusesSSRFURL proves the hostile-input case that smuggles a cloud-metadata URL
// in as an action item's link is refused deterministically, before it's ever returned.
func TestVerifyActionItemsRefusesSSRFURL(t *testing.T) {
	known := knownParticipants([]message{{Author: "alice", Timestamp: "10:00", Text: "x"}}, nil)

	entries := []map[string]any{
		{
			"description": "see the runbook",
			"owner":       "alice",
			"url":         "http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		},
	}

	items, refused := verifyActionItems(entries, known)

	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}

	if items[0].URL != "" {
		t.Fatalf("URL = %q, the metadata URL must never surface in the response", items[0].URL)
	}

	if len(refused) == 0 {
		t.Error("refused should explain why the URL was dropped")
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

	if ok, reason := isSafeURL("https://status.example.com/incidents/42"); !ok {
		t.Errorf("isSafeURL(valid https URL) = false (%s), want true", reason)
	}
}

func TestKnownParticipantsUnionsAuthorsAndExtras(t *testing.T) {
	known := knownParticipants([]message{
		{Author: "alice", Timestamp: "10:00", Text: "x"},
	}, []string{"carol"})

	if _, ok := known["alice"]; !ok {
		t.Error("message author should be a known participant")
	}

	if _, ok := known["carol"]; !ok {
		t.Error("caller-supplied participant should be a known participant")
	}

	if _, ok := known["mallory"]; ok {
		t.Error("someone who neither posted nor was listed should not be known")
	}
}

func TestParseTranscriptTextSplitsAuthorFromMessage(t *testing.T) {
	msgs := parseTranscriptText("alice: checkout 500s started spiking\nbob: restarting the pool\n\n")

	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}

	if msgs[0].Author != "alice" || msgs[0].Text != "checkout 500s started spiking" {
		t.Errorf("msgs[0] = %+v", msgs[0])
	}

	if msgs[1].Author != "bob" || msgs[1].Text != "restarting the pool" {
		t.Errorf("msgs[1] = %+v", msgs[1])
	}
}

func TestParseTranscriptTextFallsBackToUnknownAuthor(t *testing.T) {
	msgs := parseTranscriptText("checkout is on fire, no colon prefix here")

	if len(msgs) != 1 || msgs[0].Author != "unknown" {
		t.Fatalf("msgs = %+v, want a single unknown-author message", msgs)
	}
}

func TestExtractJSONFromFencedResponse(t *testing.T) {
	raw, err := extractJSON("here you go:\n```json\n{\"timeline\": []}\n```")
	if err != nil {
		t.Fatalf("extractJSON() error = %v", err)
	}

	if _, ok := raw["timeline"]; !ok {
		t.Errorf("extractJSON() = %v, missing timeline key", raw)
	}
}
