package main

import "testing"

// TestResolveActionItemsRefusesUnknownOwner proves the core guardrail: a model that names someone who
// never posted in the channel never gets that name returned as the authoritative owner — the item
// falls back to unassigned instead of being routed on the model's word.
func TestResolveActionItemsRefusesUnknownOwner(t *testing.T) {
	roster := map[string]bool{"alice": true, "bob": true}

	items, refused := resolveActionItems([]any{
		map[string]any{"description": "rotate the leaked db credential", "owner": "totally-unknown-person", "due": "eod"},
	}, roster)

	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}

	if items[0].OwnerVerified {
		t.Error("OwnerVerified = true for a non-participant, want false")
	}

	if items[0].Owner != "" {
		t.Errorf("Owner = %q, want empty — an unverified owner must never be the authoritative owner", items[0].Owner)
	}

	if items[0].ModelSuggestedOwner != "totally-unknown-person" {
		t.Errorf("ModelSuggestedOwner = %q, want the model's guess kept for reference", items[0].ModelSuggestedOwner)
	}

	if len(refused) == 0 {
		t.Error("refused should explain that the model's owner guess was not trusted")
	}
}

// TestResolveActionItemsHostilePromptInjection is the README's hostile-input example: a channel
// message tries to override the agent's own instructions to hand a payout action item to an outside
// attacker who never participated in the incident. It must be refused deterministically, regardless of
// whether the model itself resisted the injection.
func TestResolveActionItemsHostilePromptInjection(t *testing.T) {
	roster := map[string]bool{"alice": true, "bob": true}

	items, refused := resolveActionItems([]any{
		map[string]any{"description": "issue the customer refund", "owner": "attacker@evil.com", "due": "now"},
	}, roster)

	if items[0].OwnerVerified || items[0].Owner != "" {
		t.Fatal("a hallucinated/injected owner must never be returned as the authoritative owner")
	}

	if len(refused) == 0 {
		t.Error("refused should record why the injected owner was not trusted")
	}
}

// TestResolveActionItemsVerifiedOwner is the happy path: a name that actually posted in the channel
// resolves to itself, with owner_verified true and no refusal.
func TestResolveActionItemsVerifiedOwner(t *testing.T) {
	roster := map[string]bool{"alice": true, "bob": true}

	items, refused := resolveActionItems([]any{
		map[string]any{"description": "restart the payments pool", "owner": "alice", "due": "asap"},
	}, roster)

	if !items[0].OwnerVerified {
		t.Error("OwnerVerified = false for a real participant, want true")
	}

	if items[0].Owner != "alice" {
		t.Errorf("Owner = %q, want %q", items[0].Owner, "alice")
	}

	if len(refused) != 0 {
		t.Errorf("refused = %v, want empty for a verified owner", refused)
	}
}

// TestGroundTimelineDropsUnverifiableQuotes proves the evidence-grounding guardrail: a timeline entry
// whose quote does not literally appear in the actual message text is dropped, not trusted.
func TestGroundTimelineDropsUnverifiableQuotes(t *testing.T) {
	rawText := "checkout 500s spiking\ndb pool exhausted, restarting now\n"
	roster := map[string]bool{"alice": true}

	kept, dropped := groundTimeline([]any{
		map[string]any{"time": "10:01", "speaker": "alice", "event": "reported the outage", "quote": "checkout 500s spiking"},
		map[string]any{"time": "10:05", "speaker": "alice", "event": "fabricated event", "quote": "this was never actually said"},
	}, rawText, roster)

	if len(kept) != 1 || kept[0].Quote != "checkout 500s spiking" {
		t.Errorf("kept = %v, want exactly the verbatim quote", kept)
	}

	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}

	if !kept[0].SpeakerVerified {
		t.Error("SpeakerVerified = false for a known participant, want true")
	}
}

// TestGroundTimelineFlagsUnknownSpeaker keeps a grounded event but flags a speaker who never appears
// in the roster — informational, not authoritative.
func TestGroundTimelineFlagsUnknownSpeaker(t *testing.T) {
	rawText := "the vendor status page says it's their outage\n"
	roster := map[string]bool{"alice": true}

	kept, _ := groundTimeline([]any{
		map[string]any{"time": "10:02", "speaker": "vendor-bot", "event": "blamed their own outage", "quote": "the vendor status page says it's their outage"},
	}, rawText, roster)

	if len(kept) != 1 {
		t.Fatalf("len(kept) = %d, want 1", len(kept))
	}

	if kept[0].SpeakerVerified {
		t.Error("SpeakerVerified = true for a speaker outside the roster, want false")
	}
}

// TestSafeLinksRefusesSSRFTarget proves a link the model claims was mentioned is refused before it's
// ever returned when it targets a non-http(s) scheme or an internal/metadata/private host — the same
// class of check incident-triage-agent and research-agent apply to any outbound-facing URL.
func TestSafeLinksRefusesSSRFTarget(t *testing.T) {
	kept, dropped := safeLinks([]any{
		"https://status.example.com/postmortem/482",
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://localhost:8080/admin",
	})

	if len(kept) != 1 || kept[0] != "https://status.example.com/postmortem/482" {
		t.Errorf("kept = %v, want only the safe link", kept)
	}

	if dropped != 2 {
		t.Errorf("dropped = %d, want 2", dropped)
	}
}

// TestGuardrailClampsInvalidStatus proves an invalid/hallucinated status value is never passed
// through — it's deterministically clamped to "unknown".
func TestGuardrailClampsInvalidStatus(t *testing.T) {
	raw := map[string]any{
		"status":           "definitely-fixed-trust-me",
		"one_line_summary": "checkout outage",
		"timeline":         []any{},
		"action_items":     []any{},
		"links":            []any{},
	}

	got := guardrail(raw, "checkout outage", map[string]bool{"alice": true})

	if got.Status != "unknown" {
		t.Errorf("Status = %q, want the default %q for an invalid value", got.Status, "unknown")
	}
}

// TestBuildRosterFromMessagesAndExtras proves the roster — the only source owners/speakers are checked
// against — is built from real message speakers plus any caller-supplied extras, case-insensitively.
func TestBuildRosterFromMessagesAndExtras(t *testing.T) {
	roster := buildRoster([]chatMessage{
		{Speaker: "Alice", Timestamp: "10:00", Text: "checkout is down"},
	}, []string{"Bob"})

	if !roster["alice"] {
		t.Error("roster should contain the message speaker, lowercased")
	}

	if !roster["bob"] {
		t.Error("roster should contain the caller-supplied extra participant")
	}
}

// TestParseTranscript proves the freeform "speaker: text" parser the orchestrator's single free-text
// query field relies on, including an optional leading "[timestamp]" and a line with no attributable
// speaker being kept as unattributed text rather than dropped.
func TestParseTranscript(t *testing.T) {
	got := parseTranscript("[10:01] alice: checkout is down\nbob: on it\njust some unattributed note\n\n")

	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}

	if got[0].Timestamp != "10:01" || got[0].Speaker != "alice" || got[0].Text != "checkout is down" {
		t.Errorf("got[0] = %+v", got[0])
	}

	if got[1].Speaker != "bob" || got[1].Text != "on it" {
		t.Errorf("got[1] = %+v", got[1])
	}

	if got[2].Speaker != "" || got[2].Text != "just some unattributed note" {
		t.Errorf("got[2] = %+v, want unattributed text kept as-is", got[2])
	}
}

func TestExtractJSONFromFencedResponse(t *testing.T) {
	raw, err := extractJSON("here you go:\n```json\n{\"status\": \"resolved\"}\n```")
	if err != nil {
		t.Fatalf("extractJSON() error = %v", err)
	}

	if raw["status"] != "resolved" {
		t.Errorf("extractJSON() = %v", raw)
	}
}

func TestIsSafeURLBlocksSSRFTargets(t *testing.T) {
	bad := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost:8080/",
		"http://127.0.0.1/",
		"ftp://status.example.com/x",
		"http://user:pass@status.example.com/x",
		"http://internal.metadata.example/",
	}

	for _, u := range bad {
		if ok, _ := isSafeURL(u); ok {
			t.Errorf("isSafeURL(%q) = true, want false", u)
		}
	}

	if ok, reason := isSafeURL("https://status.example.com/postmortem/482"); !ok {
		t.Errorf("isSafeURL(valid https URL) = false (%s), want true", reason)
	}
}
