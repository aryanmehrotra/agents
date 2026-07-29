package main

import "testing"

func cand(id, what string, sim float64, spec int, st Stats, quarantined bool) scored {
	return scored{d: Decision{ID: id, What: what, Quarantined: quarantined}, sim: sim, spec: spec, st: st}
}

func TestRankPrecisionFloorInjectsNothing(t *testing.T) {
	c := NewConfig()
	c.Set("retrieve.precision_floor", "0.5")

	cands := []scored{
		cand("d1", "a", 0.2, 0, Stats{}, false),
		cand("d2", "b", 0.4, 0, Stats{}, false),
	}

	if out := rankAndFilter(cands, c); len(out) != 0 {
		t.Fatalf("nothing clears the floor → inject NOTHING (empty is correct), got %d", len(out))
	}
}

func TestRankOrdersByRelevanceAndCapsTopK(t *testing.T) {
	c := NewConfig()
	c.Set("retrieve.precision_floor", "0.1")
	c.Set("retrieve.top_k", "2")
	c.Set("rank.w_recency", "0") // isolate relevance
	c.Set("rank.w_importance", "0")

	cands := []scored{
		cand("low", "a", 0.3, 0, Stats{}, false),
		cand("high", "b", 0.9, 0, Stats{}, false),
		cand("mid", "c", 0.6, 0, Stats{}, false),
	}

	out := rankAndFilter(cands, c)
	if len(out) != 2 {
		t.Fatalf("top_k cap should be 2, got %d", len(out))
	}

	if out[0].Decision.ID != "high" || out[1].Decision.ID != "mid" {
		t.Fatalf("expected order high, mid (most relevant first); got %s, %s",
			out[0].Decision.ID, out[1].Decision.ID)
	}

	if out[0].Guidance == "" {
		t.Fatal("recalled item should carry a rendered guidance line")
	}
}

func TestRankExcludesQuarantined(t *testing.T) {
	c := NewConfig()
	c.Set("retrieve.precision_floor", "0.1")

	cands := []scored{
		cand("q", "a", 0.9, 0, Stats{}, true), // highest relevance but quarantined
		cand("ok", "b", 0.5, 0, Stats{}, false),
	}

	out := rankAndFilter(cands, c)
	if len(out) != 1 || out[0].Decision.ID != "ok" {
		t.Fatalf("quarantined must be excluded even if most relevant, got %+v", out)
	}
}

func TestRankFeedbackNudge(t *testing.T) {
	c := NewConfig()
	c.Set("retrieve.precision_floor", "0.1")
	c.Set("rank.w_recency", "0")
	c.Set("rank.w_importance", "0")
	c.Set("feedback.demote_per_notrel", "0.5")

	// identical relevance; "bad" has 2 not_relevant marks → should fall below "good".
	cands := []scored{
		cand("bad", "a", 0.6, 0, Stats{NotRelevant: 2}, false),
		cand("good", "b", 0.6, 0, Stats{Helpful: 1}, false),
	}

	out := rankAndFilter(cands, c)
	if len(out) != 2 || out[0].Decision.ID != "good" {
		t.Fatalf("feedback should demote repeatedly-not-relevant below helpful; got %+v", out)
	}
}
