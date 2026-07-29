package main

import "testing"

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
