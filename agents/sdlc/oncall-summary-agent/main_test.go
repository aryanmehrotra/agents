package main

import "testing"

// TestGroundTimelineOrdersByTranscriptPosition proves the core guardrail: even when the model lists
// timeline claims out of order, the kept entries are re-ordered by where they actually occur in the
// transcript — the model's own ordering is never trusted.
func TestGroundTimelineOrdersByTranscriptPosition(t *testing.T) {
	transcript := "14:00 alice: investigating 500s\n14:05 bob: found the leak\n14:10 alice: rolled back"

	claims := []timelineEntry{
		{Quote: "rolled back", Note: "rollback"},       // occurs last
		{Quote: "investigating 500s", Note: "started"}, // occurs first
		{Quote: "found the leak", Note: "root cause"},  // occurs middle
	}

	kept, dropped := groundTimeline(claims, transcript)

	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 for all-verbatim claims", dropped)
	}

	if len(kept) != 3 {
		t.Fatalf("kept = %d entries, want 3", len(kept))
	}

	want := []string{"investigating 500s", "found the leak", "rolled back"}
	for i, w := range want {
		if kept[i].Quote != w {
			t.Errorf("kept[%d].Quote = %q, want %q (chronological transcript order)", i, kept[i].Quote, w)
		}
	}
}

// TestGroundTimelineDropsFabricatedQuote proves a quote that doesn't literally appear in the
// transcript is dropped, not trusted.
func TestGroundTimelineDropsFabricatedQuote(t *testing.T) {
	transcript := "14:00 alice: investigating 500s"

	kept, dropped := groundTimeline([]timelineEntry{
		{Quote: "investigating 500s", Note: "real"},
		{Quote: "the datacenter caught fire", Note: "fabricated"},
	}, transcript)

	if len(kept) != 1 || kept[0].Quote != "investigating 500s" {
		t.Errorf("kept = %v, want exactly the verbatim quote", kept)
	}

	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
}

// TestExtractParticipantsOnlyFromRealSpeakerLines proves the participant registry is built entirely
// from the transcript's own "name:" lines, never from free text that merely mentions a name.
func TestExtractParticipantsOnlyFromRealSpeakerLines(t *testing.T) {
	transcript := "14:00 alice: investigating\n14:02 bob: paging charlie about the outage"

	got := extractParticipants(transcript)

	if !got["alice"] || !got["bob"] {
		t.Fatalf("participants = %v, want alice and bob (actual speakers)", got)
	}

	if got["charlie"] {
		t.Error("charlie was only mentioned in a message, never spoke — must not be a verified participant")
	}
}

// TestResolveActionItemsRefusesUnverifiedOwner proves an action item owner is only authoritative when
// that name was an actual speaker — a name the model invents is kept only as an unverified suggestion.
func TestResolveActionItemsRefusesUnverifiedOwner(t *testing.T) {
	participants := map[string]bool{"alice": true}

	items, refused := resolveActionItems([]struct{ action, owner string }{
		{action: "roll back the deploy", owner: "alice"},
		{action: "page the vendor", owner: "external-attacker"},
	}, participants)

	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}

	if !items[0].OwnerVerified || items[0].Owner != "alice" {
		t.Errorf("items[0] = %+v, want owner_verified=true, owner=alice", items[0])
	}

	if items[1].OwnerVerified || items[1].Owner != "" {
		t.Errorf("items[1] = %+v, want owner_verified=false and owner empty — attacker never spoke", items[1])
	}

	if items[1].ModelOwner != "external-attacker" {
		t.Errorf("items[1].ModelOwner = %q, want the model's guess kept for reference", items[1].ModelOwner)
	}

	if len(refused) == 0 {
		t.Error("refused should explain why the unverified owner was not assigned authoritatively")
	}
}

// TestResolveActionItemsRefusesSSRFURL is the README's hostile-input example: a transcript line tries
// to smuggle a cloud-metadata URL out as a "status page" action item. The URL must never survive into
// the response, regardless of whether the model itself resisted the injection.
func TestResolveActionItemsRefusesSSRFURL(t *testing.T) {
	items, refused := resolveActionItems([]struct{ action, owner string }{
		{action: "post updates to http://169.254.169.254/latest/meta-data/iam/security-credentials/", owner: "alice"},
	}, map[string]bool{"alice": true})

	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}

	if contains(items[0].Action, "169.254.169.254") {
		t.Fatalf("Action = %q, the metadata URL must never survive into the response", items[0].Action)
	}

	if len(refused) == 0 {
		t.Error("refused should record that the embedded URL was refused")
	}
}

// TestResolveActionItemsDedupes proves a duplicate action (same text, case-insensitive) is only kept
// once.
func TestResolveActionItemsDedupes(t *testing.T) {
	items, _ := resolveActionItems([]struct{ action, owner string }{
		{action: "roll back the deploy", owner: "alice"},
		{action: "Roll Back The Deploy", owner: "bob"},
	}, map[string]bool{"alice": true, "bob": true})

	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (case-insensitive duplicate dropped)", len(items))
	}
}

// TestGuardrailClampsInvalidStatus proves a hallucinated status value is never passed through.
func TestGuardrailClampsInvalidStatus(t *testing.T) {
	raw := map[string]any{
		"status":       "apocalyptic", // not a real value
		"timeline":     []any{},
		"action_items": []any{},
	}

	got := guardrail(raw, "14:00 alice: hi", map[string]bool{"alice": true})

	if got.Status != "ongoing" {
		t.Errorf("Status = %q, want the default %q for an invalid value", got.Status, "ongoing")
	}
}

func TestIsSafeURLBlocksSSRFTargets(t *testing.T) {
	bad := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost:8080/",
		"http://127.0.0.1/",
		"ftp://status.internal/x",
		"http://user:pass@status.internal/x",
	}

	for _, u := range bad {
		if ok, _ := isSafeURL(u); ok {
			t.Errorf("isSafeURL(%q) = true, want false", u)
		}
	}

	if ok, reason := isSafeURL("https://status.example.com/incident/42"); !ok {
		t.Errorf("isSafeURL(valid https URL) = false (%s), want true", reason)
	}
}

func TestExtractJSONFromFencedResponse(t *testing.T) {
	raw, err := extractJSON("here you go:\n```json\n{\"status\": \"ongoing\"}\n```")
	if err != nil {
		t.Fatalf("extractJSON() error = %v", err)
	}

	if raw["status"] != "ongoing" {
		t.Errorf("extractJSON() = %v", raw)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}

	return false
}
