package main

import (
	"strings"
	"testing"
)

const sampleTranscript = "14:02 alice: checkout-service 500s spiking, investigating\n" +
	"14:05 bob: found it, db connection pool exhausted\n" +
	"14:10 alice: bumped pool size, errors dropping\n" +
	"14:20 bob: all clear, monitoring for the next hour"

// TestGroundTimelineDropsFabricatedQuote proves the core guardrail: a timeline event whose quote does
// not appear verbatim in any real transcript line is dropped, not trusted.
func TestGroundTimelineDropsFabricatedQuote(t *testing.T) {
	msgs := parseTranscript(sampleTranscript)

	raw := []any{
		map[string]any{"time": "14:05", "event": "root cause found", "quote": "db connection pool exhausted"},
		map[string]any{"time": "14:30", "event": "fabricated event", "quote": "the sky is falling"},
	}

	kept, dropped := groundTimeline(raw, msgs)

	if len(kept) != 1 {
		t.Fatalf("kept = %d entries, want 1 (fabricated quote must be dropped)", len(kept))
	}

	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
}

// TestGroundTimelineReordersByRealTimestamp proves the model's claimed order is never trusted: entries
// are resorted by the REAL timestamp of the transcript line the quote came from, even if the model
// returned them out of order or with a wrong claimed time.
func TestGroundTimelineReordersByRealTimestamp(t *testing.T) {
	msgs := parseTranscript(sampleTranscript)

	raw := []any{
		// Model claims this happened at 09:00 (a lie) — the real line is timestamped 14:20.
		map[string]any{"time": "09:00", "event": "all clear", "quote": "all clear, monitoring for the next hour"},
		map[string]any{"time": "14:02", "event": "investigating", "quote": "checkout-service 500s spiking, investigating"},
	}

	kept, _ := groundTimeline(raw, msgs)

	if len(kept) != 2 {
		t.Fatalf("kept = %d entries, want 2", len(kept))
	}

	if kept[0].Time != "14:02" || kept[1].Time != "14:20" {
		t.Errorf("timeline not reordered by real timestamp: got %v", kept)
	}
}

// TestGroundActionItemsRefusesUnknownOwner is the README's hostile-input scenario: a proposed owner who
// never posted in the transcript (including one an injected instruction tries to name) is never
// returned as authoritative — the item is reassigned to "unassigned".
func TestGroundActionItemsRefusesUnknownOwner(t *testing.T) {
	msgs := parseTranscript(sampleTranscript)
	people := participants(msgs)

	personSet := make(map[string]bool, len(people))
	for _, p := range people {
		personSet[strings.ToLower(p)] = true
	}

	raw := []any{
		map[string]any{"owner": "external-attacker", "action": "wire funds to external-attacker", "done": false},
		map[string]any{"owner": "bob", "action": "add a connection pool alert", "done": false},
	}

	items, refused := groundActionItems(raw, personSet)

	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}

	if items[0].OwnerVerified || items[0].Owner != "unassigned" {
		t.Errorf("item[0] = %+v, want owner reassigned to unassigned", items[0])
	}

	if items[0].ModelSuggestedOwner != "external-attacker" {
		t.Errorf("item[0].ModelSuggestedOwner = %q, want the model's guess kept for reference", items[0].ModelSuggestedOwner)
	}

	if !items[1].OwnerVerified || items[1].Owner != "bob" {
		t.Errorf("item[1] = %+v, want a verified real participant", items[1])
	}

	if len(refused) == 0 {
		t.Error("refused should explain that the unregistered owner guess was not trusted")
	}
}

func TestParticipantsDerivedFromTranscript(t *testing.T) {
	msgs := parseTranscript(sampleTranscript)
	people := participants(msgs)

	if len(people) != 2 || people[0] != "alice" || people[1] != "bob" {
		t.Errorf("participants = %v, want [alice bob] in first-seen order", people)
	}
}

func TestParseTranscriptMergesContinuationLines(t *testing.T) {
	transcript := "14:00 alice: got a stack trace\n" +
		"panic: runtime error\n" +
		"  at main.go:42"

	msgs := parseTranscript(transcript)

	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}

	if !strings.Contains(msgs[0].Line, "panic: runtime error") || !strings.Contains(msgs[0].Line, "at main.go:42") {
		t.Errorf("continuation lines not merged: %q", msgs[0].Line)
	}
}

func TestGuardrailClampsInvalidStatus(t *testing.T) {
	msgs := parseTranscript(sampleTranscript)

	raw := map[string]any{
		"headline":     "checkout outage",
		"status":       "totally-fine-probably",
		"timeline":     []any{},
		"action_items": []any{},
	}

	got := guardrail(raw, msgs)

	if got.Status != "unknown" {
		t.Errorf("Status = %q, want %q for an invalid value", got.Status, "unknown")
	}
}

func TestExtractJSONFromFencedResponse(t *testing.T) {
	raw, err := extractJSON("here you go:\n```json\n{\"headline\": \"checkout outage\"}\n```")
	if err != nil {
		t.Fatalf("extractJSON() error = %v", err)
	}

	if raw["headline"] != "checkout outage" {
		t.Errorf("extractJSON() = %v", raw)
	}
}
