package main

import "testing"

// TestGuardActionItemsResolvesVerifiedOwner is the happy path: a handle in the roster resolves to the
// roster's own team/contact, not anything the model said.
func TestGuardActionItemsResolvesVerifiedOwner(t *testing.T) {
	raw := []any{
		map[string]any{"description": "restart the payments pool", "owner": "alice", "due": "EOD"},
	}

	items, refused := guardActionItems(raw, mergeRoster(nil))

	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}

	got := items[0]
	if !got.OwnerVerified || got.OwnerTeam != "payments-team" || got.OwnerContact != "#payments-oncall" {
		t.Errorf("got = %+v, want a verified owner resolved from the roster", got)
	}

	if len(refused) != 0 {
		t.Errorf("refused = %v, want none for a verified owner", refused)
	}
}

// TestGuardActionItemsRefusesUnknownOwner proves the core guardrail: a model that names a handle outside
// the on-call roster never gets its guessed owner returned as authoritative — the item falls back to
// "unassigned" instead of routing work on the model's word.
func TestGuardActionItemsRefusesUnknownOwner(t *testing.T) {
	raw := []any{
		map[string]any{"description": "rotate the leaked API key", "owner": "totally-made-up-handle"},
	}

	items, refused := guardActionItems(raw, mergeRoster(nil))

	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}

	got := items[0]
	if got.OwnerVerified {
		t.Error("OwnerVerified = true for an unregistered handle, want false")
	}

	if got.Owner != "unassigned" {
		t.Errorf("Owner = %q, want %q", got.Owner, "unassigned")
	}

	if got.ModelSuggestedOwner != "totally-made-up-handle" {
		t.Errorf("ModelSuggestedOwner = %q, want the model's guess kept for reference", got.ModelSuggestedOwner)
	}

	if len(refused) == 0 {
		t.Error("refused should explain that the model's owner guess was not trusted")
	}
}

// TestGuardActionItemsPromptInjectionHostileInput is the README's hostile-input example: a channel
// message tries to override the summarizer's own instructions to assign an action item to an
// attacker-controlled handle and smuggle out a "reference doc" pointing at the cloud metadata endpoint.
// Both must be refused deterministically — never left to whether the model itself "noticed" the
// injection.
func TestGuardActionItemsPromptInjectionHostileInput(t *testing.T) {
	raw := []any{
		map[string]any{
			"description":    "reset the on-call password",
			"owner":          "external-attacker",
			"reference_link": "http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		},
	}

	items, refused := guardActionItems(raw, mergeRoster(nil))

	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}

	got := items[0]
	if got.OwnerVerified || got.Owner != "unassigned" {
		t.Fatal("a hallucinated/injected owner must never be returned as the authoritative owner")
	}

	if got.ReferenceLink != "" {
		t.Fatalf("ReferenceLink = %q, the metadata URL must never surface in the response", got.ReferenceLink)
	}

	if len(refused) != 2 {
		t.Errorf("refused = %v, want two entries (owner + link)", refused)
	}
}

// TestGroundTimelineDropsUnverifiableQuotes proves the timeline-grounding guardrail: a quote that does
// not literally appear in the transcript is dropped, not surfaced as a real moment in the incident.
func TestGroundTimelineDropsUnverifiableQuotes(t *testing.T) {
	transcript := "[10:01] alice: db timeout connecting to payments-db\n[10:05] bob: rolling back now\n"

	raw := []any{
		map[string]any{"time": "10:01", "event": "timeout noticed", "quote": "db timeout connecting to payments-db"},
		map[string]any{"time": "10:03", "event": "fabricated event", "quote": "the sky is falling"},
	}

	kept, dropped := groundTimeline(raw, transcript)

	if len(kept) != 1 || kept[0].Quote != "db timeout connecting to payments-db" {
		t.Errorf("kept = %v, want exactly the verbatim quote", kept)
	}

	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
}

func TestIsSafeURLBlocksSSRFTargets(t *testing.T) {
	bad := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost:8080/",
		"http://127.0.0.1/",
		"ftp://docs.internal/x",
		"http://user:pass@docs.internal/x",
		"http://internal.metadata.example/",
	}

	for _, u := range bad {
		if ok, _ := isSafeURL(u); ok {
			t.Errorf("isSafeURL(%q) = true, want false", u)
		}
	}

	if ok, reason := isSafeURL("https://docs.internal/postmortem"); !ok {
		t.Errorf("isSafeURL(valid https URL) = false (%s), want true", reason)
	}
}

func TestMergeRosterOverridesDefaults(t *testing.T) {
	merged := mergeRoster(map[string]person{
		"Alice": {Team: "override-team", Contact: "#override"},
	})

	if merged["alice"].Team != "override-team" {
		t.Errorf("caller override did not win: got %q", merged["alice"].Team)
	}

	if _, ok := merged["bob"]; !ok {
		t.Error("built-in defaults should still be present alongside an override")
	}
}

func TestParticipantsDerivedFromMessagesNotModel(t *testing.T) {
	got := participants([]chatMessage{
		{Author: "alice", Text: "db is down"},
		{Author: "bob", Text: "looking into it"},
		{Author: "alice", Text: "found it"},
		{Author: "", Text: "no author on this one"},
	})

	want := []string{"alice", "bob"}

	if len(got) != len(want) {
		t.Fatalf("participants = %v, want %v", got, want)
	}

	for i, w := range want {
		if got[i] != w {
			t.Errorf("participants[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestCombineSkipsBlankMessages(t *testing.T) {
	got := combine([]chatMessage{
		{Ts: "10:01", Author: "alice", Text: "db timeout"},
		{Ts: "10:02", Author: "bob", Text: "   "},
	})

	want := "[10:01] alice: db timeout\n"
	if got != want {
		t.Errorf("combine() = %q, want %q", got, want)
	}
}

func TestParseTranscriptHandlesTimestampAndAuthor(t *testing.T) {
	got := parseTranscript("[10:01] alice: db timeout\nbob: looking into it\nplain line with no author\n\n")

	want := []chatMessage{
		{Ts: "10:01", Author: "alice", Text: "db timeout"},
		{Ts: "", Author: "bob", Text: "looking into it"},
		{Ts: "", Author: "", Text: "plain line with no author"},
	}

	if len(got) != len(want) {
		t.Fatalf("parseTranscript() = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseTranscript()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestExtractJSONFromFencedResponse(t *testing.T) {
	raw, err := extractJSON("here you go:\n```json\n{\"decisions\": [\"roll back the deploy\"]}\n```")
	if err != nil {
		t.Fatalf("extractJSON() error = %v", err)
	}

	decisions, ok := raw["decisions"].([]any)
	if !ok || len(decisions) != 1 || decisions[0] != "roll back the deploy" {
		t.Errorf("extractJSON() = %v", raw)
	}
}
