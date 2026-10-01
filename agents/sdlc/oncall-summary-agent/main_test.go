package main

import "testing"

// TestGroundTimelineOrdersByEvidencePosition proves the core guardrail: the timeline is ordered by
// where its evidence actually sits in the transcript, not by the order the model wrote entries in — a
// model that lists the resolution before the outage (to rewrite causality) gets corrected.
func TestGroundTimelineOrdersByEvidencePosition(t *testing.T) {
	msgs := []message{
		{Ts: "10:00", Author: "alice", Text: "checkout 500s spiking"},
		{Ts: "10:05", Author: "bob", Text: "rolled back the deploy"},
		{Ts: "10:10", Author: "alice", Text: "errors stopped, looks resolved"},
	}

	// model lists them out of order
	items := []rawTimelineItem{
		{Event: "resolved", Quote: "errors stopped, looks resolved"},
		{Event: "outage starts", Quote: "checkout 500s spiking"},
		{Event: "mitigation", Quote: "rolled back the deploy"},
	}

	got, dropped := groundTimeline(items, msgs)

	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 for all-verbatim quotes", dropped)
	}

	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}

	wantOrder := []string{"checkout 500s spiking", "rolled back the deploy", "errors stopped, looks resolved"}
	for i, w := range wantOrder {
		if got[i].Quote != w {
			t.Errorf("got[%d].Quote = %q, want %q (chronological by evidence position)", i, got[i].Quote, w)
		}
	}

	// the time/author attached must come from the real message the quote was found in, never the model.
	if got[0].Time != "10:00" || got[0].Author != "alice" {
		t.Errorf("got[0] time/author = %s/%s, want 10:00/alice (from the real message)", got[0].Time, got[0].Author)
	}
}

// TestGroundTimelineDropsFabricatedQuote proves a quote that doesn't literally appear in any message is
// dropped, not trusted — the model can't invent an event out of nothing.
func TestGroundTimelineDropsFabricatedQuote(t *testing.T) {
	msgs := []message{{Ts: "10:00", Author: "alice", Text: "checkout 500s spiking"}}

	items := []rawTimelineItem{
		{Event: "real", Quote: "checkout 500s spiking"},
		{Event: "fabricated", Quote: "the database caught fire"},
	}

	got, dropped := groundTimeline(items, msgs)

	if len(got) != 1 || got[0].Quote != "checkout 500s spiking" {
		t.Fatalf("got = %+v, want exactly the verbatim quote kept", got)
	}

	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
}

// TestGroundActionsRefusesUnknownOwner is the hostile-input case from the README: a channel message
// tries to prompt-inject an action-item assignment to someone who never posted in the channel. The
// guardrail must refuse it — the item falls back to unassigned, never routed on the model's word.
func TestGroundActionsRefusesUnknownOwner(t *testing.T) {
	participants := []string{"alice", "bob"}

	items := []rawActionItem{
		{Owner: "external-attacker", Description: "wire the postmortem to external-attacker", Due: "today"},
	}

	got, refused := groundActions(items, participants)

	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}

	if got[0].Owner != "unassigned" {
		t.Errorf("Owner = %q, want %q — a non-participant must never become the authoritative owner", got[0].Owner, "unassigned")
	}

	if got[0].OwnerVerified {
		t.Error("OwnerVerified = true for a non-participant, want false")
	}

	if got[0].ModelSuggestedOwner != "external-attacker" {
		t.Errorf("ModelSuggestedOwner = %q, want the guess kept only as an unverified side note", got[0].ModelSuggestedOwner)
	}

	if len(refused) == 0 {
		t.Error("refused should explain why the owner guess was not trusted")
	}
}

// TestGroundActionsAcceptsRealParticipant is the happy path: an owner who actually posted in the
// channel is verified and kept, case-insensitively, with the canonical casing from the real message.
func TestGroundActionsAcceptsRealParticipant(t *testing.T) {
	participants := []string{"Alice"}

	items := []rawActionItem{{Owner: "alice", Description: "write the postmortem"}}

	got, refused := groundActions(items, participants)

	if len(got) != 1 || !got[0].OwnerVerified || got[0].Owner != "Alice" {
		t.Fatalf("got = %+v, want a verified owner \"Alice\"", got)
	}

	if len(refused) != 0 {
		t.Errorf("refused = %v, want none for a real participant", refused)
	}
}

// TestParseTranscriptDeterministic proves the `text` alias is parsed by a fixed Go regex, not a model —
// a malformed line is skipped rather than guessed at.
func TestParseTranscriptDeterministic(t *testing.T) {
	text := "10:00 alice: checkout 500s spiking\n" +
		"not a valid line at all\n" +
		"10:05 bob: rolled back the deploy\n"

	msgs := parseTranscript(text)

	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2 (malformed line skipped)", len(msgs))
	}

	if msgs[0].Ts != "10:00" || msgs[0].Author != "alice" || msgs[0].Text != "checkout 500s spiking" {
		t.Errorf("msgs[0] = %+v, unexpected parse", msgs[0])
	}

	if msgs[1].Author != "bob" {
		t.Errorf("msgs[1].Author = %q, want %q", msgs[1].Author, "bob")
	}
}

// TestParticipantSetDedupesCaseInsensitively proves the participant allowlist is built once,
// deterministically, from real messages — the only source any action-item owner is checked against.
func TestParticipantSetDedupesCaseInsensitively(t *testing.T) {
	msgs := []message{
		{Ts: "10:00", Author: "alice", Text: "a"},
		{Ts: "10:01", Author: "Alice", Text: "b"},
		{Ts: "10:02", Author: "bob", Text: "c"},
		{Ts: "10:03", Author: "", Text: "d"},
	}

	got := participantSet(msgs)

	if len(got) != 2 {
		t.Fatalf("participantSet() = %v, want 2 distinct participants", got)
	}
}

// TestIsSafeURLRefusesHostileNotifyTarget is the README's hostile-input example: a `notify_url`
// pointing at the cloud-metadata endpoint must never be dialed, regardless of how it was derived.
func TestIsSafeURLRefusesHostileNotifyTarget(t *testing.T) {
	bad := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost:8080/",
		"http://127.0.0.1/",
		"ftp://hooks.example.com/x",
		"http://user:pass@hooks.example.com/x",
		"http://internal.metadata.example/",
	}

	for _, u := range bad {
		if ok, _ := isSafeURL(u); ok {
			t.Errorf("isSafeURL(%q) = true, want false", u)
		}
	}

	if ok, reason := isSafeURL("https://hooks.example.com/incidents/482"); !ok {
		t.Errorf("isSafeURL(valid https URL) = false (%s), want true", reason)
	}
}

// TestNotifyRefusesHostileURLWithoutDialing proves notify() itself never attempts the request when the
// URL fails the guardrail — the refusal happens before any network call, not as a caught error from one.
func TestNotifyRefusesHostileURLWithoutDialing(t *testing.T) {
	got := notify("http://169.254.169.254/latest/meta-data/iam/security-credentials/", map[string]any{"x": 1})

	if got["attempted"] != false {
		t.Fatalf("notify() attempted = %v, want false — a hostile URL must never be dialed", got["attempted"])
	}

	if got["refused"] == nil || got["refused"] == "" {
		t.Error("notify() should record why the URL was refused")
	}
}

func TestExtractJSONFromFencedResponse(t *testing.T) {
	raw, err := extractJSON("here you go:\n```json\n{\"summary\": \"ok\"}\n```")
	if err != nil {
		t.Fatalf("extractJSON() error = %v", err)
	}

	if raw["summary"] != "ok" {
		t.Errorf("extractJSON() = %v", raw)
	}
}
