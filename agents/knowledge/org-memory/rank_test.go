package main

import (
	"math"
	"testing"
)

func cand(id, what string, sim float64, spec int, st Stats, quarantined bool) scored {
	return scored{d: Decision{ID: id, What: what, Quarantined: quarantined}, sim: sim, spec: spec, st: st}
}

// ranked drops the effective-floor return so ordering assertions stay readable.
func ranked(cands []scored, cfg *Config, chain ...string) []RecalledItem {
	out, _, _ := rankAndFilter(cands, cfg, chain...)
	return out
}

// authCand is a candidate carrying an author scope, so authority weighting has something to read.
func authCand(id string, sim float64, author string) scored {
	d := Decision{ID: id, What: id}
	if author != "" {
		d.Scope = []string{"author:" + author}
	}

	return scored{d: d, sim: sim}
}

// authorityOnly isolates the authority signal: every other prior weight is zeroed, so priorNorm is
// exactly the author's role weight and score−sim is exactly prior_cap × that weight.
func authorityOnly() *Config {
	c := NewConfig()
	c.Set("retrieve.precision_floor", "0.1")
	c.Set("rank.w_recency", "0")
	c.Set("rank.w_importance", "0")
	c.Set("rank.w_retention", "0")
	c.Set("rank.w_feedback", "0")
	c.Set("rank.w_authority", "1")
	c.Set("role.boss", "cto")
	c.Set("authority.cto", "1.0")
	c.Set("authority.sde2", "0.3") // the default role for an unknown login

	return c
}

// TestRankPriorIsNotConstant is the regression guard for the inert-prior defect: the prior used to be
// a weighted SUM clamped at prior_cap, and since recency+importance+retention alone always exceed the
// cap it saturated for every candidate — score−similarity was +prior_cap for everyone, so the priors
// carried no ordering information at all. A constant offset also makes every "does authority evict?"
// or "does w_authority change anything?" check vacuously pass. The prior must DIFFERENTIATE.
func TestRankPriorIsNotConstant(t *testing.T) {
	c := authorityOnly()

	out := ranked([]scored{
		authCand("cto-authored", 0.60, "boss"),
		authCand("unknown-authored", 0.60, "nobody"),
		authCand("unattributed", 0.60, ""),
	}, c)

	if len(out) != 3 {
		t.Fatalf("expected 3 results, got %d", len(out))
	}

	deltas := map[string]float64{}
	for _, it := range out {
		deltas[it.Decision.ID] = it.Score - it.Similarity
	}

	if deltas["cto-authored"] <= deltas["unknown-authored"] {
		t.Fatalf("a CTO-authored decision must earn a larger prior than an unknown author's: cto=%.4f unknown=%.4f",
			deltas["cto-authored"], deltas["unknown-authored"])
	}

	if deltas["unknown-authored"] <= deltas["unattributed"] {
		t.Fatalf("an attributed decision must earn a larger prior than an unattributed one: unknown=%.4f none=%.4f",
			deltas["unknown-authored"], deltas["unattributed"])
	}
}

// TestRankPriorStaysBounded is the other half of the invariant: differentiating is only safe if the
// prior can still never bury a materially-more-relevant answer. The positive prior must stay within
// prior_cap, so a cosine gap wider than the cap is decisive no matter who wrote the weaker match.
func TestRankPriorStaysBounded(t *testing.T) {
	c := authorityOnly()
	c.Set("rank.prior_cap", "0.01")

	for _, tc := range []struct {
		name       string
		betterSim  float64
		wantWinner string
	}{
		{"gap wider than the cap → relevance wins", 0.62, "more-relevant"},
		{"gap inside the cap → authority breaks the near-tie", 0.6005, "cto-authored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := ranked([]scored{
				authCand("cto-authored", 0.60, "boss"),
				authCand("more-relevant", tc.betterSim, ""),
			}, c)

			if out[0].Decision.ID != tc.wantWinner {
				t.Fatalf("expected %q first, got %q (scores %.4f / %.4f)",
					tc.wantWinner, out[0].Decision.ID, out[0].Score, out[1].Score)
			}

			for _, it := range out {
				if d := it.Score - it.Similarity; d < 0 || d > 0.01+1e-9 {
					t.Fatalf("positive prior must stay within prior_cap, got %+.4f for %s", d, it.Decision.ID)
				}
			}
		})
	}
}

// TestRankAuthorityToggleChangesOrdering proves the w_authority knob is a real toggle. Under the old
// saturating clamp this check could not fail — a uniform offset cannot reorder anything — which is
// exactly why the pre-fix "ordering identical at w_authority 0 vs 0.15" result proved nothing.
func TestRankAuthorityToggleChangesOrdering(t *testing.T) {
	cands := []scored{
		authCand("cto-authored", 0.6000, "boss"),
		authCand("unattributed", 0.6005, ""),
	}

	on := authorityOnly()

	off := authorityOnly()
	off.Set("rank.w_authority", "0")

	withAuth := ranked(cands, on)
	noAuth := ranked(cands, off)

	if withAuth[0].Decision.ID != "cto-authored" {
		t.Fatalf("with authority on, the CTO's near-tie should win; got %q", withAuth[0].Decision.ID)
	}

	if noAuth[0].Decision.ID != "unattributed" {
		t.Fatalf("with authority off, pure cosine should win; got %q", noAuth[0].Decision.ID)
	}
}

// TestRankPositiveFeedbackSaturates: feedback is a relevance nudge, never a popularity ladder — the
// 100th "helpful" must not buy a materially-less-relevant decision to the top.
func TestRankPositiveFeedbackSaturates(t *testing.T) {
	c := NewConfig()
	c.Set("retrieve.precision_floor", "0.1")
	c.Set("rank.w_recency", "0")
	c.Set("rank.w_importance", "0")
	c.Set("rank.w_retention", "0")
	c.Set("rank.w_authority", "0")
	c.Set("rank.w_feedback", "1")

	out := ranked([]scored{
		cand("adored", "a", 0.60, 0, Stats{Helpful: 500}, false),
		cand("plain", "b", 0.62, 0, Stats{}, false),
	}, c)

	if out[0].Decision.ID != "plain" {
		t.Fatalf("500 helpful marks must not outweigh a 0.02 cosine gap; got %q first", out[0].Decision.ID)
	}

	if d := out[0].Score - out[0].Similarity; math.Abs(d) > 0.01+1e-9 {
		t.Fatalf("prior escaped its bound: %+.4f", d)
	}
}

func TestRankPrecisionFloorInjectsNothing(t *testing.T) {
	c := NewConfig()
	c.Set("retrieve.precision_floor", "0.5")

	cands := []scored{
		cand("d1", "a", 0.2, 0, Stats{}, false),
		cand("d2", "b", 0.4, 0, Stats{}, false),
	}

	if out := ranked(cands, c); len(out) != 0 {
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

	out := ranked(cands, c)
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

	out := ranked(cands, c)
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

	out := ranked(cands, c)
	if len(out) != 2 || out[0].Decision.ID != "good" {
		t.Fatalf("feedback should demote repeatedly-not-relevant below helpful; got %+v", out)
	}
}
