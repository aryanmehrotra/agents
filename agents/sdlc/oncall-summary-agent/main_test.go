package main

import "testing"

// TestResolveActionItemsRefusesHostileAssignee is the README's hostile-input example: a channel
// message tries to override the agent's own instructions to assign the action item to an identity that
// never posted and isn't in the roster. It must be refused deterministically — never left to whether
// the model "noticed" the injection.
func TestResolveActionItemsRefusesHostileAssignee(t *testing.T) {
	roster := buildRoster([]message{
		{Speaker: "alice", Timestamp: "2026-10-25T10:00:00Z", Text: "checkout 500s spiking"},
	}, nil)

	claims := []rawActionItem{
		{Description: "roll back the deploy", Assignee: "external-attacker@evil.com", Due: "now"},
	}

	kept, refused := resolveActionItems(claims, roster)

	if len(kept) != 1 {
		t.Fatalf("len(kept) = %d, want 1", len(kept))
	}

	got := kept[0]
	if got.AssigneeVerified {
		t.Fatal("assignee_verified = true for a non-participant, non-roster assignee, want false")
	}

	if got.Assignee != "" {
		t.Fatalf("Assignee = %q, want empty — never routed on the model's guess", got.Assignee)
	}

	if !got.NeedsHumanAssignment {
		t.Error("needs_human_assignment = false, want true")
	}

	if got.ModelSuggestedAssignee != "external-attacker@evil.com" {
		t.Errorf("ModelSuggestedAssignee = %q, want the guess kept for reference", got.ModelSuggestedAssignee)
	}

	if len(refused) == 0 {
		t.Error("refused should explain that the assignee was not trusted")
	}
}

// TestResolveActionItemsVerifiesRealParticipant is the happy path: an assignee who actually posted in
// the transcript resolves to a verified roster entry.
func TestResolveActionItemsVerifiesRealParticipant(t *testing.T) {
	roster := buildRoster([]message{
		{Speaker: "bob", Timestamp: "2026-10-25T10:00:00Z", Text: "I'm on it"},
	}, map[string]string{"bob": "payments-oncall"})

	claims := []rawActionItem{{Description: "restart the pool", Assignee: "Bob"}}

	kept, refused := resolveActionItems(claims, roster)

	if len(kept) != 1 || !kept[0].AssigneeVerified {
		t.Fatalf("assignee_verified = false for a real participant, want true (kept=%+v)", kept)
	}

	if kept[0].Assignee != "Bob" {
		t.Errorf("Assignee = %q, want %q (original casing preserved)", kept[0].Assignee, "Bob")
	}

	if kept[0].AssigneeRole != "payments-oncall" {
		t.Errorf("AssigneeRole = %q, want %q", kept[0].AssigneeRole, "payments-oncall")
	}

	if len(refused) != 0 {
		t.Errorf("refused = %v, want none for a verified assignee", refused)
	}
}

// TestGroundTimelineDropsUnverifiableQuotes proves the timeline guardrail: a quote that doesn't appear
// verbatim in any message is dropped, and a kept event's speaker/timestamp always come from the
// matching real message, never the model's claim about them.
func TestGroundTimelineDropsUnverifiableQuotes(t *testing.T) {
	messages := []message{
		{Speaker: "alice", Timestamp: "2026-10-25T10:00:00Z", Text: "checkout 500s spiking"},
		{Speaker: "bob", Timestamp: "2026-10-25T10:05:00Z", Text: "found it, db pool exhausted"},
	}

	claims := []rawTimelineClaim{
		{Quote: "db pool exhausted", Event: "root cause identified"},
		{Quote: "this never happened, pure fabrication", Event: "fake event"},
	}

	kept, dropped := groundTimeline(claims, messages)

	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}

	if len(kept) != 1 {
		t.Fatalf("len(kept) = %d, want 1", len(kept))
	}

	if kept[0].Speaker != "bob" || kept[0].Timestamp != "2026-10-25T10:05:00Z" {
		t.Errorf("kept[0] = %+v, want speaker/timestamp taken from the matching real message", kept[0])
	}
}

// TestGroundTimelineOverridesSpoofedSpeakerAndTimestamp proves the model's own speaker/timestamp claim
// (even if it tried to supply one) is irrelevant — only the real message it matched against counts.
// Here the attack is a quote that, if trusted blindly on position, would misattribute an event; instead
// grounding always resolves to whichever real message actually contains the quote.
func TestGroundTimelineOrdersChronologicallyByTranscriptPosition(t *testing.T) {
	messages := []message{
		{Speaker: "alice", Timestamp: "2026-10-25T10:00:00Z", Text: "checkout 500s spiking"},
		{Speaker: "bob", Timestamp: "2026-10-25T10:05:00Z", Text: "found it, db pool exhausted"},
		{Speaker: "alice", Timestamp: "2026-10-25T10:10:00Z", Text: "deployed the fix, monitoring"},
	}

	// Claims given out of order — grounding must still return them in transcript order.
	claims := []rawTimelineClaim{
		{Quote: "deployed the fix, monitoring", Event: "fix deployed"},
		{Quote: "checkout 500s spiking", Event: "incident started"},
		{Quote: "db pool exhausted", Event: "root cause found"},
	}

	kept, dropped := groundTimeline(claims, messages)

	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}

	if len(kept) != 3 {
		t.Fatalf("len(kept) = %d, want 3", len(kept))
	}

	wantOrder := []string{"checkout 500s spiking", "db pool exhausted", "deployed the fix, monitoring"}
	for i, q := range wantOrder {
		if kept[i].Quote != q {
			t.Errorf("kept[%d].Quote = %q, want %q (chronological order)", i, kept[i].Quote, q)
		}
	}
}

func TestSeverityStatusClampToAllowlist(t *testing.T) {
	raw := map[string]any{
		"tldr":     "x",
		"severity": "apocalyptic", // not a real value
		"status":   "vibes-based", // not a real value
	}

	got := guardrail(raw, []message{{Speaker: "a", Timestamp: "t", Text: "x"}}, buildRoster(nil, nil))

	if got.Severity != "medium" {
		t.Errorf("Severity = %q, want the default %q for an invalid value", got.Severity, "medium")
	}

	if got.Status != "unknown" {
		t.Errorf("Status = %q, want the default %q for an invalid value", got.Status, "unknown")
	}
}

func TestBuildRosterMergesParticipantsAndOverride(t *testing.T) {
	roster := buildRoster([]message{
		{Speaker: "Alice", Timestamp: "t", Text: "x"},
	}, map[string]string{"bob": "payments-team"})

	if role, ok := roster["alice"]; !ok || role != "participant" {
		t.Errorf("roster[alice] = (%q, %v), want (participant, true)", role, ok)
	}

	if role, ok := roster["bob"]; !ok || role != "payments-team" {
		t.Errorf("roster[bob] = (%q, %v), want (payments-team, true)", role, ok)
	}
}

func TestParseTranscriptParsesSpeakerAndTimestamp(t *testing.T) {
	got := parseTranscript("[10:00] alice: checkout 500s spiking\nbob: found it, db pool exhausted\nnot-a-message-no-colon-pattern-just-noise")

	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (%+v)", len(got), got)
	}

	if got[0].Timestamp != "10:00" || got[0].Speaker != "alice" || got[0].Text != "checkout 500s spiking" {
		t.Errorf("got[0] = %+v, want {10:00 alice checkout 500s spiking}", got[0])
	}

	if got[1].Speaker != "bob" || got[1].Timestamp != "" {
		t.Errorf("got[1] = %+v, want speaker=bob, no timestamp", got[1])
	}

	if got[2].Speaker != "unknown" || got[2].Text == "" {
		t.Errorf("got[2] = %+v, want an unattributed line kept, not dropped", got[2])
	}
}

func TestParseTranscriptEmptyInputYieldsNoMessages(t *testing.T) {
	if got := parseTranscript("   \n\n  "); len(got) != 0 {
		t.Errorf("parseTranscript(blank) = %+v, want empty", got)
	}
}

func TestExtractJSONFromFencedResponse(t *testing.T) {
	raw, err := extractJSON("here you go:\n```json\n{\"tldr\": \"outage resolved\"}\n```")
	if err != nil {
		t.Fatalf("extractJSON() error = %v", err)
	}

	if raw["tldr"] != "outage resolved" {
		t.Errorf("extractJSON() = %v", raw)
	}
}
