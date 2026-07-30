package main

import (
	"context"
	"math"
	"testing"
	"time"
)

// TestHalfLifeMeansHalfLife asserts the property a knob named `halflife` must satisfy: at exactly H
// days, retention is 1/2. Both decay curves were written as exp(-t/H), which has half-life H·ln2 —
// so `halflife_days = 90` really produced a 62.4-day half-life, and the recency curve's documented
// "≈30-day half-life" was 20.8 days. Every such knob was off from its own name by ln2 ≈ 0.693,
// invisibly, because nothing ever asserted the defining property. It does now.
func TestHalfLifeMeansHalfLife(t *testing.T) {
	for _, h := range []float64{21, 30, 90, 365} {
		if got := halfLifeDecay(h, h); math.Abs(got-0.5) > 1e-9 {
			t.Errorf("at t = half-life (%.0fd) retention must be 0.5, got %.4f", h, got)
		}

		if got := halfLifeDecay(2*h, h); math.Abs(got-0.25) > 1e-9 {
			t.Errorf("at t = 2 half-lives (%.0fd) retention must be 0.25, got %.4f", 2*h, got)
		}
	}

	if got := halfLifeDecay(0, 90); got != 1 {
		t.Errorf("a zero-age decision is fully retained, got %.4f", got)
	}

	if got := halfLifeDecay(10, 0); got != 0 {
		t.Errorf("a zero/negative half-life is a misconfiguration and must not score, got %.4f", got)
	}
}

// TestRecencyAndRetentionAgreeOnAge guards the defect where the ranker and the forgetting layer
// decayed the SAME quantity with different curves — 20.8 days against 62.4 — disagreeing 3× on one
// decision in one scoring pass. They may carry different half-lives (they answer different
// questions), but they must be the same FUNCTION, so setting the knobs equal makes them agree.
func TestRecencyAndRetentionAgreeOnAge(t *testing.T) {
	cfg := NewConfig()
	cfg.Set("rank.recency_halflife_days", "90") // match forget.halflife_days' default

	now := time.Now()
	d := Decision{Updated: now.AddDate(0, 0, -90)}

	rec := recencyScore(d.Updated, now, cfg)
	ret := retention(d, Stats{}, now, cfg)

	if math.Abs(rec-ret) > 1e-9 {
		t.Fatalf("same age + same half-life ⇒ same decay; recency=%.6f retention=%.6f", rec, ret)
	}

	if math.Abs(rec-0.5) > 1e-9 {
		t.Fatalf("at exactly one half-life both must be 0.5, got %.6f", rec)
	}
}

func containsID(items []RecalledItem, id string) bool {
	for _, it := range items {
		if it.Decision.ID == id {
			return true
		}
	}

	return false
}

// Retrievability must DECAY with disuse: an old, never-reinforced decision is less retrievable than
// a fresh one (Bjork & Bjork 1992 — retrieval strength falls with disuse).
func TestRetentionDecaysWithDisuse(t *testing.T) {
	cfg := NewConfig()
	now := time.Now()

	fresh := Decision{Updated: now}
	stale := Decision{Updated: now.AddDate(0, 0, -500)} // 5.6 half-lives at H=90 → 2^-5.6 ≈ 0.021

	rFresh := retention(fresh, Stats{}, now, cfg)
	rStale := retention(stale, Stats{}, now, cfg)

	if !(rFresh > rStale) {
		t.Fatalf("fresh (%.3f) must be more retrievable than stale (%.3f)", rFresh, rStale)
	}

	if rStale >= cfg.F("forget.floor", 0.03) {
		t.Fatalf("a 500-day unused decision should fall below the forget floor; got %.3f", rStale)
	}
}

// Use REINFORCES: the same stale decision, but marked helpful, must be MORE retrievable than the
// untouched stale one — and survive above the floor (value-directed remembering / synaptic tagging).
func TestRetentionReinforcedByUse(t *testing.T) {
	cfg := NewConfig()
	now := time.Now()

	stale := Decision{Updated: now.AddDate(0, 0, -400)}

	rCold := retention(stale, Stats{}, now, cfg)
	rWarm := retention(stale, Stats{Helpful: 3, Used: 2}, now, cfg)

	if !(rWarm > rCold) {
		t.Fatalf("reinforcement must raise retrievability: warm %.3f should exceed cold %.3f", rWarm, rCold)
	}

	if rWarm < cfg.F("forget.floor", 0.03) {
		t.Fatalf("a reinforced decision must survive above the forget floor; got %.3f", rWarm)
	}
}

// Negative feedback ERODES retrievability below an equivalent neutral decision.
func TestRetentionErodedByNegative(t *testing.T) {
	cfg := NewConfig()
	now := time.Now()

	d := Decision{Updated: now.AddDate(0, 0, -30)}

	rNeutral := retention(d, Stats{}, now, cfg)
	rBad := retention(d, Stats{Wrong: 3, NotRelevant: 2}, now, cfg)

	if !(rBad < rNeutral) {
		t.Fatalf("negative feedback must lower retrievability: bad %.3f should be under neutral %.3f", rBad, rNeutral)
	}
}

// The end-to-end forgetting guarantee: a decayed decision is DEMOTED out of recall (not surfaced),
// yet REMAINS in the store (Get returns it) — forgetting is demote-not-delete, and it can revive.
func TestForgetFloorDemotesButKeeps(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	// strong lexical overlap so the deterministic test embedder ranks it well above the floor —
	// this isolates the variable under test (retention), not similarity.
	query := []string{"repo:x", "retry backoff exponential jitter"}

	d, err := en.Capture(ctx, Decision{What: "retry backoff exponential jitter", Why: "avoid thundering herd", Scope: []string{"repo:x"}})
	if err != nil {
		t.Fatal(err)
	}

	// FRESH: it must surface (proves demotion later is about retention, not relevance)
	fresh, err := en.Recall(ctx, query)
	if err != nil {
		t.Fatal(err)
	}

	if !containsID(fresh, d.ID) {
		t.Fatalf("precondition: a fresh, relevant decision should surface; got %+v", fresh)
	}

	// age it well past its half-life with no reinforcement → below the forget floor.
	// 500 days at H=90 is 5.6 half-lives (2^-5.6 ≈ 0.021 < the 0.03 floor). 400 days would NOT be
	// enough: 4.4 half-lives ≈ 0.046. It used to be, only because the decay ran ln2 too fast.
	d.Updated = time.Now().AddDate(0, 0, -500)
	en.store.Put(d)

	out, err := en.Recall(ctx, query)
	if err != nil {
		t.Fatal(err)
	}

	if containsID(out, d.ID) {
		t.Fatalf("a decayed decision must be demoted from recall, but it was surfaced")
	}

	// ...yet it is NOT deleted — still fully present in the store (cold storage).
	if _, ok := en.store.Get(d.ID); !ok {
		t.Fatal("forgetting must DEMOTE, never delete — the decision vanished from the store")
	}

	// reinforcing it revives retrievability above the floor (retrieval strength jumps on use)
	en.store.Bump(d.ID, "helpful", 3)

	if r := retention(d, en.store.Stats(d.ID), time.Now(), en.cfg); r < en.cfg.F("forget.floor", 0.03) {
		t.Fatalf("reinforcement should revive a demoted decision above the floor; got %.3f", r)
	}
}

// Consolidate reports hot vs cold honestly and MUTATES NOTHING (introspection only).
func TestConsolidateReportsWithoutMutating(t *testing.T) {
	en := newTestEngine()
	ctx := context.Background()

	keep, _ := en.Capture(ctx, Decision{What: "keep: fresh decision", Scope: []string{"repo:x"}})
	fade, _ := en.Capture(ctx, Decision{What: "fade: old unused decision", Scope: []string{"repo:x"}})

	fade.Updated = time.Now().AddDate(0, 0, -500)
	en.store.Put(fade)

	before := len(en.store.Active())

	rep := en.Consolidate(time.Now(), 10)

	if rep.Total != 2 || rep.Hot < 1 || rep.Cold < 1 {
		t.Fatalf("expected 1 hot + 1 cold of 2; got total=%d hot=%d cold=%d", rep.Total, rep.Hot, rep.Cold)
	}

	if len(rep.Coldest) == 0 || rep.Coldest[0].ID != fade.ID {
		t.Fatalf("the coldest item should be the aged 'fade' decision; got %+v", rep.Coldest)
	}

	if _, ok := en.store.Get(keep.ID); !ok {
		t.Fatal("consolidation must not drop the hot decision")
	}

	if after := len(en.store.Active()); after != before {
		t.Fatalf("Consolidate must not mutate the store: active %d → %d", before, after)
	}
}

// TestNotRelevantDoesNotDecayADecision guards a category error with teeth.
//
// Relevance is a RELATION between a document and an information need, never a property the document
// owns (Saracevic, JASIST 2007). So `not_relevant` — "this was a poor answer to what I asked" — says
// nothing about whether the decision is true, and must never erode its retrievability. `wrong` is the
// opposite: a claim about the decision itself, and the only signal allowed to fade it out of memory.
//
// Before this fix, retention penalised Wrong*2 + NotRelevant, so one bad match for one phrasing
// decayed a correct decision for every future query.
func TestNotRelevantDoesNotDecayADecision(t *testing.T) {
	cfg := NewConfig()
	now := time.Now()
	d := Decision{Updated: now.AddDate(0, 0, -30)}

	clean := retention(d, Stats{}, now, cfg)

	// A pile of "bad match for my question" verdicts must leave the decision exactly as retrievable.
	if got := retention(d, Stats{NotRelevant: 10}, now, cfg); got != clean {
		t.Errorf("not_relevant must not decay a decision: %.4f vs %.4f — a retrieval miss is not evidence the rule is false", got, clean)
	}

	// A "this rule is false" verdict must.
	if got := retention(d, Stats{Wrong: 2}, now, cfg); got >= clean {
		t.Errorf("wrong MUST erode retrievability: %.4f vs %.4f", got, clean)
	}
}
