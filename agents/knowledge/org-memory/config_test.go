package main

import "testing"

// TestConfigChangesAreAudited: Gate #0 requires every knob change to be "validated, versioned, and
// audited (who/what/when — same append-only discipline as the memory itself)". Validation existed;
// the audit trail did not. Config IS behaviour here — one write to retrieve.precision_floor can
// silence recall completely — so an unrecorded change is an unexplainable outage.
func TestConfigChangesAreAudited(t *testing.T) {
	c := NewConfig()

	if got := c.History(0); len(got) != 0 {
		t.Fatalf("a fresh config has no history, got %d", len(got))
	}

	c.SetBy("retrieve.precision_floor", "0.65", "alice")
	c.SetBy("retrieve.precision_floor", "0.99", "bob")
	c.SetBy("rank.w_authority", "0", "alice", "team:platform")

	hist := c.History(0)
	if len(hist) != 3 {
		t.Fatalf("expected 3 audited changes, got %d", len(hist))
	}

	// The replaced value must be captured, or the record cannot answer "put it back".
	if hist[0].From != "" || hist[0].To != "0.65" {
		t.Errorf(`first write should record from="" (was on its default) to=0.65, got %q→%q`, hist[0].From, hist[0].To)
	}

	if hist[1].From != "0.65" || hist[1].To != "0.99" {
		t.Errorf("second write should record the value it replaced, got %q→%q", hist[1].From, hist[1].To)
	}

	if hist[1].By != "bob" {
		t.Errorf("actor must be attributed, got %q", hist[1].By)
	}

	if hist[2].Scope != "team:platform" {
		t.Errorf("scope must be recorded (a team knob is not an org knob), got %q", hist[2].Scope)
	}

	if hist[0].At.IsZero() {
		t.Error("every change needs a timestamp")
	}

	// Newest last, and a limit returns the most RECENT entries, never a stale prefix.
	if tail := c.History(1); len(tail) != 1 || tail[0].Key != "rank.w_authority" {
		t.Errorf("History(1) must return the most recent change, got %+v", tail)
	}
}

func TestConfigDefaultsAndLiveSet(t *testing.T) {
	c := NewConfig()

	if c.F("retrieve.precision_floor", 0.3) != 0.3 {
		t.Fatal("missing knob must return the default (zero-config runs)")
	}

	c.Set("retrieve.precision_floor", "0.5")

	if c.F("retrieve.precision_floor", 0.3) != 0.5 {
		t.Fatal("set knob must override the default, live")
	}

	c.Set("retrieve.top_k", "7")

	if c.I("retrieve.top_k", 3) != 7 {
		t.Fatal("int knob")
	}

	c.Set("capture.mode", "strict")

	if c.Str("capture.mode", "loose") != "strict" {
		t.Fatal("string knob")
	}
}

func TestConfigFractalScope(t *testing.T) {
	c := NewConfig()
	c.Set("retrieve.top_k", "3")               // org
	c.Set("retrieve.top_k", "5", "team:pay")   // team override
	c.Set("retrieve.top_k", "9", "person:ana") // person override

	if got := c.I("retrieve.top_k", 0, "person:ana", "team:pay"); got != 9 {
		t.Fatalf("person scope should win, got %d", got)
	}

	if got := c.I("retrieve.top_k", 0, "person:bob", "team:pay"); got != 5 {
		t.Fatalf("team scope should win when no person override, got %d", got)
	}

	if got := c.I("retrieve.top_k", 0, "person:bob", "team:other"); got != 3 {
		t.Fatalf("should fall back to org, got %d", got)
	}
}
