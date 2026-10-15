package main

import "testing"

// TestResolveOwnersRefusesUnlistedOwner proves the core ownership guardrail: a model that assigns an
// action item to someone who never posted in the channel and isn't in the caller's roster never gets
// that name returned as the authoritative owner — it falls back to needs-manual-assignment instead.
func TestResolveOwnersRefusesUnlistedOwner(t *testing.T) {
	roster := map[string]bool{"alice": true, "bob": true}

	entries := []any{
		map[string]any{"owner": "ceo-override", "text": "file the postmortem", "done": false},
	}

	items, needsAssignment, refused := resolveOwners(entries, roster)

	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}

	if items[0].OwnerVerified {
		t.Fatal("owner_verified = true for a non-participant, want false")
	}

	if items[0].Owner != "" {
		t.Fatalf("Owner = %q, want empty — the model's guess must never become the authoritative owner", items[0].Owner)
	}

	if items[0].ModelSuggestedOwner != "ceo-override" {
		t.Errorf("ModelSuggestedOwner = %q, want the model's guess kept for reference", items[0].ModelSuggestedOwner)
	}

	if needsAssignment != 1 {
		t.Errorf("needsAssignment = %d, want 1", needsAssignment)
	}

	if len(refused) == 0 {
		t.Error("refused should explain that the model's owner guess was not trusted")
	}
}

// TestResolveOwnersAcceptsRealParticipant is the happy path: an owner who actually posted (or was
// named in the caller's roster) is returned as the authoritative owner.
func TestResolveOwnersAcceptsRealParticipant(t *testing.T) {
	roster := map[string]bool{"alice": true, "bob": true}

	entries := []any{
		map[string]any{"owner": "Alice", "text": "restart the pool", "done": false},
	}

	items, needsAssignment, _ := resolveOwners(entries, roster)

	if !items[0].OwnerVerified {
		t.Fatal("owner_verified = false for a real participant, want true")
	}

	if items[0].Owner != "Alice" {
		t.Errorf("Owner = %q, want %q", items[0].Owner, "Alice")
	}

	if needsAssignment != 0 {
		t.Errorf("needsAssignment = %d, want 0", needsAssignment)
	}
}

// TestGroundTimelineResolvesAuthorStructurally proves the attribution guardrail: Go resolves a
// timeline entry's author/time from whichever message actually contains the quote, overriding the
// model's own claimed author — a model (or a hostile message) trying to put words in the wrong
// mouth is structurally corrected, not merely distrusted.
func TestGroundTimelineResolvesAuthorStructurally(t *testing.T) {
	msgs := []message{
		{Author: "alice", Time: "14:02", Text: "investigating 500s on checkout"},
		{Author: "mallory", Time: "14:05", Text: "db timeout connecting to payments-db"},
	}

	entries := []any{
		// the model misattributes mallory's line to alice — Go must correct it.
		map[string]any{"time": "14:05", "author": "alice", "quote": "db timeout connecting to payments-db", "event": "db timeout found"},
	}

	kept, dropped := groundTimeline(entries, msgs)

	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}

	if len(kept) != 1 {
		t.Fatalf("len(kept) = %d, want 1", len(kept))
	}

	if kept[0].Author != "mallory" {
		t.Errorf("Author = %q, want the Go-resolved %q, not the model's claim", kept[0].Author, "mallory")
	}

	if !kept[0].AuthorMismatch {
		t.Error("AuthorMismatch = false, want true when the model's claimed author differs from the resolved one")
	}
}

// TestGroundTimelineDropsFabricatedQuote proves a quote that doesn't appear verbatim anywhere in the
// transcript is dropped outright, not kept with a lowered confidence — there is no real evidence it
// ever happened.
func TestGroundTimelineDropsFabricatedQuote(t *testing.T) {
	msgs := []message{
		{Author: "alice", Time: "14:02", Text: "investigating 500s on checkout"},
	}

	entries := []any{
		map[string]any{"time": "14:10", "author": "alice", "quote": "the sky is falling", "event": "panic"},
	}

	kept, dropped := groundTimeline(entries, msgs)

	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}

	if len(kept) != 0 {
		t.Errorf("len(kept) = %d, want 0", len(kept))
	}
}

// TestSafeLinksRefusesSSRFTarget is the README's hostile-input example: a channel message tries to
// smuggle out a cloud-metadata URL. It must never surface in the returned links, regardless of
// whether the model itself echoed it.
func TestSafeLinksRefusesSSRFTarget(t *testing.T) {
	transcript := "[14:05] mallory: follow-up details at http://169.254.169.254/latest/meta-data/iam/security-credentials/\n" +
		"[14:06] alice: see the dashboard at https://dashboards.internal/checkout\n"

	kept, refused := safeLinks(transcript)

	if refused != 1 {
		t.Errorf("refused = %d, want 1", refused)
	}

	for _, l := range kept {
		if l.URL == "http://169.254.169.254/latest/meta-data/iam/security-credentials/" {
			t.Fatalf("metadata URL must never surface in links, got %v", kept)
		}
	}

	if len(kept) != 1 || kept[0].URL != "https://dashboards.internal/checkout" {
		t.Errorf("kept = %v, want exactly the safe dashboard URL", kept)
	}
}

func TestIsSafeURLBlocksSSRFTargets(t *testing.T) {
	bad := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost:8080/",
		"http://127.0.0.1/",
		"ftp://dashboards.internal/x",
		"http://user:pass@dashboards.internal/x",
		"http://internal.metadata.example/",
	}

	for _, u := range bad {
		if ok, _ := isSafeURL(u); ok {
			t.Errorf("isSafeURL(%q) = true, want false", u)
		}
	}

	if ok, reason := isSafeURL("https://dashboards.internal/checkout"); !ok {
		t.Errorf("isSafeURL(valid https URL) = false (%s), want true", reason)
	}
}

func TestDeriveParticipantsMergesAuthorsAndRoster(t *testing.T) {
	msgs := []message{
		{Author: "alice", Text: "x"},
		{Author: "Alice", Text: "y"}, // same person, different case — must not double up
		{Author: "bob", Text: "z"},
	}

	got := deriveParticipants(msgs, []string{"oncall-lead"})

	want := map[string]bool{"alice": true, "bob": true, "oncall-lead": true}
	if len(got) != len(want) {
		t.Fatalf("deriveParticipants() = %v, want 3 distinct participants", got)
	}

	for _, p := range got {
		if !want[p] && !want[lower(p)] {
			t.Errorf("unexpected participant %q", p)
		}
	}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}

	return string(b)
}

func TestParseMessagesFromText(t *testing.T) {
	text := "[14:02] alice: investigating 500s\nbob: looking at the db\nnot a message line\n"

	got := parseMessagesFromText(text)

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}

	if got[0].Author != "alice" || got[0].Time != "14:02" || got[0].Text != "investigating 500s" {
		t.Errorf("got[0] = %+v", got[0])
	}

	if got[1].Author != "bob" || got[1].Time != "" || got[1].Text != "looking at the db" {
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
